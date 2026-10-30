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
	"maps"
	"slices"
	"strconv"
	"strings"
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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
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
	// staticDaemonSpecGenerationKey annotates the static IMEX DaemonSet with
	// the ComputeDomainDaemonSet generation its pod template was rendered from.
	staticDaemonSpecGenerationKey = "resource.nvidia.com/specGeneration"
	// staticDaemonAppliedGenerationKey annotates the static IMEX DaemonSet with
	// its generation as last written by the controller, to detect edits.
	staticDaemonAppliedGenerationKey = "resource.nvidia.com/appliedGeneration"
	// staticDaemonImageKey annotates the static IMEX DaemonSet with the image
	// pinned at creation, used while spec.image is empty.
	staticDaemonImageKey  = "resource.nvidia.com/pinnedImage"
	gpuCliqueNodeLabelKey = "nvidia.com/gpu.clique"

	// cddsSyncDelay batches the events of thousands of daemons into one sync.
	cddsSyncDelay = 500 * time.Millisecond
	// cddsResyncInterval rechecks the cliques and rollout without events.
	cddsResyncInterval = time.Minute
	// cliqueRestartTimeout bounds how long a restarted clique counts as in
	// progress before the informer shows its new daemons.
	cliqueRestartTimeout = 2 * time.Minute
)

// StaticDaemonSetTemplateData renders the static IMEX DaemonSet.
type StaticDaemonSetTemplateData struct {
	Namespace                 string
	Name                      string
	DaemonSetLabelKey         string
	DaemonSetLabelValue       types.UID
	CliqueNodeLabelKey        string
	ComputeDomainNodeLabelKey string
	ResourceClaimTemplateName string
	MaxNodesPerIMEXDomain     int
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
	// channelClaimNodes returns the nodes with claims allocated an IMEX
	// channel, whose daemons must not be removed.
	channelClaimNodes func() map[string]bool

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
	now                   func() time.Time

	// restarting holds cliques whose daemons were deleted for a rollout,
	// with when.
	restartingMu sync.Mutex
	restarting   map[string]time.Time
}

func NewComputeDomainDaemonSetManager(config *ManagerConfig, channelClaimNodes func() map[string]bool) *ComputeDomainDaemonSetManager {
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
		channelClaimNodes:     channelClaimNodes,
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
		now:                   time.Now,
		restarting:            make(map[string]time.Time),
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
		m.reportDegraded(ctx, "ReconcileError", err)
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
	// An invalid spec leaves the daemons as they are, rather than failing to
	// apply it over and over.
	if err := m.validateSpec(cdds); err != nil {
		m.reportDegraded(ctx, "InvalidSpec", err)
		return 0, nil
	}
	rct, err := m.ensureResourceClaimTemplate(ctx, cdds)
	if err != nil {
		return 0, err
	}
	pods := m.pods(cdds.UID)
	state, err := m.ensureDaemonSet(ctx, cdds, rct.Name, pods)
	if err != nil {
		return 0, err
	}
	ds := state.ds

	if err := m.realignDaemons(ctx, pods); err != nil {
		return 0, err
	}
	if err := m.removeStaleDaemons(ctx, cdds, pods); err != nil {
		return 0, err
	}

	hash := ds.Spec.Template.Annotations[staticDaemonTemplateHashKey]
	cliques, unclustered := groupByClique(m.nodes(), pods, cdds.Spec.NodeSelector)
	var requeueAfter time.Duration
	if cdds.Spec.UpdateStrategy.Type == nvapi.ComputeDomainDaemonSetUpdateStrategyRollingUpdate {
		if requeueAfter, err = m.rollout(ctx, cdds, cliques, unclustered, hash); err != nil {
			return 0, err
		}
	}
	if err := m.updateStatus(ctx, cdds, state, cliques, pods, hash); err != nil {
		return 0, err
	}
	return requeueAfter, nil
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
	if len(m.channelClaimNodes()) > 0 {
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
// of its pod template. The image is spec.image, else the one pinned when the
// DaemonSet was created, so that driver upgrades do not change it.
func (m *ComputeDomainDaemonSetManager) renderDaemonSet(cdds *nvapi.ComputeDomainDaemonSet, rctName string, existing *appsv1.DaemonSet) (*appsv1.DaemonSet, error) {
	pinned := m.config.imageName
	if existing != nil && existing.Annotations[staticDaemonImageKey] != "" {
		pinned = existing.Annotations[staticDaemonImageKey]
	}
	image := cdds.Spec.Image
	if image == "" {
		image = pinned
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
		ComputeDomainNodeLabelKey: computeDomainLabelKey,
		ResourceClaimTemplateName: rctName,
		MaxNodesPerIMEXDomain:     cdds.Spec.MaxNodesPerClique,
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

	// Values from the spec are set here rather than in the template, so that
	// they cannot change the rendered YAML.
	podSpec := &ds.Spec.Template.Spec
	podSpec.Containers[0].Image = image
	if overrides := imexConfigOverridesEnv(cdds.Spec.IMEXConfig); overrides != "" {
		podSpec.Containers[0].Env = append(podSpec.Containers[0].Env, corev1.EnvVar{Name: "IMEX_CONFIG_OVERRIDES", Value: overrides})
	}
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
	ds.Annotations = map[string]string{staticDaemonImageKey: pinned}
	return &ds, nil
}

// imexConfigOverridesEnv formats IMEX config overrides as parsed by the
// daemon, sorted for a stable pod template.
func imexConfigOverridesEnv(overrides map[string]string) string {
	keys := slices.Sorted(maps.Keys(overrides))
	var env strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&env, "%s=%s,", key, overrides[key])
	}
	return env.String()
}

// validateSpec checks what the CRD schema cannot, before the spec is applied.
func (m *ComputeDomainDaemonSetManager) validateSpec(cdds *nvapi.ComputeDomainDaemonSet) error {
	var errs []error
	if err := imex.ValidateConfigOverrides(cdds.Spec.IMEXConfig); err != nil {
		errs = append(errs, fmt.Errorf("spec.imexConfig: %w", err))
	}
	// The daemon parses the overrides as comma-separated KEY=VALUE pairs.
	for key, value := range cdds.Spec.IMEXConfig {
		if strings.Contains(value, ",") {
			errs = append(errs, fmt.Errorf("spec.imexConfig value of %q must not contain a comma", key))
		}
	}
	for key, value := range cdds.Spec.NodeSelector {
		for _, msg := range validation.IsQualifiedName(key) {
			errs = append(errs, fmt.Errorf("spec.nodeSelector key %q: %s", key, msg))
		}
		for _, msg := range validation.IsValidLabelValue(value) {
			errs = append(errs, fmt.Errorf("spec.nodeSelector value %q: %s", value, msg))
		}
	}
	if name := cdds.Spec.PriorityClassName; name != "" {
		for _, msg := range validation.IsDNS1123Subdomain(name) {
			errs = append(errs, fmt.Errorf("spec.priorityClassName %q: %s", name, msg))
		}
	}
	for _, secret := range cdds.Spec.ImagePullSecrets {
		for _, msg := range validation.IsDNS1123Subdomain(secret.Name) {
			errs = append(errs, fmt.Errorf("spec.imagePullSecrets name %q: %s", secret.Name, msg))
		}
	}
	return utilerrors.NewAggregate(errs)
}

var nodeSelectorOperators = map[corev1.NodeSelectorOperator]selection.Operator{
	corev1.NodeSelectorOpIn:           selection.In,
	corev1.NodeSelectorOpNotIn:        selection.NotIn,
	corev1.NodeSelectorOpExists:       selection.Exists,
	corev1.NodeSelectorOpDoesNotExist: selection.DoesNotExist,
	corev1.NodeSelectorOpGt:           selection.GreaterThan,
	corev1.NodeSelectorOpLt:           selection.LessThan,
}

// podPlacedOn reports whether a DaemonSet with this pod spec runs a pod on the
// node, by its node selector and required node affinity.
func podPlacedOn(spec *corev1.PodSpec, node *corev1.Node) bool {
	nodeLabels := labels.Set(node.Labels)
	if !labels.SelectorFromSet(spec.NodeSelector).Matches(nodeLabels) {
		return false
	}
	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil || spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return true
	}
	for _, term := range spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		selector := labels.NewSelector()
		valid := true
		for _, expr := range term.MatchExpressions {
			op, ok := nodeSelectorOperators[expr.Operator]
			if !ok {
				valid = false
				break
			}
			req, err := labels.NewRequirement(expr.Key, op, expr.Values)
			if err != nil {
				valid = false
				break
			}
			selector = selector.Add(*req)
		}
		if valid && selector.Matches(nodeLabels) {
			return true
		}
	}
	return false
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

// staticDaemonSetState is the static IMEX DaemonSet after reconciling it.
type staticDaemonSetState struct {
	ds *appsv1.DaemonSet
	// templateOutdated is set when the driver renders a newer pod template
	// than the DaemonSet runs.
	templateOutdated bool
	// blockedNodes would lose their daemon under running workloads with the
	// spec change that is held back.
	blockedNodes []string
}

// ensureDaemonSet creates the static IMEX DaemonSet, or updates its pod
// template when the spec changed or the DaemonSet was edited. A newer driver
// template alone is not applied, so driver upgrades leave the daemons alone.
func (m *ComputeDomainDaemonSetManager) ensureDaemonSet(ctx context.Context, cdds *nvapi.ComputeDomainDaemonSet, rctName string, pods []*corev1.Pod) (*staticDaemonSetState, error) {
	var existing *appsv1.DaemonSet
	if obj, exists, err := m.daemonSetInf.GetStore().GetByKey(m.config.driverNamespace + "/" + staticDaemonSetName(cdds.UID)); err == nil && exists {
		existing, _ = obj.(*appsv1.DaemonSet)
	}
	desired, err := m.renderDaemonSet(cdds, rctName, existing)
	if err != nil {
		return nil, err
	}
	specGeneration := strconv.FormatInt(cdds.Generation, 10)

	if existing == nil {
		desired.Annotations[staticDaemonSpecGenerationKey] = specGeneration
		desired.Annotations[staticDaemonAppliedGenerationKey] = "1"
		created, err := m.config.clientsets.Core.AppsV1().DaemonSets(desired.Namespace).Create(ctx, desired, metav1.CreateOptions{})
		if errors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("waiting for DaemonSet %s in the informer", desired.Name)
		}
		if err != nil {
			return nil, fmt.Errorf("error creating DaemonSet: %w", err)
		}
		klog.Infof("Created static IMEX DaemonSet %s/%s", created.Namespace, created.Name)
		return &staticDaemonSetState{ds: created}, nil
	}

	sameTemplate := existing.Spec.Template.Annotations[staticDaemonTemplateHashKey] == desired.Spec.Template.Annotations[staticDaemonTemplateHashKey]
	specChanged := existing.Annotations[staticDaemonSpecGenerationKey] != specGeneration
	edited := existing.Annotations[staticDaemonAppliedGenerationKey] != strconv.FormatInt(existing.Generation, 10)
	if !specChanged && !edited {
		return &staticDaemonSetState{ds: existing, templateOutdated: !sameTemplate}, nil
	}

	// The DaemonSet controller removes daemons from nodes its pod template no
	// longer selects right away, whatever the update strategy.
	if blocked := m.nodesLosingDaemons(&desired.Spec.Template.Spec, pods); len(blocked) > 0 {
		return &staticDaemonSetState{ds: existing, blockedNodes: blocked}, nil
	}

	updated := existing.DeepCopy()
	updated.Spec.Template = desired.Spec.Template
	updated.Spec.UpdateStrategy = desired.Spec.UpdateStrategy
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[staticDaemonSpecGenerationKey] = specGeneration
	updated.Annotations[staticDaemonImageKey] = desired.Annotations[staticDaemonImageKey]
	result, err := m.config.clientsets.Core.AppsV1().DaemonSets(updated.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("error updating DaemonSet: %w", err)
	}
	// Spec writes bump the generation, metadata writes do not.
	if applied := strconv.FormatInt(result.Generation, 10); result.Annotations[staticDaemonAppliedGenerationKey] != applied {
		result = result.DeepCopy()
		result.Annotations[staticDaemonAppliedGenerationKey] = applied
		if result, err = m.config.clientsets.Core.AppsV1().DaemonSets(result.Namespace).Update(ctx, result, metav1.UpdateOptions{}); err != nil {
			return nil, fmt.Errorf("error recording the applied DaemonSet generation: %w", err)
		}
	}
	if edited && !specChanged {
		klog.Infof("Reverted changes to static IMEX DaemonSet %s/%s", result.Namespace, result.Name)
	} else {
		klog.Infof("Updated the pod template of static IMEX DaemonSet %s/%s", result.Namespace, result.Name)
	}
	return &staticDaemonSetState{ds: result}, nil
}

// nodesLosingDaemons returns the nodes whose daemon a pod spec would remove
// while claims there are allocated an IMEX channel.
func (m *ComputeDomainDaemonSetManager) nodesLosingDaemons(spec *corev1.PodSpec, pods []*corev1.Pod) []string {
	inUse := m.channelClaimNodes()
	if len(inUse) == 0 {
		return nil
	}
	nodes := make(map[string]*corev1.Node)
	for _, node := range m.nodes() {
		nodes[node.Name] = node
	}
	var blocked []string
	for _, pod := range pods {
		node, ok := nodes[pod.Spec.NodeName]
		if ok && inUse[node.Name] && !podPlacedOn(spec, node) {
			blocked = append(blocked, node.Name)
		}
	}
	slices.Sort(blocked)
	return slices.Compact(blocked)
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

func nodeReadiness(nodes []*corev1.Node) map[string]bool {
	ready := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		for _, c := range node.Status.Conditions {
			if c.Type == corev1.NodeReady {
				ready[node.Name] = c.Status == corev1.ConditionTrue
			}
		}
	}
	return ready
}

// rollout restarts the next batch of outdated cliques.
func (m *ComputeDomainDaemonSetManager) rollout(ctx context.Context, cdds *nvapi.ComputeDomainDaemonSet, cliques []*staticClique, unclustered []*corev1.Pod, hash string) (time.Duration, error) {
	now := m.now()
	plan := planCliqueRollout(cliques, unclustered, hash, cdds.Spec.UpdateStrategy.RollingUpdate, nodeReadiness(m.nodes()), m.restartingCliques(cliques, hash, now), now)
	if len(plan.cliques) > 0 {
		klog.Infof("Restarting the static IMEX daemons of cliques %v", plan.cliques)
	}
	for _, pod := range plan.pods {
		err := m.config.clientsets.Core.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &pod.UID},
		})
		if err != nil && !errors.IsNotFound(err) && !errors.IsConflict(err) {
			return 0, fmt.Errorf("error deleting outdated IMEX daemon pod %s: %w", pod.Name, err)
		}
	}
	m.restartingMu.Lock()
	for _, id := range plan.cliques {
		m.restarting[id] = now
	}
	m.restartingMu.Unlock()
	return plan.requeueAfter, nil
}

// restartingCliques returns the cliques restarted recently whose new daemons
// the informer may not show yet, and forgets the others.
func (m *ComputeDomainDaemonSetManager) restartingCliques(cliques []*staticClique, hash string, now time.Time) map[string]bool {
	m.restartingMu.Lock()
	defer m.restartingMu.Unlock()
	restarting := make(map[string]bool, len(m.restarting))
	for _, c := range cliques {
		started, ok := m.restarting[c.id]
		if !ok {
			continue
		}
		if !c.hasOutdatedPods(hash) || now.Sub(started) > cliqueRestartTimeout {
			delete(m.restarting, c.id)
			continue
		}
		restarting[c.id] = true
	}
	return restarting
}

// realignDaemons restarts the daemons whose node moved to another NVLink
// partition, so that their new pod joins the IMEX domain of the new one. The
// partition change disrupted both IMEX domains already, so this does not wait
// for the update strategy.
func (m *ComputeDomainDaemonSetManager) realignDaemons(ctx context.Context, pods []*corev1.Pod) error {
	partitionOf := make(map[string]string)
	for _, node := range m.nodes() {
		partitionOf[node.Name] = nodePartition(node)
	}
	for _, pod := range pods {
		joined, ok := daemonPartition(pod)
		current := partitionOf[pod.Spec.NodeName]
		if !ok || current == "" || joined == current || pod.DeletionTimestamp != nil {
			continue
		}
		klog.Infof("Node %s moved from NVLink partition %q to %q, restarting its static IMEX daemon %s", pod.Spec.NodeName, joined, current, pod.Name)
		err := m.config.clientsets.Core.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &pod.UID},
		})
		if err != nil && !errors.IsNotFound(err) && !errors.IsConflict(err) {
			return fmt.Errorf("error restarting IMEX daemon pod %s: %w", pod.Name, err)
		}
	}
	return nil
}

// daemonInPartition reports whether a pod still serves the daemon of a node in
// a partition. A daemon that has not joined a partition yet counts, as it
// may still join that one.
func daemonInPartition(pods []*corev1.Pod, node, partition string) bool {
	return slices.ContainsFunc(pods, func(pod *corev1.Pod) bool {
		joined, ok := daemonPartition(pod)
		return pod.Spec.NodeName == node && (!ok || joined == partition)
	})
}

// removeStaleDaemons removes clique entries of daemons whose pod is gone
// without unregistering, for example with its node, or that moved to another
// partition. A miss in the informer is confirmed against the API server
// first.
func (m *ComputeDomainDaemonSetManager) removeStaleDaemons(ctx context.Context, cdds *nvapi.ComputeDomainDaemonSet, pods []*corev1.Pod) error {
	var live []*corev1.Pod
	for _, obj := range m.cliqueInf.GetStore().List() {
		clique, ok := obj.(*nvapi.ComputeDomainClique)
		if !ok || clique.Labels[computeDomainDaemonSetLabelKey] != string(cdds.UID) {
			continue
		}
		partition := clique.Labels[computeDomainCliqueLabelKey]
		var keep []*nvapi.ComputeDomainDaemonInfo
		for _, daemon := range clique.Daemons {
			if daemonInPartition(pods, daemon.NodeName, partition) {
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
				live = make([]*corev1.Pod, 0, len(list.Items))
				for i := range list.Items {
					live = append(live, &list.Items[i])
				}
			}
			if daemonInPartition(live, daemon.NodeName, partition) {
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

func (m *ComputeDomainDaemonSetManager) updateStatus(ctx context.Context, cdds *nvapi.ComputeDomainDaemonSet, state *staticDaemonSetState, cliques []*staticClique, pods []*corev1.Pod, hash string) error {
	ds := state.ds
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
		case cdds.Spec.UpdateStrategy.Type != nvapi.ComputeDomainDaemonSetUpdateStrategyRollingUpdate:
			rollout.Reason, rollout.Message = "WaitingForPodDeletion", fmt.Sprintf("%d IMEX daemons run an outdated revision, delete their pods to update them", outdated)
		case cdds.Spec.UpdateStrategy.RollingUpdate != nil && cdds.Spec.UpdateStrategy.RollingUpdate.Paused:
			rollout.Reason, rollout.Message = "Paused", fmt.Sprintf("%d IMEX daemons run an outdated revision", outdated)
		default:
			rollout.Reason, rollout.Message = "RollingUpdate", fmt.Sprintf("%d IMEX daemons run an outdated revision", outdated)
		}
	}
	outdated := metav1.Condition{Type: nvapi.ComputeDomainDaemonSetConditionTemplateOutdated, Status: metav1.ConditionFalse, Reason: "TemplateCurrent",
		Message: "the daemons run the pod template of this driver"}
	if state.templateOutdated {
		outdated.Status, outdated.Reason = metav1.ConditionTrue, "DriverUpgraded"
		outdated.Message = fmt.Sprintf("this driver renders a newer IMEX daemon template, applied with the next spec change, for example spec.image=%s", m.config.imageName)
	}
	blocked := metav1.Condition{Type: nvapi.ComputeDomainDaemonSetConditionUpdateBlocked, Status: metav1.ConditionFalse, Reason: "Applied",
		Message: "the spec is applied"}
	if len(state.blockedNodes) > 0 {
		blocked.Status, blocked.Reason = metav1.ConditionTrue, "NodesInUse"
		blocked.Message = truncate(fmt.Sprintf("the spec would remove IMEX daemons from nodes with workloads using IMEX channels: %v", state.blockedNodes))
	}
	degraded := metav1.Condition{Type: nvapi.ComputeDomainDaemonSetConditionDegraded, Status: metav1.ConditionFalse, Reason: "Reconciled",
		Message: "the daemons are reconciled"}
	for _, c := range []metav1.Condition{ready, rollout, outdated, blocked, degraded} {
		c.ObservedGeneration = cdds.Generation
		meta.SetStatusCondition(&status.Conditions, c)
	}
	meta.RemoveStatusCondition(&status.Conditions, nvapi.ComputeDomainDaemonSetConditionDeletionBlocked)
	return m.writeStatus(ctx, cdds, newCDDS)
}

// reportDegraded records why the daemons cannot be reconciled. It is best
// effort, as the sync is retried anyway.
func (m *ComputeDomainDaemonSetManager) reportDegraded(ctx context.Context, reason string, cause error) {
	cdds, err := m.get()
	if err != nil || cdds == nil {
		return
	}
	newCDDS := cdds.DeepCopy()
	meta.SetStatusCondition(&newCDDS.Status.Conditions, metav1.Condition{
		Type:               nvapi.ComputeDomainDaemonSetConditionDegraded,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            truncate(cause.Error()),
		ObservedGeneration: cdds.Generation,
	})
	if err := m.writeStatus(ctx, cdds, newCDDS); err != nil {
		klog.V(2).Infof("Error reporting ComputeDomainDaemonSet as degraded: %v", err)
	}
}

// truncate bounds condition messages.
func truncate(msg string) string {
	const maxLen = 1024
	if len(msg) <= maxLen {
		return msg
	}
	return msg[:maxLen-3] + "..."
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

// reportStaticDaemonsDisabled flags an existing ComputeDomainDaemonSet while
// the controller does not run static IMEX daemons. Its daemons keep running,
// unmanaged, and would conflict with per-ComputeDomain IMEX daemons.
func reportStaticDaemonsDisabled(ctx context.Context, config *ManagerConfig) {
	client := config.clientsets.Nvidia.ResourceV1beta1().ComputeDomainDaemonSets()
	cdds, err := client.Get(ctx, nvapi.ComputeDomainDaemonSetName, metav1.GetOptions{})
	if err != nil {
		// Also when the CRD is not installed.
		klog.V(4).Infof("No ComputeDomainDaemonSet to report: %v", err)
		return
	}
	message := fmt.Sprintf("the controller runs with IMEX %s and the ComputeDomainDaemonSet gate set to %t, so it does not manage these static IMEX daemons, which keep running. Use imex.scope=cluster, or delete the ComputeDomainDaemonSet", config.imexConfig, featuregates.Enabled(featuregates.ComputeDomainDaemonSet))
	klog.Warningf("ComputeDomainDaemonSet %s exists, but %s", cdds.Name, message)
	newCDDS := cdds.DeepCopy()
	meta.SetStatusCondition(&newCDDS.Status.Conditions, metav1.Condition{
		Type:               nvapi.ComputeDomainDaemonSetConditionDegraded,
		Status:             metav1.ConditionTrue,
		Reason:             "StaticDaemonsDisabled",
		Message:            truncate(message),
		ObservedGeneration: cdds.Generation,
	})
	if equality.Semantic.DeepEqual(cdds.Status, newCDDS.Status) {
		return
	}
	if _, err := client.UpdateStatus(ctx, newCDDS, metav1.UpdateOptions{}); err != nil {
		klog.Errorf("Error reporting ComputeDomainDaemonSet %s as unmanaged: %v", cdds.Name, err)
	}
}
