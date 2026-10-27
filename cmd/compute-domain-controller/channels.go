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
	"context"
	"encoding/json"
	"fmt"
	"sort"

	coreapi "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
)

// availableHostChannels intersects node pool advertisements independently for
// each physical NVLink fabric domain.
func availableHostChannels(ctx context.Context, config *ManagerConfig) (map[string][]int, error) {
	slices, err := config.clientsets.Resource.ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list ResourceSlices: %w", err)
	}
	type poolChannels struct {
		generation int64
		expected   int64
		slices     int64
		domain     string
		channels   map[int]bool
	}
	pools := make(map[string]*poolChannels)
	for _, slice := range slices.Items {
		if slice.Spec.Driver != DriverName || slice.Spec.NodeName == nil {
			continue
		}
		pool := pools[slice.Spec.Pool.Name]
		if pool == nil || slice.Spec.Pool.Generation > pool.generation {
			pool = &poolChannels{generation: slice.Spec.Pool.Generation, expected: slice.Spec.Pool.ResourceSliceCount, channels: make(map[int]bool)}
			pools[slice.Spec.Pool.Name] = pool
		}
		if slice.Spec.Pool.Generation != pool.generation {
			continue
		}
		pool.slices++
		for _, device := range slice.Spec.Devices {
			typeAttr := device.Attributes["type"]
			idAttr := device.Attributes["id"]
			if typeAttr.StringValue == nil {
				typeAttr = device.Attributes[resourceapi.QualifiedName(DriverName+"/type")]
			}
			if idAttr.IntValue == nil {
				idAttr = device.Attributes[resourceapi.QualifiedName(DriverName+"/id")]
			}
			if typeAttr.StringValue != nil && *typeAttr.StringValue == "channel" && idAttr.IntValue != nil && *idAttr.IntValue >= 0 {
				domainAttr := device.Attributes[imex.FabricDomainIDAttribute]
				if domainAttr.StringValue == nil {
					domainAttr = device.Attributes[resourceapi.QualifiedName(DriverName+"/"+imex.FabricDomainIDAttribute)]
				}
				if domainAttr.StringValue == nil || *domainAttr.StringValue == "" {
					return nil, fmt.Errorf("waiting for IMEX ResourceSlice pool %s to publish fabric domain identity", slice.Spec.Pool.Name)
				}
				if pool.domain != "" && pool.domain != *domainAttr.StringValue {
					return nil, fmt.Errorf("IMEX ResourceSlice pool %s contains multiple fabric domains", slice.Spec.Pool.Name)
				}
				pool.domain = *domainAttr.StringValue
				pool.channels[int(*idAttr.IntValue)] = true
			}
		}
	}
	type domainChannels struct {
		pools  int
		counts map[int]int
	}
	domains := make(map[string]*domainChannels)
	for _, pool := range pools {
		if pool.slices != pool.expected {
			return nil, fmt.Errorf("waiting for complete IMEX ResourceSlice pool publication")
		}
		if pool.domain == "" {
			continue
		}
		domain := domains[pool.domain]
		if domain == nil {
			domain = &domainChannels{counts: make(map[int]int)}
			domains[pool.domain] = domain
		}
		domain.pools++
		for id := range pool.channels {
			domain.counts[id]++
		}
	}
	available := make(map[string][]int)
	for fabricDomainID, domain := range domains {
		for id, count := range domain.counts {
			if count == domain.pools {
				available[fabricDomainID] = append(available[fabricDomainID], id)
			}
		}
		sort.Ints(available[fabricDomainID])
	}
	return available, nil
}

func (m *WorkloadResourceClaimTemplateManager) reserveHostChannel(ctx context.Context, uid string) (imex.ChannelReservation, error) {
	client := m.config.clientsets.Core.CoreV1().ConfigMaps(m.config.driverNamespace)
	var reservation imex.ChannelReservation
	err := retry.OnError(retry.DefaultRetry, func(err error) bool {
		return errors.IsConflict(err) || errors.IsAlreadyExists(err)
	}, func() error {
		registry, err := client.Get(ctx, imex.ChannelReservationsConfigMap, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			registry, err = client.Create(ctx, &coreapi.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: imex.ChannelReservationsConfigMap}}, metav1.CreateOptions{})
		}
		if err != nil {
			return err
		}
		reservations := make(map[string]imex.ChannelReservation)
		for owner := range registry.Data {
			current, _, err := imex.ChannelReservationFor(registry, owner)
			if err != nil {
				return err
			}
			reservations[owner] = current
		}
		for owner, current := range reservations {
			for other, existing := range reservations {
				if owner >= other || current.ChannelID != existing.ChannelID {
					continue
				}
				if current.FabricDomainID == "" || existing.FabricDomainID == "" || current.FabricDomainID == existing.FabricDomainID {
					return fmt.Errorf("channel %d in fabric domain %q reserved by both %s and %s", current.ChannelID, current.FabricDomainID, owner, other)
				}
			}
		}
		if current, exists := reservations[uid]; exists {
			reservation = current
			return nil
		}
		available, err := availableHostChannels(ctx, m.config)
		if err != nil {
			return err
		}
		type fabricDomainCandidate struct {
			id           string
			reservations int
		}
		var fabricDomains []fabricDomainCandidate
		for fabricDomainID := range available {
			candidate := fabricDomainCandidate{id: fabricDomainID}
			for _, existing := range reservations {
				if existing.FabricDomainID == fabricDomainID {
					candidate.reservations++
				}
			}
			fabricDomains = append(fabricDomains, candidate)
		}
		sort.Slice(fabricDomains, func(i, j int) bool {
			if fabricDomains[i].reservations != fabricDomains[j].reservations {
				return fabricDomains[i].reservations < fabricDomains[j].reservations
			}
			return fabricDomains[i].id < fabricDomains[j].id
		})
		for _, candidate := range fabricDomains {
			fabricDomainID := candidate.id
			for _, id := range available[fabricDomainID] {
				used := false
				for _, existing := range reservations {
					if existing.ChannelID == id && (existing.FabricDomainID == "" || existing.FabricDomainID == fabricDomainID) {
						used = true
						break
					}
				}
				if used {
					continue
				}
				reservation = imex.ChannelReservation{FabricDomainID: fabricDomainID, ChannelID: id}
				if err := imex.SetChannelReservation(registry, uid, reservation); err != nil {
					return err
				}
				if _, err := client.Update(ctx, registry, metav1.UpdateOptions{}); err != nil {
					return err
				}
				return nil
			}
		}
		return fmt.Errorf("no unreserved IMEX channel advertised in any fabric domain")
	})
	if err != nil {
		return imex.ChannelReservation{}, fmt.Errorf("reserve IMEX channel for ComputeDomain %s: %w", uid, err)
	}
	return reservation, nil
}

// releaseHostChannel is called only after the template has disappeared. Claims
// can outlive their template, so retain the reservation until those are gone.
func (m *WorkloadResourceClaimTemplateManager) releaseHostChannel(ctx context.Context, uid string) error {
	claims, err := m.config.clientsets.Resource.ResourceClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list ResourceClaims: %w", err)
	}
	for _, claim := range claims.Items {
		var configs []resourceapi.DeviceConfiguration
		for _, config := range claim.Spec.Devices.Config {
			configs = append(configs, config.DeviceConfiguration)
		}
		if claim.Status.Allocation != nil {
			for _, config := range claim.Status.Allocation.Devices.Config {
				configs = append(configs, config.DeviceConfiguration)
			}
		}
		for _, config := range configs {
			if config.Opaque == nil || config.Opaque.Driver != DriverName {
				continue
			}
			var identity struct {
				Kind     string `json:"kind"`
				DomainID string `json:"domainID"`
			}
			if err := json.Unmarshal(config.Opaque.Parameters.Raw, &identity); err != nil {
				return fmt.Errorf("decode ResourceClaim %s/%s configuration: %w", claim.Namespace, claim.Name, err)
			}
			if identity.Kind == "ComputeDomainChannelConfig" && identity.DomainID == uid {
				return fmt.Errorf("waiting for ResourceClaim %s/%s to be removed before releasing IMEX channel", claim.Namespace, claim.Name)
			}
		}
	}
	client := m.config.clientsets.Core.CoreV1().ConfigMaps(m.config.driverNamespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		registry, err := client.Get(ctx, imex.ChannelReservationsConfigMap, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, exists := registry.Data[uid]; !exists {
			return nil
		}
		if err := imex.DeleteChannelReservation(registry, uid); err != nil {
			return err
		}
		_, err = client.Update(ctx, registry, metav1.UpdateOptions{})
		return err
	})
}
