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
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func channelReservationsEnabled() bool {
	return featuregates.Enabled(featuregates.ComputeDomainChannelReservations)
}

// resourceClaimTemplateCount treats a missing count (objects stored before
// the field existed) as one template.
func resourceClaimTemplateCount(cd *nvapi.ComputeDomain) int {
	if cd.Spec.Channel == nil || cd.Spec.Channel.ResourceClaimTemplate.Count < 1 {
		return 1
	}
	return cd.Spec.Channel.ResourceClaimTemplate.Count
}

// workloadResourceClaimTemplateName keeps the user-provided name for a single
// template, so that existing manifests keep working.
func workloadResourceClaimTemplateName(cd *nvapi.ComputeDomain, index int) string {
	if resourceClaimTemplateCount(cd) == 1 {
		return cd.Spec.Channel.ResourceClaimTemplate.Name
	}
	return fmt.Sprintf("%s-%d", cd.Spec.Channel.ResourceClaimTemplate.Name, index)
}

func channelDeviceName(channel int) string {
	return fmt.Sprintf("channel-%d", channel)
}

func channelSelectorExpression(channel int) string {
	return fmt.Sprintf("device.attributes['%s'].id == %d", DriverName, channel)
}

// ChannelReservationManager reserves IMEX channels for ComputeDomains and
// records them in their status. Driver-managed ComputeDomains each have their
// own IMEX domain and use channels 0..count-1. Host-managed ComputeDomains
// share the host IMEX domain, so their channels are unique cluster-wide,
// except legacySharedChannel.
type ChannelReservationManager struct {
	config             *ManagerConfig
	listComputeDomains ListComputeDomainsFunc
	updateStatus       UpdateComputeDomainStatusFunc

	sync.Mutex
	// pending holds reservations the informer cache may not reflect yet.
	pending map[string][]int
}

func NewChannelReservationManager(config *ManagerConfig, list ListComputeDomainsFunc, updateStatus UpdateComputeDomainStatusFunc) *ChannelReservationManager {
	return &ChannelReservationManager{
		config:             config,
		listComputeDomains: list,
		updateStatus:       updateStatus,
		pending:            make(map[string][]int),
	}
}

// legacySharedChannel is the channel that host-managed ComputeDomains created
// before channel reservations keep sharing, as their running workloads use it.
// It is never reserved for new ComputeDomains.
const legacySharedChannel = 0

// Reserve returns the ComputeDomain with its channels reserved in status. A
// legacy host-managed ComputeDomain, whose workload template predates
// reservations, is recorded on legacySharedChannel.
func (m *ChannelReservationManager) Reserve(ctx context.Context, cd *nvapi.ComputeDomain, legacy bool) (*nvapi.ComputeDomain, error) {
	count := resourceClaimTemplateCount(cd)
	if count > m.config.imexMaxChannels {
		return nil, fmt.Errorf("ComputeDomain requests %d channels, but at most %d IMEX channels are available", count, m.config.imexMaxChannels)
	}

	m.Lock()
	defer m.Unlock()

	hostManaged := m.config.imexConfig.EffectiveHostManaged()
	var used map[int]string
	if hostManaged {
		var err error
		if used, err = m.usedChannels(string(cd.UID)); err != nil {
			return nil, err
		}
	}

	if len(cd.Status.Channels) > 0 {
		if err := m.validateReservation(cd.Status.Channels, count, used); err != nil {
			return nil, fmt.Errorf("invalid IMEX channel reservation %v: %w", cd.Status.Channels, err)
		}
		return cd, nil
	}

	var channels []int
	switch {
	case !hostManaged:
		for id := range count {
			channels = append(channels, id)
		}
	case legacy:
		channels = []int{legacySharedChannel}
	default:
		for id := legacySharedChannel + 1; id < m.config.imexMaxChannels && len(channels) < count; id++ {
			if _, taken := used[id]; !taken {
				channels = append(channels, id)
			}
		}
		if len(channels) < count {
			return nil, fmt.Errorf("only %d of the %d requested IMEX channels are unreserved", len(channels), count)
		}
	}

	newCD := cd.DeepCopy()
	newCD.Status.Channels = channels
	updatedCD, err := m.updateStatus(ctx, newCD)
	if err != nil {
		return nil, fmt.Errorf("error recording reserved IMEX channels: %w", err)
	}
	// An outdated CRD schema silently prunes the field.
	if !slices.Equal(updatedCD.Status.Channels, channels) {
		return nil, fmt.Errorf("status.channels was not persisted, apply the ComputeDomain CRD of this release")
	}
	m.pending[string(cd.UID)] = channels
	klog.Infof("Reserved IMEX channels %v for ComputeDomain %s/%s", channels, cd.Namespace, cd.Name)
	return updatedCD, nil
}

// validateReservation guards against an edited status: the spec is
// immutable, so a valid reservation never changes.
func (m *ChannelReservationManager) validateReservation(channels []int, count int, used map[int]string) error {
	if len(channels) != count {
		return fmt.Errorf("expected %d channels", count)
	}
	seen := make(map[int]bool, len(channels))
	for _, id := range channels {
		if id < 0 || id >= m.config.imexMaxChannels {
			return fmt.Errorf("channel %d out of range [0, %d)", id, m.config.imexMaxChannels)
		}
		if seen[id] {
			return fmt.Errorf("channel %d reserved twice", id)
		}
		seen[id] = true
		if owner, taken := used[id]; taken {
			return fmt.Errorf("channel %d also reserved by ComputeDomain %s", id, owner)
		}
	}
	return nil
}

// usedChannels maps channels reserved by ComputeDomains other than excludeUID
// to their owner. legacySharedChannel is shared, so it is never used up.
func (m *ChannelReservationManager) usedChannels(excludeUID string) (map[int]string, error) {
	cds, err := m.listComputeDomains()
	if err != nil {
		return nil, fmt.Errorf("error listing ComputeDomains: %w", err)
	}
	used := make(map[int]string)
	add := func(uid string, ids []int) {
		for _, id := range ids {
			if uid != excludeUID && id != legacySharedChannel {
				used[id] = uid
			}
		}
	}
	for _, cd := range cds {
		add(string(cd.UID), cd.Status.Channels)
	}
	for uid, ids := range m.pending {
		add(uid, ids)
	}
	return used, nil
}

// Release forgets a ComputeDomain about to lose its finalizer, whose status
// reservation goes away with it.
func (m *ChannelReservationManager) Release(uid string) {
	m.Lock()
	defer m.Unlock()
	delete(m.pending, uid)
}

// AssertNoClaimsReferenceComputeDomain holds back releasing a ComputeDomain's
// channels while claims, and thus workloads, may still use them.
func (m *ChannelReservationManager) AssertNoClaimsReferenceComputeDomain(ctx context.Context, uid string) error {
	claims, err := m.config.clientsets.Core.ResourceV1().ResourceClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing ResourceClaims: %w", err)
	}
	for _, claim := range claims.Items {
		if claimReferencesComputeDomain(&claim, uid) {
			return fmt.Errorf("waiting for ResourceClaim %s/%s to be removed before releasing IMEX channels", claim.Namespace, claim.Name)
		}
	}
	return nil
}

func claimReferencesComputeDomain(claim *resourceapi.ResourceClaim, uid string) bool {
	var configs []resourceapi.DeviceConfiguration
	for _, c := range claim.Spec.Devices.Config {
		configs = append(configs, c.DeviceConfiguration)
	}
	if claim.Status.Allocation != nil {
		for _, c := range claim.Status.Allocation.Devices.Config {
			configs = append(configs, c.DeviceConfiguration)
		}
	}
	return uid != "" && slices.ContainsFunc(configs, func(c resourceapi.DeviceConfiguration) bool {
		return opaqueChannelConfigDomainID(c.Opaque) == uid
	})
}

// opaqueChannelConfigDomainID returns the domain ID of a ComputeDomainChannelConfig
// of this driver, or "" for anything else (including undecodable configs,
// which must not block unrelated ComputeDomains).
func opaqueChannelConfigDomainID(opaque *resourceapi.OpaqueDeviceConfiguration) string {
	if opaque == nil || opaque.Driver != DriverName {
		return ""
	}
	var identity struct {
		Kind     string `json:"kind"`
		DomainID string `json:"domainID"`
	}
	if err := json.Unmarshal(opaque.Parameters.Raw, &identity); err != nil || identity.Kind != nvapi.ComputeDomainChannelConfigKind {
		return ""
	}
	return identity.DomainID
}
