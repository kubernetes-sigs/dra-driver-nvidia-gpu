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
	"encoding/json"
	"slices"

	resourceapi "k8s.io/api/resource/v1"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func channelReservationsEnabled() bool {
	return featuregates.Enabled(featuregates.ComputeDomainChannelReservations)
}

// workloadResourceClaimTemplateNames returns the names of the workload
// ResourceClaimTemplates of a ComputeDomain, from channel or channels.
func workloadResourceClaimTemplateNames(cd *nvapi.ComputeDomain) []string {
	var names []string
	for _, c := range cd.Spec.GetChannels() {
		names = append(names, c.ResourceClaimTemplate.Name)
	}
	return names
}

// legacySharedChannel is the channel that host-managed ComputeDomains created
// before channel reservations keep sharing, as their running workloads use it.
// It is never assigned to other claims.
const legacySharedChannel = 0

// claimDomainIDs returns the domain IDs of the claim's channel configs.
func claimDomainIDs(claim *resourceapi.ResourceClaim) []string {
	var configs []resourceapi.DeviceConfiguration
	for _, c := range claim.Spec.Devices.Config {
		configs = append(configs, c.DeviceConfiguration)
	}
	if claim.Status.Allocation != nil {
		for _, c := range claim.Status.Allocation.Devices.Config {
			configs = append(configs, c.DeviceConfiguration)
		}
	}
	var ids []string
	for _, c := range configs {
		if id := opaqueChannelConfigDomainID(c.Opaque); id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
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

// decodeChannelConfig returns a ComputeDomainChannelConfig of this driver,
// or nil for anything else.
func decodeChannelConfig(opaque *resourceapi.OpaqueDeviceConfiguration) *nvapi.ComputeDomainChannelConfig {
	if opaque == nil || opaque.Driver != DriverName {
		return nil
	}
	var config nvapi.ComputeDomainChannelConfig
	if err := json.Unmarshal(opaque.Parameters.Raw, &config); err != nil || config.Kind != nvapi.ComputeDomainChannelConfigKind || config.DomainID == "" {
		return nil
	}
	return &config
}
