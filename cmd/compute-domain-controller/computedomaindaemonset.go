/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

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
	"encoding/json"
	"fmt"
	"hash/fnv"
	"slices"
	"strconv"
	"sync"
	"text/template"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	nvinformers "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/informers/externalversions"
)

const (
	StaticDaemonSetTemplatePath = "/templates/compute-domain-static-daemon.tmpl.yaml"

	// computeDomainDaemonSetLabelKey labels the objects of a
	// ComputeDomainDaemonSet with its UID.
	computeDomainDaemonSetLabelKey  = "resource.nvidia.com/computeDomainDaemonSet"
	computeDomainDaemonSetFinalizer = "resource.nvidia.com/computeDomainDaemonSet"
	// staticDaemonTemplateHashKey annotates the static IMEX DaemonSet and its
	// pods with the hash of the pod template they run.
	staticDaemonTemplateHashKey = "resource.nvidia.com/templateHash"
	gpuCliqueNodeLabelKey       = "nvidia.com/gpu.clique"

	// cddsSyncDelay batches the events of thousands of daemons into one sync.
	cddsSyncDelay = 500 * time.Millisecond
	// cddsResyncInterval rechecks the cliques and rollout without events.
	cddsResyncInterval = time.Minute
)

// StaticDaemonSetTemplateData renders the static IMEX DaemonSet.
type StaticDaemonSetTemplateData struct {
	Namespace                 string
	Name                      string
	DaemonSetLabelKey         string
	DaemonSetLabelValue       types.UID
	CliqueNodeLabelKey        string
	ResourceClaimTemplateName string
	ImageName                 string
	MaxNodesPerIMEXDomain     int
	IMEXConfigOverrides       map[string]string
	LogVerbosity              int
}

// ComputeDomainDaemonSetManager runs the static IMEX daemons of the
// ComputeDomainDaemonSet: one DaemonSet in the driver namespace, its daemon
// ResourceClaimTemplate, and the ComputeDomainCliques its daemons register
// in. It rolls out spec changes by clique and reports the daemons in status.
//
// All events funnel into one queue key, so that a sync sees a consistent
// view and bursts from thousands of daemons collapse into few syncs.
type ComputeDomainDaemonSetManager struct {
	config *ManagerConfig
	// hasChannelClaims reports whether any claim is allocated an IMEX
	// channel, which holds back deletion.
	hasChannelClaims func() bool

	waitGroup     sync.WaitGroup
	cancelContext context.CancelFunc

	cddsFactory   nvinformers.SharedInformerFactory
	cddsInformer  cache.SharedIndexInformer
	cliqueFactory nvinformers.SharedInformerFactory
	cliqueInf     cache.SharedIndexInformer
	driverFactory informers.SharedInformerFactory
	daemonSetInf  cache.SharedIndexInformer
	podInf        cache.SharedIndexInformer
	rctInf        cache.SharedIndexInformer
	nodeFactory   informers.SharedInformerFactory
	nodeInf       cache.SharedIndexInformer

	queue workqueue.TypedRateLimitingInterface[string]

	daemonSetTemplatePath string
	rctTemplatePath       string
}

func NewComputeDomainDaemonSetManager(config *ManagerConfig, hasChannelClaims func() bool) *ComputeDomainDaemonSetManager {
	cddsFactory := nvinformers.NewSharedInformerFactory(config.clientsets.Nvidia, informerResyncPeriod)
	ownedSelector := func(opts *metav1.ListOptions) {
		opts.LabelSelector = computeDomainDaemonSetLabelKey
	}
	cliqueFactory := nvinformers.NewSharedInformerFactoryWithOptions(config.clientsets.Nvidia, informerResyncPeriod,
		nvinformers.WithNamespace(config.driverNamespace), nvinformers.WithTweakListOptions(ownedSelector))
	driverFactory := informers.NewSharedInformerFactoryWithOptions(config.clientsets.Core, informerResyncPeriod,
		informers.WithNamespace(config.driverNamespace), informers.WithTweakListOptions(ownedSelector))
	nodeFactory := informers.NewSharedInformerFactoryWithOptions(config.clientsets.Core, informerResyncPeriod,
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = gpuCliqueNodeLabelKey
		}))

	return &ComputeDomainDaemonSetManager{
		config:                config,
		hasChannelClaims:      hasChannelClaims,
		cddsFactory:           cddsFactory,
		cddsInformer:          cddsFactory.Resource().V1beta1().ComputeDomainDaemonSets().Informer(),
		cliqueFactory:         cliqueFactory,
		cliqueInf:             cliqueFactory.Resource().V1beta1().ComputeDomainCliques().Informer(),
		driverFactory:         driverFactory,
		daemonSetInf:          driverFactory.Apps().V1().DaemonSets().Informer(),
		podInf:                driverFactory.Core().V1().Pods().Informer(),
		rctInf:                driverFactory.Resource().V1().ResourceClaimTemplates().Informer(),
		nodeFactory:           nodeFactory,
		nodeInf:               nodeFactory.Core().V1().Nodes().Informer(),
		daemonSetTemplatePath: StaticDaemonSetTemplatePath,
		rctTemplatePath:       DaemonSetResourceClaimTemplateTemplatePath,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Second, time.Minute),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "compute-domain-daemonset"},
		),
	}
}

func (m *ComputeDomainDaemonSetManager) Start(ctx context.Context) (rerr error) {
	ctx, cancel := context.WithCancel(ctx)
	m.cancelContext = cancel

	defer func() {
		if rerr != nil {
			if err := m.Stop(); err != nil {
				klog.Errorf("error stopping ComputeDomainDaemonSet manager: %v", err)
			}
		}
	}()

	enqueue := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { m.queue.AddAfter(nvapi.ComputeDomainDaemonSetName, cddsSyncDelay) },
		UpdateFunc: func(any, any) { m.queue.AddAfter(nvapi.ComputeDomainDaemonSetName, cddsSyncDelay) },
		DeleteFunc: func(any) { m.queue.AddAfter(nvapi.ComputeDomainDaemonSetName, cddsSyncDelay) },
	}
	all := []cache.SharedIndexInformer{m.cddsInformer, m.cliqueInf, m.daemonSetInf, m.podInf, m.rctInf, m.nodeInf}
	synced := make([]cache.InformerSynced, 0, len(all))
	for _, informer := range all {
		if _, err := informer.AddEventHandler(enqueue); err != nil {
			return fmt.Errorf("error adding event handler: %w", err)
		}
		synced = append(synced, informer.HasSynced)
	}

	for _, factory := range []interface{ Start(<-chan struct{}) }{m.cddsFactory, m.cliqueFactory, m.driverFactory, m.nodeFactory} {
		m.waitGroup.Go(func() { factory.Start(ctx.Done()) })
	}
	if !cache.WaitForCacheSync(ctx.Done(), synced...) {
		return fmt.Errorf("informer cache sync for ComputeDomainDaemonSet failed")
	}

	m.waitGroup.Go(func() {
		for m.processNextItem(ctx) {
		}
	})
	m.waitGroup.Go(func() {
		ticker := time.NewTicker(cddsResyncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.queue.Add(nvapi.ComputeDomainDaemonSetName)
			}
		}
	})
	m.queue.Add(nvapi.ComputeDomainDaemonSetName)
	return nil
}

func (m *ComputeDomainDaemonSetManager) Stop() error {
	m.queue.ShutDown()
	if m.cancelContext != nil {
		m.cancelContext()
	}
	m.waitGroup.Wait()
	return nil
}

func (m *ComputeDomainDaemonSetManager) processNextItem(ctx context.Context) bool {
	key, shutdown := m.queue.Get()
	if shutdown {
		return false
	}
	defer m.queue.Done(key)

	requeueAfter, err := m.sync(ctx)
	if err != nil {
		klog.Errorf("ComputeDomainDaemonSet sync failed (attempt %d): %v", m.queue.NumRequeues(key), err)
		m.queue.AddRateLimited(key)
		return true
	}
	m.queue.Forget(key)
	if requeueAfter > 0 {
		m.queue.AddAfter(key, requeueAfter)
	}
	return true
}

// get returns the ComputeDomainDaemonSet, or nil.
func (m *ComputeDomainDaemonSetManager) get() (*nvapi.ComputeDomainDaemonSet, error) {
	obj, exists, err := m.cddsInformer.GetStore().GetByKey(nvapi.ComputeDomainDaemonSetName)
	if err != nil || !exists {
		return nil, err
	}
	cdds, ok := obj.(*nvapi.ComputeDomainDaemonSet)
	if !ok {
		return nil, fmt.Errorf("unexpected object %T in ComputeDomainDaemonSet informer", obj)
	}
	return cdds, nil
}

func staticDaemonSetName(uid types.UID) string {
	return "computedomain-daemonset-" + string(uid)
}

func (m *ComputeDomainDaemonSetManager) sync(ctx context.Context) (time.Duration, error) {
	cdds, err := m.get()
	if err != nil {
		return 0, err
	}
	uid := types.UID("")
	if cdds != nil {
		uid = cdds.UID
	}
	// Objects of a ComputeDomainDaemonSet that is gone, also one whose
	// finalizer was removed by hand.
	if err := m.deleteObjects(ctx, func(owner string) bool { return owner != string(uid) }); err != nil {
		return 0, err
	}
	if cdds == nil {
		return 0, nil
	}
	if cdds.GetDeletionTimestamp() != nil {
		return m.delete(ctx, cdds)
	}

	if cdds, err = m.addFinalizer(ctx, cdds); err != nil {
		return 0, err
	}
	rct, err := m.ensureResourceClaimTemplate(ctx, cdds)
	if err != nil {
		return 0, err
	}
	ds, err := m.ensureDaemonSet(ctx, cdds, rct.Name)
	if err != nil {
		return 0, err
	}

	pods := m.pods(cdds.UID)
	if err := m.removeStaleDaemons(ctx, cdds, pods); err != nil {
		return 0, err
	}

	hash := ds.Spec.Template.Annotations[staticDaemonTemplateHashKey]
	cliques, _ := groupByClique(m.nodes(), pods, cdds.Spec.NodeSelector)
	if err := m.updateStatus(ctx, cdds, ds, cliques, pods, hash); err != nil {
		return 0, err
	}
	return 0, nil
}

func (m *ComputeDomainDaemonSetManager) addFinalizer(ctx context.Context, cdds *nvapi.ComputeDomainDaemonSet) (*nvapi.ComputeDomainDaemonSet, error) {
	if slices.Contains(cdds.Finalizers, computeDomainDaemonSetFinalizer) {
		return cdds, nil
	}
	newCDDS := cdds.DeepCopy()
	newCDDS.Finalizers = append(newCDDS.Finalizers, computeDomainDaemonSetFinalizer)
	updated, err := m.config.clientsets.Nvidia.ResourceV1beta1().ComputeDomainDaemonSets().Update(ctx, newCDDS, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("error adding finalizer to ComputeDomainDaemonSet: %w", err)
	}
	return updated, nil
}

// delete removes the static IMEX daemons once no workload uses an IMEX
// channel, as removing them breaks the IMEX domains of running workloads.
func (m *ComputeDomainDaemonSetManager) delete(ctx context.Context, cdds *nvapi.ComputeDomainDaemonSet) (time.Duration, error) {
	if m.hasChannelClaims() {
		newCDDS := cdds.DeepCopy()
		meta.SetStatusCondition(&newCDDS.Status.Conditions, metav1.Condition{
			Type:               nvapi.ComputeDomainDaemonSetConditionDeletionBlocked,
			Status:             metav1.ConditionTrue,
			Reason:             "ChannelClaimsExist",
			Message:            "waiting for all ResourceClaims allocated an IMEX channel to be deleted",
			ObservedGeneration: cdds.Generation,
		})
		if err := m.writeStatus(ctx, cdds, newCDDS); err != nil {
			return 0, err
		}
		return cddsResyncInterval, nil
	}

	if err := m.deleteObjects(ctx, func(owner string) bool { return owner == string(cdds.UID) }); err != nil {
		return 0, err
	}
	if _, exists, _ := m.daemonSetInf.GetStore().GetByKey(m.config.driverNamespace + "/" + staticDaemonSetName(cdds.UID)); exists {
		return 0, fmt.Errorf("waiting for DaemonSet %s to be deleted", staticDaemonSetName(cdds.UID))
	}

	newCDDS := cdds.DeepCopy()
	newCDDS.Finalizers = slices.DeleteFunc(newCDDS.Finalizers, func(f string) bool { return f == computeDomainDaemonSetFinalizer })
	if _, err := m.config.clientsets.Nvidia.ResourceV1beta1().ComputeDomainDaemonSets().Update(ctx, newCDDS, metav1.UpdateOptions{}); err != nil && !errors.IsNotFound(err) {
		return 0, fmt.Errorf("error removing finalizer from ComputeDomainDaemonSet: %w", err)
	}
	klog.Infof("Deleted the static IMEX daemons of ComputeDomainDaemonSet %s", cdds.UID)
	return 0, nil
}

// deleteObjects deletes the DaemonSets, ResourceClaimTemplates and
// ComputeDomainCliques of the ComputeDomainDaemonSet UIDs that match.
func (m *ComputeDomainDaemonSetManager) deleteObjects(ctx context.Context, matches func(owner string) bool) error {
	for _, obj := range m.daemonSetInf.GetStore().List() {
		ds, ok := obj.(*appsv1.DaemonSet)
		if !ok || !matches(ds.Labels[computeDomainDaemonSetLabelKey]) || ds.DeletionTimestamp != nil {
			continue
		}
		err := m.config.clientsets.Core.AppsV1().DaemonSets(ds.Namespace).Delete(ctx, ds.Name, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("error deleting DaemonSet %s: %w", ds.Name, err)
		}
		klog.Infof("Deleted static IMEX DaemonSet %s/%s", ds.Namespace, ds.Name)
	}
	for _, obj := range m.rctInf.GetStore().List() {
		rct, ok := obj.(*resourceapi.ResourceClaimTemplate)
		if !ok || !matches(rct.Labels[computeDomainDaemonSetLabelKey]) || rct.DeletionTimestamp != nil {
			continue
		}
		err := m.config.clientsets.Core.ResourceV1().ResourceClaimTemplates(rct.Namespace).Delete(ctx, rct.Name, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("error deleting ResourceClaimTemplate %s: %w", rct.Name, err)
		}
	}
	for _, obj := range m.cliqueInf.GetStore().List() {
		clique, ok := obj.(*nvapi.ComputeDomainClique)
		if !ok || !matches(clique.Labels[computeDomainDaemonSetLabelKey]) || clique.DeletionTimestamp != nil {
			continue
		}
		err := m.config.clientsets.Nvidia.ResourceV1beta1().ComputeDomainCliques(clique.Namespace).Delete(ctx, clique.Name, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("error deleting ComputeDomainClique %s: %w", clique.Name, err)
		}
	}
	return nil
}

func (m *ComputeDomainDaemonSetManager) ensureResourceClaimTemplate(ctx context.Context, cdds *nvapi.ComputeDomainDaemonSet) (*resourceapi.ResourceClaimTemplate, error) {
	name := staticDaemonSetName(cdds.UID)
	if obj, exists, err := m.rctInf.GetStore().GetByKey(m.config.driverNamespace + "/" + name); err == nil && exists {
		if rct, ok := obj.(*resourceapi.ResourceClaimTemplate); ok {
			return rct, nil
		}
	}

	daemonConfig := nvapi.DefaultComputeDomainDaemonConfig()
	daemonConfig.DomainID = string(cdds.UID)
	rct, err := renderResourceClaimTemplate(m.rctTemplatePath, &ResourceClaimTemplateTemplateData{
		Namespace:               m.config.driverNamespace,
		Name:                    name,
		Finalizer:               computeDomainDaemonSetFinalizer,
		ComputeDomainLabelKey:   computeDomainDaemonSetLabelKey,
		ComputeDomainLabelValue: cdds.UID,
		TargetLabelKey:          computeDomainResourceClaimTemplateTargetLabelKey,
		TargetLabelValue:        computeDomainResourceClaimTemplateTargetDaemon,
		DeviceClassName:         computeDomainDaemonDeviceClass,
		DriverName:              DriverName,
		DaemonConfig:            daemonConfig,
	})
	if err != nil {
		return nil, err
	}
	// Deleted with the ComputeDomainDaemonSet, without a finalizer of its own.
	rct.Finalizers = nil

	created, err := m.config.clientsets.Core.ResourceV1().ResourceClaimTemplates(rct.Namespace).Create(ctx, rct, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return rct, nil
	}
	if err != nil {
		return nil, fmt.Errorf("error creating ResourceClaimTemplate: %w", err)
	}
	return created, nil
}

// renderDaemonSet renders the static IMEX DaemonSet, annotated with the hash
// of its pod template. The image is spec.image, else the one the DaemonSet
// runs, else the driver image, so that driver upgrades do not change it.
func (m *ComputeDomainDaemonSetManager) renderDaemonSet(cdds *nvapi.ComputeDomainDaemonSet, rctName string, existing *appsv1.DaemonSet) (*appsv1.DaemonSet, error) {
	image := cdds.Spec.Image
	if image == "" && existing != nil && len(existing.Spec.Template.Spec.Containers) > 0 {
		image = existing.Spec.Template.Spec.Containers[0].Image
	}
	if image == "" {
		image = m.config.imageName
	}

	tmpl, err := template.ParseFiles(m.daemonSetTemplatePath)
	if err != nil {
		return nil, fmt.Errorf("failed to parse template file: %w", err)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, StaticDaemonSetTemplateData{
		Namespace:                 m.config.driverNamespace,
		Name:                      staticDaemonSetName(cdds.UID),
		DaemonSetLabelKey:         computeDomainDaemonSetLabelKey,
		DaemonSetLabelValue:       cdds.UID,
		CliqueNodeLabelKey:        gpuCliqueNodeLabelKey,
		ResourceClaimTemplateName: rctName,
		ImageName:                 image,
		MaxNodesPerIMEXDomain:     cdds.Spec.MaxNodesPerClique,
		IMEXConfigOverrides:       cdds.Spec.IMEXConfig,
		LogVerbosity:              cdds.Spec.LogVerbosity,
	}); err != nil {
		return nil, fmt.Errorf("failed to execute template: %w", err)
	}
	var obj unstructured.Unstructured
	if err := yaml.Unmarshal(rendered.Bytes(), &obj); err != nil {
		return nil, fmt.Errorf("failed to unmarshal yaml: %w", err)
	}
	var ds appsv1.DaemonSet
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.UnstructuredContent(), &ds); err != nil {
		return nil, fmt.Errorf("failed to convert unstructured data to typed object: %w", err)
	}

	podSpec := &ds.Spec.Template.Spec
	podSpec.NodeSelector = cdds.Spec.NodeSelector
	podSpec.PriorityClassName = cdds.Spec.PriorityClassName
	podSpec.ImagePullSecrets = cdds.Spec.ImagePullSecrets
	if len(podSpec.ImagePullSecrets) == 0 {
		for _, name := range m.config.imagePullSecretNames {
			podSpec.ImagePullSecrets = append(podSpec.ImagePullSecrets, corev1.LocalObjectReference{Name: name})
		}
	}
	podSpec.Containers[0].Resources.Requests = cdds.Spec.Resources.Requests
	podSpec.Containers[0].Resources.Limits = cdds.Spec.Resources.Limits

	hash, err := podTemplateHash(&ds.Spec.Template)
	if err != nil {
		return nil, err
	}
	if ds.Spec.Template.Annotations == nil {
		ds.Spec.Template.Annotations = map[string]string{}
	}
	ds.Spec.Template.Annotations[staticDaemonTemplateHashKey] = hash
	return &ds, nil
}

func podTemplateHash(template *corev1.PodTemplateSpec) (string, error) {
	data, err := json.Marshal(template)
	if err != nil {
		return "", fmt.Errorf("error hashing pod template: %w", err)
	}
	h := fnv.New64a()
	_, _ = h.Write(data)
	return strconv.FormatUint(h.Sum64(), 36), nil
}

// ensureDaemonSet creates the static IMEX DaemonSet, or updates its pod
// template. Its OnDelete strategy leaves restarting daemons to the rollout.
func (m *ComputeDomainDaemonSetManager) ensureDaemonSet(ctx context.Context, cdds *nvapi.ComputeDomainDaemonSet, rctName string) (*appsv1.DaemonSet, error) {
	var existing *appsv1.DaemonSet
	if obj, exists, err := m.daemonSetInf.GetStore().GetByKey(m.config.driverNamespace + "/" + staticDaemonSetName(cdds.UID)); err == nil && exists {
		existing, _ = obj.(*appsv1.DaemonSet)
	}
	desired, err := m.renderDaemonSet(cdds, rctName, existing)
	if err != nil {
		return nil, err
	}

	if existing == nil {
		created, err := m.config.clientsets.Core.AppsV1().DaemonSets(desired.Namespace).Create(ctx, desired, metav1.CreateOptions{})
		if errors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("waiting for DaemonSet %s in the informer", desired.Name)
		}
		if err != nil {
			return nil, fmt.Errorf("error creating DaemonSet: %w", err)
		}
		klog.Infof("Created static IMEX DaemonSet %s/%s", created.Namespace, created.Name)
		return created, nil
	}

	if existing.Spec.Template.Annotations[staticDaemonTemplateHashKey] == desired.Spec.Template.Annotations[staticDaemonTemplateHashKey] {
		return existing, nil
	}
	updated := existing.DeepCopy()
	updated.Spec.Template = desired.Spec.Template
	updated.Spec.UpdateStrategy = desired.Spec.UpdateStrategy
	result, err := m.config.clientsets.Core.AppsV1().DaemonSets(updated.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("error updating DaemonSet: %w", err)
	}
	klog.Infof("Updated the pod template of static IMEX DaemonSet %s/%s", result.Namespace, result.Name)
	return result, nil
}

func (m *ComputeDomainDaemonSetManager) pods(uid types.UID) []*corev1.Pod {
	var pods []*corev1.Pod
	for _, obj := range m.podInf.GetStore().List() {
		if pod, ok := obj.(*corev1.Pod); ok && pod.Labels[computeDomainDaemonSetLabelKey] == string(uid) {
			pods = append(pods, pod)
		}
	}
	return pods
}

func (m *ComputeDomainDaemonSetManager) nodes() []*corev1.Node {
	var nodes []*corev1.Node
	for _, obj := range m.nodeInf.GetStore().List() {
		if node, ok := obj.(*corev1.Node); ok {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

// removeStaleDaemons removes clique entries of daemons whose pod is gone
// without unregistering, for example with its node. A miss in the informer
// is confirmed against the API server first.
func (m *ComputeDomainDaemonSetManager) removeStaleDaemons(ctx context.Context, cdds *nvapi.ComputeDomainDaemonSet, pods []*corev1.Pod) error {
	running := make(map[string]bool, len(pods))
	for _, pod := range pods {
		running[pod.Spec.NodeName] = true
	}
	var live map[string]bool
	for _, obj := range m.cliqueInf.GetStore().List() {
		clique, ok := obj.(*nvapi.ComputeDomainClique)
		if !ok || clique.Labels[computeDomainDaemonSetLabelKey] != string(cdds.UID) {
			continue
		}
		var keep []*nvapi.ComputeDomainDaemonInfo
		for _, daemon := range clique.Daemons {
			if running[daemon.NodeName] {
				keep = append(keep, daemon)
				continue
			}
			if live == nil {
				list, err := m.config.clientsets.Core.CoreV1().Pods(m.config.driverNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: fmt.Sprintf("%s=%s", computeDomainDaemonSetLabelKey, cdds.UID),
				})
				if err != nil {
					return fmt.Errorf("error listing static IMEX daemon pods: %w", err)
				}
				live = make(map[string]bool, len(list.Items))
				for i := range list.Items {
					live[list.Items[i].Spec.NodeName] = true
				}
			}
			if live[daemon.NodeName] {
				keep = append(keep, daemon)
			}
		}
		if len(keep) == len(clique.Daemons) {
			continue
		}
		newClique := clique.DeepCopy()
		newClique.Daemons = keep
		if _, err := m.config.clientsets.Nvidia.ResourceV1beta1().ComputeDomainCliques(clique.Namespace).Update(ctx, newClique, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("error removing stale daemons from ComputeDomainClique %s: %w", clique.Name, err)
		}
		klog.Infof("Removed %d stale daemons from ComputeDomainClique %s", len(clique.Daemons)-len(keep), clique.Name)
	}
	return nil
}

func (m *ComputeDomainDaemonSetManager) updateStatus(ctx context.Context, cdds *nvapi.ComputeDomainDaemonSet, ds *appsv1.DaemonSet, cliques []*staticClique, pods []*corev1.Pod, hash string) error {
	newCDDS := cdds.DeepCopy()
	status := &newCDDS.Status
	status.ObservedGeneration = cdds.Generation
	status.Image = ds.Spec.Template.Spec.Containers[0].Image
	status.DesiredNumberScheduled = ds.Status.DesiredNumberScheduled
	status.NumberReady = ds.Status.NumberReady
	status.UpdatedNumberScheduled = 0
	for _, pod := range pods {
		if podUpdated(pod, hash) && pod.DeletionTimestamp == nil {
			status.UpdatedNumberScheduled++
		}
	}
	status.Cliques = cliqueStatuses(cliques, hash)
	status.NumberCliques = int32(len(status.Cliques))

	ready := metav1.Condition{Type: nvapi.ComputeDomainDaemonSetConditionReady, Status: metav1.ConditionFalse, Reason: "DaemonsNotReady",
		Message: fmt.Sprintf("%d of %d IMEX daemons are ready", status.NumberReady, status.DesiredNumberScheduled)}
	if status.DesiredNumberScheduled > 0 && status.NumberReady == status.DesiredNumberScheduled {
		ready.Status, ready.Reason = metav1.ConditionTrue, "DaemonsReady"
	}
	rollout := metav1.Condition{Type: nvapi.ComputeDomainDaemonSetConditionRolloutInProgress, Status: metav1.ConditionFalse, Reason: "Updated",
		Message: "all IMEX daemons run the current revision"}
	if outdated := status.DesiredNumberScheduled - status.UpdatedNumberScheduled; outdated > 0 {
		rollout.Status = metav1.ConditionTrue
		switch {
		default:
			rollout.Reason, rollout.Message = "WaitingForPodDeletion", fmt.Sprintf("%d IMEX daemons run an outdated revision, delete their pods to update them", outdated)
		}
	}
	for _, c := range []metav1.Condition{ready, rollout} {
		c.ObservedGeneration = cdds.Generation
		meta.SetStatusCondition(&status.Conditions, c)
	}
	meta.RemoveStatusCondition(&status.Conditions, nvapi.ComputeDomainDaemonSetConditionDeletionBlocked)
	return m.writeStatus(ctx, cdds, newCDDS)
}

func (m *ComputeDomainDaemonSetManager) writeStatus(ctx context.Context, old, cdds *nvapi.ComputeDomainDaemonSet) error {
	if equality.Semantic.DeepEqual(old.Status, cdds.Status) {
		return nil
	}
	if _, err := m.config.clientsets.Nvidia.ResourceV1beta1().ComputeDomainDaemonSets().UpdateStatus(ctx, cdds, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("error updating ComputeDomainDaemonSet status: %w", err)
	}
	return nil
}
