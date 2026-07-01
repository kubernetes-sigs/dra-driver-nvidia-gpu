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

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

const (
	DaemonSetResourceClaimTemplatePath = "/templates/compute-domain-daemon-resource-claim.tmpl.yaml"
)

type DaemonSetResourceClaimManager struct {
	config           *ManagerConfig
	waitGroup        sync.WaitGroup
	cancelContext    context.CancelFunc
	getComputeDomain GetComputeDomainFunc

	informer      cache.SharedIndexInformer
	mutationCache cache.MutationCache

	cleanupManager *CleanupManager[*resourceapi.ResourceClaim]
}

func NewDaemonSetResourceClaimManager(config *ManagerConfig, getComputeDomain GetComputeDomainFunc) *DaemonSetResourceClaimManager {
	labelSelector := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      computeDomainLabelKey,
				Operator: metav1.LabelSelectorOpExists,
			},
			{
				Key:      computeDomainResourceClaimTemplateTargetLabelKey,
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{computeDomainResourceClaimTemplateTargetDaemon},
			},
		},
	}

	tweakListOptions := func(opts *metav1.ListOptions) {
		opts.LabelSelector = metav1.FormatLabelSelector(labelSelector)
	}

	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
				tweakListOptions(&options)
				return config.clientsets.Resource.ResourceClaims(config.driverNamespace).List(ctx, options)
			},
			WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
				tweakListOptions(&options)
				return config.clientsets.Resource.ResourceClaims(config.driverNamespace).Watch(ctx, options)
			},
		},
		&resourceapi.ResourceClaim{},
		informerResyncPeriod,
		cache.Indexers{},
	)

	m := &DaemonSetResourceClaimManager{
		config:           config,
		getComputeDomain: getComputeDomain,
		informer:         informer,
	}
	m.cleanupManager = NewCleanupManager[*resourceapi.ResourceClaim](informer, getComputeDomain, m.cleanup)

	return m
}

func (m *DaemonSetResourceClaimManager) Start(ctx context.Context) (rerr error) {
	ctx, cancel := context.WithCancel(ctx)
	m.cancelContext = cancel

	defer func() {
		if rerr != nil {
			if err := m.Stop(); err != nil {
				klog.Errorf("error stopping DaemonSetResourceClaimManager: %v", err)
			}
		}
	}()

	if err := addComputeDomainLabelIndexer[*resourceapi.ResourceClaim](m.informer); err != nil {
		return fmt.Errorf("error adding indexer for ComputeDomain label: %w", err)
	}

	m.mutationCache = cache.NewIntegerResourceVersionMutationCache(
		klog.Background(),
		m.informer.GetStore(),
		m.informer.GetIndexer(),
		mutationCacheTTL,
		true,
	)

	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		m.informer.Run(ctx.Done())
	}()

	if !cache.WaitForCacheSync(ctx.Done(), m.informer.HasSynced) {
		return fmt.Errorf("informer cache sync for ResourceClaim failed")
	}

	if err := m.cleanupManager.Start(ctx); err != nil {
		return fmt.Errorf("error starting cleanup manager: %w", err)
	}

	return nil
}

func (m *DaemonSetResourceClaimManager) Stop() error {
	if m.cancelContext != nil {
		m.cancelContext()
	}
	m.waitGroup.Wait()
	return nil
}

func (m *DaemonSetResourceClaimManager) Create(ctx context.Context, cd *nvapi.ComputeDomain) (string, error) {
	rcs, err := getByComputeDomainUID[*resourceapi.ResourceClaim](ctx, m.mutationCache, string(cd.UID))
	if err != nil {
		return "", fmt.Errorf("error retrieving ResourceClaim: %w", err)
	}
	if len(rcs) > 1 {
		return "", fmt.Errorf("more than one ResourceClaim found with same ComputeDomain UID")
	}
	if len(rcs) == 1 {
		return rcs[0].Name, nil
	}

	daemonConfig := nvapi.DefaultComputeDomainDaemonConfig()
	daemonConfig.DomainID = string(cd.UID)

	templateData := ResourceClaimTemplateTemplateData{
		Namespace:               m.config.driverNamespace,
		Name:                    fmt.Sprintf("computedomain-daemon-%s", cd.UID),
		Finalizer:               computeDomainFinalizer,
		ComputeDomainLabelKey:   computeDomainLabelKey,
		ComputeDomainLabelValue: cd.UID,
		TargetLabelKey:          computeDomainResourceClaimTemplateTargetLabelKey,
		TargetLabelValue:        computeDomainResourceClaimTemplateTargetDaemon,
		DeviceClassName:         computeDomainDaemonDeviceClass,
		DriverName:              DriverName,
		DaemonConfig:            daemonConfig,
	}

	tmpl, err := template.ParseFiles(DaemonSetResourceClaimTemplatePath)
	if err != nil {
		return "", fmt.Errorf("failed to parse template file: %w", err)
	}

	var resourceClaimYaml bytes.Buffer
	if err := tmpl.Execute(&resourceClaimYaml, templateData); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	var unstructuredObj unstructured.Unstructured
	if err := yaml.Unmarshal(resourceClaimYaml.Bytes(), &unstructuredObj); err != nil {
		return "", fmt.Errorf("failed to unmarshal yaml: %w", err)
	}

	var resourceClaim resourceapi.ResourceClaim
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.UnstructuredContent(), &resourceClaim); err != nil {
		return "", fmt.Errorf("failed to convert unstructured data to typed object: %w", err)
	}

	rc, err := m.config.clientsets.Resource.ResourceClaims(resourceClaim.Namespace).Create(ctx, &resourceClaim, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("error creating ResourceClaim: %w", err)
	}

	m.mutationCache.Mutation(rc)

	return rc.Name, nil
}

func (m *DaemonSetResourceClaimManager) Delete(ctx context.Context, cdUID string) error {
	rcs, err := getByComputeDomainUID[*resourceapi.ResourceClaim](ctx, m.mutationCache, cdUID)
	if err != nil {
		return fmt.Errorf("error retrieving ResourceClaim: %w", err)
	}
	if len(rcs) > 1 {
		return fmt.Errorf("more than one ResourceClaim found with same ComputeDomain UID")
	}
	if len(rcs) == 0 {
		return nil
	}

	rc := rcs[0]

	if rc.GetDeletionTimestamp() != nil {
		return nil
	}

	err = m.config.clientsets.Resource.ResourceClaims(rc.Namespace).Delete(ctx, rc.Name, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("erroring deleting ResourceClaim: %w", err)
	}

	return nil
}

func (m *DaemonSetResourceClaimManager) RemoveFinalizer(ctx context.Context, cdUID string) error {
	rcs, err := getByComputeDomainUID[*resourceapi.ResourceClaim](ctx, m.mutationCache, cdUID)
	if err != nil {
		return fmt.Errorf("error retrieving ResourceClaim: %w", err)
	}
	if len(rcs) > 1 {
		return fmt.Errorf("more than one ResourceClaim found with same ComputeDomain UID")
	}
	if len(rcs) == 0 {
		return nil
	}

	rc := rcs[0]

	if rc.GetDeletionTimestamp() == nil {
		return fmt.Errorf("attempting to remove finalizer before ResourceClaim marked for deletion")
	}

	newRC := rc.DeepCopy()
	newRC.Finalizers = []string{}
	for _, f := range rc.Finalizers {
		if f != computeDomainFinalizer {
			newRC.Finalizers = append(newRC.Finalizers, f)
		}
	}
	if len(rc.Finalizers) == len(newRC.Finalizers) {
		return nil
	}

	if _, err := m.config.clientsets.Resource.ResourceClaims(rc.Namespace).Update(ctx, newRC, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("error updating ResourceClaim: %w", err)
	}

	m.mutationCache.Mutation(newRC)

	return nil
}

func (m *DaemonSetResourceClaimManager) AssertRemoved(ctx context.Context, cdUID string) error {
	rcs, err := getByComputeDomainUID[*resourceapi.ResourceClaim](ctx, m.informer.GetIndexer(), cdUID)
	if err != nil {
		return fmt.Errorf("error retrieving ResourceClaim: %w", err)
	}
	if len(rcs) != 0 {
		return fmt.Errorf("still exists")
	}
	return nil
}

func (m *DaemonSetResourceClaimManager) cleanup(ctx context.Context, cdUID string) error {
	if err := m.Delete(ctx, cdUID); err != nil {
		return fmt.Errorf("error deleting ResourceClaim: %w", err)
	}
	if err := m.RemoveFinalizer(ctx, cdUID); err != nil {
		return fmt.Errorf("error removing ResourceClaim finalizer: %w", err)
	}
	return nil
}
