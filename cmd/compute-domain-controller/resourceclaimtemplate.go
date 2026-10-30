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
	"encoding/json"
	"fmt"
	"sync"
	"text/template"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
)

const (
	DaemonSetResourceClaimTemplateTemplatePath = "/templates/compute-domain-daemon-claim-template.tmpl.yaml"
	WorkloadResourceClaimTemplateTemplatePath  = "/templates/compute-domain-workload-claim-template.tmpl.yaml"
)

type ResourceClaimTemplateTemplateData struct {
	Namespace               string
	Name                    string
	Finalizer               string
	ComputeDomainLabelKey   string
	ComputeDomainLabelValue types.UID
	TargetLabelKey          string
	TargetLabelValue        string
	DeviceClassName         string
	DriverName              string
	ChannelConfig           *nvapi.ComputeDomainChannelConfig
	DaemonConfig            *nvapi.ComputeDomainDaemonConfig
	// Owner, with per-ComputeDomain IMEX daemons, is the ComputeDomain whose
	// OwnerTaintKey the channel request tolerates, so that it lands on nodes
	// without, or with this, ComputeDomain's IMEX daemon.
	Owner         string
	OwnerTaintKey string
}

type BaseResourceClaimTemplateManager struct {
	config           *ManagerConfig
	waitGroup        sync.WaitGroup
	cancelContext    context.CancelFunc
	getComputeDomain GetComputeDomainFunc

	informer      cache.SharedIndexInformer
	mutationCache cache.MutationCache

	templatePath string

	cleanupManager *CleanupManager[*resourceapi.ResourceClaimTemplate]
}

type DaemonSetResourceClaimTemplateManager struct {
	*BaseResourceClaimTemplateManager
}

type WorkloadResourceClaimTemplateManager struct {
	*BaseResourceClaimTemplateManager
}

func newBaseResourceClaimTemplateManager(config *ManagerConfig, getComputeDomain GetComputeDomainFunc, labelSelector *metav1.LabelSelector, namespace string) *BaseResourceClaimTemplateManager {
	tweakListOptions := func(opts *metav1.ListOptions) {
		opts.LabelSelector = metav1.FormatLabelSelector(labelSelector)
	}

	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
				tweakListOptions(&options)
				return config.clientsets.Resource.ResourceClaimTemplates("").List(ctx, options)
			},
			WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
				tweakListOptions(&options)
				return config.clientsets.Resource.ResourceClaimTemplates("").Watch(ctx, options)
			},
		},
		&resourceapi.ResourceClaimTemplate{},
		informerResyncPeriod,
		cache.Indexers{},
	)

	m := &BaseResourceClaimTemplateManager{
		config:           config,
		getComputeDomain: getComputeDomain,
		informer:         informer,
	}
	m.cleanupManager = NewCleanupManager[*resourceapi.ResourceClaimTemplate](informer, getComputeDomain, m.cleanup)

	return m
}

func (m *BaseResourceClaimTemplateManager) Start(ctx context.Context) (rerr error) {
	ctx, cancel := context.WithCancel(ctx)
	m.cancelContext = cancel

	defer func() {
		if rerr != nil {
			if err := m.Stop(); err != nil {
				klog.Errorf("error stopping ResourceClaimTemplate manager: %v", err)
			}
		}
	}()

	if err := addComputeDomainLabelIndexer[*resourceapi.ResourceClaimTemplate](m.informer); err != nil {
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
		return fmt.Errorf("informer cache sync for ResourceClaimTemplate failed")
	}

	if err := m.cleanupManager.Start(ctx); err != nil {
		return fmt.Errorf("error starting cleanup manager: %w", err)
	}

	return nil
}

func (m *BaseResourceClaimTemplateManager) Stop() error {
	if m.cancelContext != nil {
		m.cancelContext()
	}
	m.waitGroup.Wait()
	return nil
}

func (m *BaseResourceClaimTemplateManager) Create(ctx context.Context, templatePath string, templateData *ResourceClaimTemplateTemplateData) (*resourceapi.ResourceClaimTemplate, error) {
	resourceClaimTemplate, err := renderResourceClaimTemplate(templatePath, templateData)
	if err != nil {
		return nil, err
	}

	rct, err := m.config.clientsets.Resource.ResourceClaimTemplates(resourceClaimTemplate.Namespace).Create(ctx, resourceClaimTemplate, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("error creating ResourceClaimTemplate: %w", err)
	}

	// Add the newly created ResourceClaimTemplate to the mutation cache
	// This ensures subsequent calls will see it immediately
	m.mutationCache.Mutation(rct)

	return rct, nil
}

func renderResourceClaimTemplate(templatePath string, templateData *ResourceClaimTemplateTemplateData) (*resourceapi.ResourceClaimTemplate, error) {
	tmpl, err := template.ParseFiles(templatePath)
	if err != nil {
		return nil, fmt.Errorf("failed to parse template file: %w", err)
	}

	var resourceClaimTemplateYaml bytes.Buffer
	if err := tmpl.Execute(&resourceClaimTemplateYaml, templateData); err != nil {
		return nil, fmt.Errorf("failed to execute template: %w", err)
	}

	var unstructuredObj unstructured.Unstructured
	err = yaml.Unmarshal(resourceClaimTemplateYaml.Bytes(), &unstructuredObj)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal yaml: %w", err)
	}

	var resourceClaimTemplate resourceapi.ResourceClaimTemplate
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.UnstructuredContent(), &resourceClaimTemplate)
	if err != nil {
		return nil, fmt.Errorf("failed to convert unstructured data to typed object: %w", err)
	}

	return &resourceClaimTemplate, nil
}

// Delete deletes all ResourceClaimTemplates of a ComputeDomain.
func (m *BaseResourceClaimTemplateManager) Delete(ctx context.Context, cdUID string) error {
	rcts, err := getByComputeDomainUID[*resourceapi.ResourceClaimTemplate](ctx, m.mutationCache, cdUID)
	if err != nil {
		return fmt.Errorf("error retrieving ResourceClaimTemplate: %w", err)
	}

	for _, rct := range rcts {
		if rct.GetDeletionTimestamp() != nil {
			continue
		}
		err = m.config.clientsets.Resource.ResourceClaimTemplates(rct.Namespace).Delete(ctx, rct.Name, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("error deleting ResourceClaimTemplate %s/%s: %w", rct.Namespace, rct.Name, err)
		}
	}

	return nil
}

// RemoveFinalizer removes the finalizer from all ResourceClaimTemplates of a
// ComputeDomain.
func (m *BaseResourceClaimTemplateManager) RemoveFinalizer(ctx context.Context, cdUID string) error {
	rcts, err := getByComputeDomainUID[*resourceapi.ResourceClaimTemplate](ctx, m.mutationCache, cdUID)
	if err != nil {
		return fmt.Errorf("error retrieving ResourceClaimTemplate: %w", err)
	}

	for _, rct := range rcts {
		if err := m.removeFinalizer(ctx, rct); err != nil {
			return err
		}
	}
	return nil
}

func (m *BaseResourceClaimTemplateManager) removeFinalizer(ctx context.Context, rct *resourceapi.ResourceClaimTemplate) error {
	if rct.GetDeletionTimestamp() == nil {
		return fmt.Errorf("attempting to remove finalizer before ResourceClaimTemplate marked for deletion")
	}

	newRCT := rct.DeepCopy()
	newRCT.Finalizers = []string{}
	for _, f := range rct.Finalizers {
		if f != computeDomainFinalizer {
			newRCT.Finalizers = append(newRCT.Finalizers, f)
		}
	}
	if len(rct.Finalizers) == len(newRCT.Finalizers) {
		return nil
	}

	if _, err := m.config.clientsets.Resource.ResourceClaimTemplates(rct.Namespace).Update(ctx, newRCT, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("error updating ResourceClaimTemplate: %w", err)
	}

	// Update mutation cache after successful update
	m.mutationCache.Mutation(newRCT)

	return nil
}

// replace deletes an outdated ResourceClaimTemplate so that it can be
// recreated, and reports whether it is gone. Claims already created from it
// keep working.
func (m *BaseResourceClaimTemplateManager) replace(ctx context.Context, rct *resourceapi.ResourceClaimTemplate) (bool, error) {
	client := m.config.clientsets.Resource.ResourceClaimTemplates(rct.Namespace)
	if rct.GetDeletionTimestamp() == nil {
		if err := client.Delete(ctx, rct.Name, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			return false, fmt.Errorf("error deleting outdated ResourceClaimTemplate %s/%s: %w", rct.Namespace, rct.Name, err)
		}
		klog.Infof("Replacing outdated ResourceClaimTemplate %s/%s", rct.Namespace, rct.Name)
		return false, nil
	}
	if err := m.removeFinalizer(ctx, rct); err != nil {
		return false, err
	}
	// The mutation cache may still hold the deleted object.
	_, err := client.Get(ctx, rct.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return true, nil
	}
	return false, err
}

func (m *BaseResourceClaimTemplateManager) AssertRemoved(ctx context.Context, cdUID string) error {
	rcts, err := getByComputeDomainUID[*resourceapi.ResourceClaimTemplate](ctx, m.informer.GetIndexer(), cdUID)
	if err != nil {
		return fmt.Errorf("error retrieving ResourceClaimTemplate: %w", err)
	}
	if len(rcts) != 0 {
		return fmt.Errorf("ResourceClaimTemplate for ComputeDomain %s still exists", cdUID)
	}
	return nil
}

func (m *BaseResourceClaimTemplateManager) cleanup(ctx context.Context, cdUID string) error {
	if err := m.Delete(ctx, cdUID); err != nil {
		return fmt.Errorf("error deleting ResourceClaimTemplate: %w", err)
	}
	if err := m.RemoveFinalizer(ctx, cdUID); err != nil {
		return fmt.Errorf("error removing ResourceClaimTemplate finalizer: %w", err)
	}
	return nil
}

func NewDaemonSetResourceClaimTemplateManager(config *ManagerConfig, getComputeDomain GetComputeDomainFunc) *DaemonSetResourceClaimTemplateManager {
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

	base := newBaseResourceClaimTemplateManager(config, getComputeDomain, labelSelector, config.driverNamespace)

	base.templatePath = DaemonSetResourceClaimTemplateTemplatePath
	m := &DaemonSetResourceClaimTemplateManager{
		BaseResourceClaimTemplateManager: base,
	}

	return m
}

func (m *DaemonSetResourceClaimTemplateManager) Create(ctx context.Context, cd *nvapi.ComputeDomain) (*resourceapi.ResourceClaimTemplate, error) {
	rcts, err := getByComputeDomainUID[*resourceapi.ResourceClaimTemplate](ctx, m.mutationCache, string(cd.UID))
	if err != nil {
		return nil, fmt.Errorf("error retrieving ResourceClaimTemplate: %w", err)
	}
	if len(rcts) > 1 {
		return nil, fmt.Errorf("more than one ResourceClaimTemplate found with same ComputeDomain UID")
	}
	if len(rcts) == 1 {
		return rcts[0], nil
	}

	rct, err := m.BaseResourceClaimTemplateManager.Create(ctx, m.templatePath, m.templateData(cd))
	if err != nil {
		return nil, fmt.Errorf("error creating ResourceClaimTemplate from base: %w", err)
	}

	return rct, nil
}

func (m *DaemonSetResourceClaimTemplateManager) templateData(cd *nvapi.ComputeDomain) *ResourceClaimTemplateTemplateData {
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
	return &templateData
}

func NewWorkloadResourceClaimTemplateManager(config *ManagerConfig, getComputeDomain GetComputeDomainFunc) *WorkloadResourceClaimTemplateManager {
	labelSelector := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      computeDomainLabelKey,
				Operator: metav1.LabelSelectorOpExists,
			},
			{
				Key:      computeDomainResourceClaimTemplateTargetLabelKey,
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{computeDomainResourceClaimTemplateTargetWorkload},
			},
		},
	}

	base := newBaseResourceClaimTemplateManager(config, getComputeDomain, labelSelector, "")

	base.templatePath = WorkloadResourceClaimTemplateTemplatePath
	m := &WorkloadResourceClaimTemplateManager{
		BaseResourceClaimTemplateManager: base,
	}

	return m
}

// channelAllocationModeFor decides the AllocationMode to request in the
// workload ResourceClaimTemplate for a ComputeDomain channel.
//
// When ComputeDomains share an IMEX domain (cluster scope) this always forces
// AllocationMode Single, regardless of what the ComputeDomain requested: the
// shared IMEX domain holds other ComputeDomains' channels.
//
// Otherwise (per-ComputeDomain IMEX daemons), the channel's own AllocationMode
// is used as-is.
func channelAllocationModeFor(channel nvapi.ComputeDomainChannelSpec, sharedDomain bool) string {
	if sharedDomain {
		return nvapi.ComputeDomainChannelAllocationModeSingle
	}
	return channel.AllocationMode
}

// Create ensures the workload ResourceClaimTemplates of a ComputeDomain exist.
// With channel reservations, each requests the shared channel device and
// names itself in its config, so that the controller assigns its claims one
// channel per NVLink clique at admission. Templates created before are kept
// as they are, so that existing ComputeDomains keep working as without
// channel reservations, on channel 0.
func (m *WorkloadResourceClaimTemplateManager) Create(ctx context.Context, cd *nvapi.ComputeDomain) error {
	channels := cd.Spec.GetChannels()
	reservations := channelReservationsEnabled()
	if !reservations && cd.Spec.Channel == nil {
		return fmt.Errorf("channels requires the %s feature gate", featuregates.ComputeDomainChannelReservations)
	}

	rcts, err := getByComputeDomainUID[*resourceapi.ResourceClaimTemplate](ctx, m.mutationCache, string(cd.UID))
	if err != nil {
		return fmt.Errorf("error retrieving ResourceClaimTemplate: %w", err)
	}
	existing := make(map[string]*resourceapi.ResourceClaimTemplate, len(rcts))
	for _, rct := range rcts {
		existing[rct.Name] = rct
	}

	for _, channel := range channels {
		name := channel.ResourceClaimTemplate.Name
		rct, exists := existing[name]
		if exists && (!reservations || !namesWorkloadTemplate(rct)) {
			continue
		}
		data := m.templateData(cd, channel, reservations)
		if exists {
			upToDate, err := m.upToDate(rct, data)
			if err != nil {
				return err
			}
			if upToDate {
				continue
			}
			gone, err := m.replace(ctx, rct)
			if err != nil {
				return err
			}
			if !gone {
				return fmt.Errorf("waiting for outdated ResourceClaimTemplate %s/%s to be deleted", rct.Namespace, rct.Name)
			}
		}
		if _, err := m.BaseResourceClaimTemplateManager.Create(ctx, m.templatePath, data); err != nil {
			return fmt.Errorf("error creating ResourceClaimTemplate '%s/%s': %w", cd.Namespace, name, err)
		}
	}
	return nil
}

// templateData renders a workload template. Without reservations it requests
// the default channel.
func (m *WorkloadResourceClaimTemplateManager) templateData(cd *nvapi.ComputeDomain, channel nvapi.ComputeDomainChannelSpec, reservations bool) *ResourceClaimTemplateTemplateData {
	channelConfig := nvapi.DefaultComputeDomainChannelConfig()
	channelConfig.DomainID = string(cd.UID)
	channelConfig.AllocationMode = channelAllocationModeFor(channel, m.config.imexConfig.SharedIMEXDomain())

	templateData := ResourceClaimTemplateTemplateData{
		Namespace:               cd.Namespace,
		Name:                    channel.ResourceClaimTemplate.Name,
		Finalizer:               computeDomainFinalizer,
		ComputeDomainLabelKey:   computeDomainLabelKey,
		ComputeDomainLabelValue: cd.UID,
		TargetLabelKey:          computeDomainResourceClaimTemplateTargetLabelKey,
		TargetLabelValue:        computeDomainResourceClaimTemplateTargetWorkload,
		DeviceClassName:         computeDomainDefaultChannelDeviceClass,
		DriverName:              DriverName,
		ChannelConfig:           channelConfig,
	}
	if reservations {
		templateData.DeviceClassName = computeDomainChannelDeviceClass
		channelConfig.ResourceClaimTemplateName = channel.ResourceClaimTemplate.Name
		if !m.config.imexConfig.SharedIMEXDomain() {
			templateData.Owner = string(cd.UID)
			templateData.OwnerTaintKey = imex.ChannelOwnerTaintKey
		}
	}
	return &templateData
}

// upToDate reports whether an existing template requests what the current
// one would.
func (m *WorkloadResourceClaimTemplateManager) upToDate(rct *resourceapi.ResourceClaimTemplate, data *ResourceClaimTemplateTemplateData) (bool, error) {
	expected, err := renderResourceClaimTemplate(m.templatePath, data)
	if err != nil {
		return false, err
	}
	return deviceRequestsMatch(rct, expected) && channelConfigsMatch(rct, expected), nil
}

// namesWorkloadTemplate reports whether a workload template names itself in
// its channel config, as templates created with channel reservations do.
func namesWorkloadTemplate(rct *resourceapi.ResourceClaimTemplate) bool {
	for _, c := range rct.Spec.Spec.Devices.Config {
		if config := decodeChannelConfig(c.Opaque); config != nil && config.ResourceClaimTemplateName != "" {
			return true
		}
	}
	return false
}

// channelConfigsMatch compares the channel configs of two templates, decoded
// as the API server may serialize them differently.
func channelConfigsMatch(actual, expected *resourceapi.ResourceClaimTemplate) bool {
	decode := func(rct *resourceapi.ResourceClaimTemplate) []nvapi.ComputeDomainChannelConfig {
		var configs []nvapi.ComputeDomainChannelConfig
		for _, c := range rct.Spec.Spec.Devices.Config {
			if c.Opaque == nil || c.Opaque.Driver != DriverName {
				continue
			}
			var config nvapi.ComputeDomainChannelConfig
			if err := json.Unmarshal(c.Opaque.Parameters.Raw, &config); err != nil {
				return nil
			}
			configs = append(configs, config)
		}
		return configs
	}
	actualConfigs := decode(actual)
	return actualConfigs != nil && equality.Semantic.DeepEqual(actualConfigs, decode(expected))
}

// deviceRequestsMatch compares the device requests of two templates after
// the defaults the API server applies.
func deviceRequestsMatch(actual, expected *resourceapi.ResourceClaimTemplate) bool {
	normalize := func(rct *resourceapi.ResourceClaimTemplate) []resourceapi.DeviceRequest {
		requests := make([]resourceapi.DeviceRequest, 0, len(rct.Spec.Spec.Devices.Requests))
		for _, r := range rct.Spec.Spec.Devices.Requests {
			r = *r.DeepCopy()
			if e := r.Exactly; e != nil {
				if e.AllocationMode == "" {
					e.AllocationMode = resourceapi.DeviceAllocationModeExactCount
				}
				if e.AllocationMode == resourceapi.DeviceAllocationModeExactCount && e.Count == 0 {
					e.Count = 1
				}
				for i := range e.Tolerations {
					if e.Tolerations[i].Operator == "" {
						e.Tolerations[i].Operator = resourceapi.DeviceTolerationOpEqual
					}
				}
			}
			requests = append(requests, r)
		}
		return requests
	}
	return equality.Semantic.DeepEqual(normalize(actual), normalize(expected))
}
