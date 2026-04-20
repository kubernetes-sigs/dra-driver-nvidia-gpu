/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"text/template"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta2"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/apis/computedomain/numnodes"
)

const (
	DaemonSetTemplatePath = "/templates/compute-domain-daemon.tmpl.yaml"
)

type DaemonSetTemplateData struct {
	Namespace                 string
	Name                      string
	Finalizer                 string
	ComputeDomainLabelKey     string
	ComputeDomainLabelValue   types.UID
	ResourceClaimTemplateName string
	ImageName                 string
	MaxNodesPerIMEXDomain     int
	FeatureGates              map[string]bool
	LogVerbosity              int
	ImagePullSecretNames      []string
}

type DaemonSetManager struct {
	sync.Mutex

	config           *ManagerConfig
	waitGroup        sync.WaitGroup
	cancelContext    context.CancelFunc
	getComputeDomain GetComputeDomainFunc

	factory       informers.SharedInformerFactory
	informer      cache.SharedIndexInformer
	mutationCache cache.MutationCache

	resourceClaimTemplateManager *DaemonSetResourceClaimTemplateManager
	cdStatusManager              *ComputeDomainStatusManager
	cleanupManager               *CleanupManager[*appsv1.DaemonSet]
}

func NewDaemonSetManager(config *ManagerConfig, getComputeDomain GetComputeDomainFunc, listComputeDomains ListComputeDomainsFunc, updateComputeDomainStatus UpdateComputeDomainStatusFunc) *DaemonSetManager {
	labelSelector := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      computeDomainLabelKey,
				Operator: metav1.LabelSelectorOpExists,
			},
		},
	}

	factory := informers.NewSharedInformerFactoryWithOptions(
		config.clientsets.Core,
		informerResyncPeriod,
		informers.WithNamespace(config.driverNamespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = metav1.FormatLabelSelector(labelSelector)
		}),
	)

	informer := factory.Apps().V1().DaemonSets().Informer()

	m := &DaemonSetManager{
		config:           config,
		getComputeDomain: getComputeDomain,
		factory:          factory,
		informer:         informer,
	}
	m.resourceClaimTemplateManager = NewDaemonSetResourceClaimTemplateManager(config, getComputeDomain)

	// Create ComputeDomainStatusManager to sync node info to CD status
	// - When feature gate ON: syncs from CDCliques + non-fabric-attached pods
	// - When feature gate OFF: syncs from non-fabric-attached pods + handles deletions
	m.cdStatusManager = NewComputeDomainStatusManager(config, listComputeDomains, updateComputeDomainStatus)

	m.cleanupManager = NewCleanupManager[*appsv1.DaemonSet](informer, getComputeDomain, m.cleanup)

	return m
}

func (m *DaemonSetManager) Start(ctx context.Context) (rerr error) {
	ctx, cancel := context.WithCancel(ctx)
	m.cancelContext = cancel

	defer func() {
		if rerr != nil {
			if err := m.Stop(); err != nil {
				klog.Errorf("error stopping DaemonSet manager: %v", err)
			}
		}
	}()

	if err := addComputeDomainLabelIndexer[*appsv1.DaemonSet](m.informer); err != nil {
		return fmt.Errorf("error adding indexer for MultiNodeEnvironment label: %w", err)
	}

	m.mutationCache = cache.NewIntegerResourceVersionMutationCache(
		klog.Background(),
		m.informer.GetStore(),
		m.informer.GetIndexer(),
		mutationCacheTTL,
		true,
	)

	_, err := m.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			m.config.workQueue.Enqueue(obj, m.onAddOrUpdate)
		},
		UpdateFunc: func(objOld, objNew any) {
			m.config.workQueue.Enqueue(objNew, m.onAddOrUpdate)
		},
	})
	if err != nil {
		return fmt.Errorf("error adding event handlers for DaemonSet informer: %w", err)
	}

	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		m.factory.Start(ctx.Done())
	}()

	if !cache.WaitForCacheSync(ctx.Done(), m.informer.HasSynced) {
		return fmt.Errorf("informer cache sync for DaemonSet failed")
	}

	if err := m.resourceClaimTemplateManager.Start(ctx); err != nil {
		return fmt.Errorf("error starting ResourceClaimTemplate manager: %w", err)
	}

	if err := m.cdStatusManager.Start(ctx); err != nil {
		return fmt.Errorf("error starting ComputeDomain status manager: %w", err)
	}

	if err := m.cleanupManager.Start(ctx); err != nil {
		return fmt.Errorf("error starting cleanup manager: %w", err)
	}

	return nil
}

func (m *DaemonSetManager) Stop() error {
	if err := m.cdStatusManager.Stop(); err != nil {
		klog.Errorf("error stopping ComputeDomain status manager: %v", err)
	}
	if err := m.resourceClaimTemplateManager.Stop(); err != nil {
		return fmt.Errorf("error stopping ResourceClaimTemplate manager: %w", err)
	}
	if m.cancelContext != nil {
		m.cancelContext()
	}
	m.waitGroup.Wait()
	return nil
}

func (m *DaemonSetManager) Create(ctx context.Context, cd *nvapi.ComputeDomain) (*appsv1.DaemonSet, error) {
	ds, err := getByComputeDomainUID[*appsv1.DaemonSet](ctx, m.mutationCache, string(cd.UID))
	if err != nil {
		return nil, fmt.Errorf("error retrieving DaemonSet: %w", err)
	}
	if len(ds) > 1 {
		return nil, fmt.Errorf("more than one DaemonSet found with same ComputeDomain UID")
	}
	if len(ds) == 1 {
		return ds[0], nil
	}

	rct, err := m.resourceClaimTemplateManager.Create(ctx, cd)
	if err != nil {
		return nil, fmt.Errorf("error creating ResourceClaimTemplate: %w", err)
	}

	templateData := DaemonSetTemplateData{
		Namespace:                 m.config.driverNamespace,
		Name:                      fmt.Sprintf("computedomain-daemon-%s", cd.UID),
		Finalizer:                 computeDomainFinalizer,
		ComputeDomainLabelKey:     computeDomainLabelKey,
		ComputeDomainLabelValue:   cd.UID,
		ResourceClaimTemplateName: rct.Name,
		ImageName:                 m.config.imageName,
		MaxNodesPerIMEXDomain:     m.config.maxNodesPerIMEXDomain,
		FeatureGates:              featuregates.ToMap(),
		LogVerbosity:              m.config.logVerbosityCDDaemon,
		ImagePullSecretNames:      m.config.imagePullSecretNames,
	}

	tmpl, err := template.ParseFiles(DaemonSetTemplatePath)
	if err != nil {
		return nil, fmt.Errorf("failed to parse template file: %w", err)
	}

	var daemonSetYaml bytes.Buffer
	if err := tmpl.Execute(&daemonSetYaml, templateData); err != nil {
		return nil, fmt.Errorf("failed to execute template: %w", err)
	}

	var unstructuredObj unstructured.Unstructured
	err = yaml.Unmarshal(daemonSetYaml.Bytes(), &unstructuredObj)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal yaml: %w", err)
	}

	var daemonSet appsv1.DaemonSet
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.UnstructuredContent(), &daemonSet)
	if err != nil {
		return nil, fmt.Errorf("failed to convert unstructured data to typed object: %w", err)
	}

	d, err := m.config.clientsets.Core.AppsV1().DaemonSets(daemonSet.Namespace).Create(ctx, &daemonSet, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("error creating DaemonSet: %w", err)
	}

	// Add the newly created DaemonSet to the mutation cache
	// This ensures subsequent calls will see it immediately
	m.mutationCache.Mutation(d)

	return d, nil
}

func (m *DaemonSetManager) Get(ctx context.Context, cdUID string) (*appsv1.DaemonSet, error) {
	ds, err := getByComputeDomainUID[*appsv1.DaemonSet](ctx, m.mutationCache, cdUID)
	if err != nil {
		return nil, fmt.Errorf("error retrieving DaemonSet: %w", err)
	}
	if len(ds) > 1 {
		return nil, fmt.Errorf("more than one DaemonSet found with same ComputeDomain UID")
	}
	if len(ds) == 0 {
		return nil, nil
	}
	return ds[0], nil
}

func (m *DaemonSetManager) Delete(ctx context.Context, cdUID string) error {
	ds, err := getByComputeDomainUID[*appsv1.DaemonSet](ctx, m.mutationCache, cdUID)
	if err != nil {
		return fmt.Errorf("error retrieving DaemonSet: %w", err)
	}
	if len(ds) > 1 {
		return fmt.Errorf("more than one DaemonSet found with same ComputeDomain UID")
	}
	if len(ds) == 0 {
		return nil
	}

	d := ds[0]

	if err := m.resourceClaimTemplateManager.Delete(ctx, cdUID); err != nil {
		return fmt.Errorf("error deleting ResourceClaimTemplate: %w", err)
	}

	if d.GetDeletionTimestamp() != nil {
		return nil
	}

	err = m.config.clientsets.Core.AppsV1().DaemonSets(d.Namespace).Delete(ctx, d.Name, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("erroring deleting DaemonSet: %w", err)
	}

	return nil
}

func (m *DaemonSetManager) RemoveFinalizer(ctx context.Context, cdUID string) error {
	if err := m.resourceClaimTemplateManager.RemoveFinalizer(ctx, cdUID); err != nil {
		return fmt.Errorf("error removing finalizer on ResourceClaimTemplate: %w", err)
	}
	if err := m.removeFinalizer(ctx, cdUID); err != nil {
		return fmt.Errorf("error removing finalizer on DaemonSet: %w", err)
	}
	return nil
}

func (m *DaemonSetManager) AssertRemoved(ctx context.Context, cdUID string) error {
	if err := m.resourceClaimTemplateManager.AssertRemoved(ctx, cdUID); err != nil {
		return fmt.Errorf("error asserting ResourceClaimTemplate removed: %w", err)
	}
	if err := m.assertRemoved(ctx, cdUID); err != nil {
		return fmt.Errorf("error asserting DaemonSet removal: %w", err)
	}
	return nil
}

func (m *DaemonSetManager) removeFinalizer(ctx context.Context, cdUID string) error {
	ds, err := getByComputeDomainUID[*appsv1.DaemonSet](ctx, m.mutationCache, cdUID)
	if err != nil {
		return fmt.Errorf("error retrieving DaemonSet: %w", err)
	}
	if len(ds) > 1 {
		return fmt.Errorf("more than one DaemonSet found with same ComputeDomain UID")
	}
	if len(ds) == 0 {
		return nil
	}

	d := ds[0]

	if d.GetDeletionTimestamp() == nil {
		return fmt.Errorf("attempting to remove finalizer before DaemonSet marked for deletion")
	}

	newD := d.DeepCopy()
	newD.Finalizers = []string{}
	for _, f := range d.Finalizers {
		if f != computeDomainFinalizer {
			newD.Finalizers = append(newD.Finalizers, f)
		}
	}
	if len(d.Finalizers) == len(newD.Finalizers) {
		return nil
	}

	if _, err := m.config.clientsets.Core.AppsV1().DaemonSets(d.Namespace).Update(ctx, newD, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("error updating DaemonSet: %w", err)
	}

	// Update mutation cache after successful update
	m.mutationCache.Mutation(newD)

	return nil
}

func (m *DaemonSetManager) assertRemoved(ctx context.Context, cdUID string) error {
	ds, err := getByComputeDomainUID[*appsv1.DaemonSet](ctx, m.informer.GetIndexer(), cdUID)
	if err != nil {
		return fmt.Errorf("error retrieving DaemonSet: %w", err)
	}
	if len(ds) != 0 {
		return fmt.Errorf("still exists")
	}
	return nil
}

func (m *DaemonSetManager) onAddOrUpdate(ctx context.Context, obj any) error {
	d, ok := obj.(*appsv1.DaemonSet)
	if !ok {
		return fmt.Errorf("failed to cast to DaemonSet")
	}

	klog.V(2).Infof("Processing added or updated DaemonSet: %s/%s", d.Namespace, d.Name)

	cd, err := m.getComputeDomain(d.Labels[computeDomainLabelKey])
	if err != nil {
		return fmt.Errorf("error getting ComputeDomain: %w", err)
	}
	if cd == nil {
		return nil
	}

	expected := numnodes.FromObject(&cd.ObjectMeta)
	if int(d.Status.NumberReady) != expected {
		return nil
	}

	newCD := cd.DeepCopy()
	newCD.Status.Status = nvapi.ComputeDomainStatusReady
	if _, err = m.config.clientsets.Nvidia.ResourceV1beta2().ComputeDomains(newCD.Namespace).UpdateStatus(ctx, newCD, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("error updating nodes in ComputeDomain status: %w", err)
	}

	return nil
}

func (m *DaemonSetManager) cleanup(ctx context.Context, cdUID string) error {
	if err := m.Delete(ctx, cdUID); err != nil {
		return fmt.Errorf("error deleting DaemonSet: %w", err)
	}
	if err := m.RemoveFinalizer(ctx, cdUID); err != nil {
		return fmt.Errorf("error removing DaemonSet finalizer: %w", err)
	}
	return nil
}
