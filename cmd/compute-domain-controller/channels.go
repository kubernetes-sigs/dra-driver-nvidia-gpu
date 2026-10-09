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
	"strconv"

	coreapi "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
)

// availableHostChannels intersects node pool advertisements. A reservation
// must have the same meaning on every node in the shared host IMEX domain.
func availableHostChannels(ctx context.Context, config *ManagerConfig) ([]int, error) {
	slices, err := config.clientsets.Resource.ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list ResourceSlices: %w", err)
	}
	type poolChannels struct {
		generation int64
		expected   int64
		slices     int64
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
				pool.channels[int(*idAttr.IntValue)] = true
			}
		}
	}
	counts := make(map[int]int)
	for _, pool := range pools {
		if pool.slices != pool.expected {
			return nil, fmt.Errorf("waiting for complete IMEX ResourceSlice pool publication")
		}
		for id := range pool.channels {
			counts[id]++
		}
	}
	var channels []int
	for id, count := range counts {
		if count == len(pools) {
			channels = append(channels, id)
		}
	}
	sort.Ints(channels)
	return channels, nil
}

func (m *WorkloadResourceClaimTemplateManager) reserveHostChannel(ctx context.Context, uid string) (int, error) {
	client := m.config.clientsets.Core.CoreV1().ConfigMaps(m.config.driverNamespace)
	channel := -1
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
		used := make(map[int]string)
		for owner, value := range registry.Data {
			id, err := strconv.Atoi(value)
			if err != nil || id < 0 {
				return fmt.Errorf("invalid channel reservation for ComputeDomain %s: %q", owner, value)
			}
			if other, exists := used[id]; exists {
				return fmt.Errorf("channel %d reserved by both %s and %s", id, other, owner)
			}
			used[id] = owner
			if owner == uid {
				channel = id
			}
		}
		if channel >= 0 {
			return nil
		}
		channels, err := availableHostChannels(ctx, m.config)
		if err != nil {
			return err
		}
		for _, id := range channels {
			if _, exists := used[id]; exists {
				continue
			}
			if registry.Data == nil {
				registry.Data = make(map[string]string)
			}
			registry.Data[uid] = strconv.Itoa(id)
			if _, err := client.Update(ctx, registry, metav1.UpdateOptions{}); err != nil {
				return err
			}
			channel = id
			return nil
		}
		return fmt.Errorf("no unreserved IMEX channel advertised on all nodes")
	})
	if err != nil {
		return 0, fmt.Errorf("reserve IMEX channel for ComputeDomain %s: %w", uid, err)
	}
	return channel, nil
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
		delete(registry.Data, uid)
		_, err = client.Update(ctx, registry, metav1.UpdateOptions{})
		return err
	})
}
