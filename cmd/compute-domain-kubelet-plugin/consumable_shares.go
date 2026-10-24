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
	"fmt"
	"strconv"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

const (
	consumableSharesDisabled  = "disabled"
	consumableSharesUnlimited = "unlimited"
)

// validateConsumableSharesFlag accepts 'disabled', 'unlimited', or a positive
// integer. Unlike the GPU plugin there is no 'memory' mode: an IMEX channel
// has no memory to account for.
func validateConsumableSharesFlag(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || value == consumableSharesDisabled {
		return nil
	}
	if !featuregates.Enabled(featuregates.ConsumableShares) {
		return fmt.Errorf("--consumable-shares requires feature gate %s to be enabled", featuregates.ConsumableShares)
	}
	// Only channel reservations keep the scheduler from sharing a channel
	// between ComputeDomains on a node (node ownership, channel taints).
	if !featuregates.Enabled(featuregates.ComputeDomainChannelReservations) {
		return fmt.Errorf("--consumable-shares requires feature gate %s to be enabled", featuregates.ComputeDomainChannelReservations)
	}
	if value == consumableSharesUnlimited {
		return nil
	}
	if val, err := strconv.Atoi(value); err != nil || val <= 0 {
		return fmt.Errorf("invalid value for --consumable-shares: %q (must be '%s', '%s', or a positive integer)", value, consumableSharesDisabled, consumableSharesUnlimited)
	}
	return nil
}

func isConsumableSharesEnabled(config *Config) bool {
	if !featuregates.Enabled(featuregates.ConsumableShares) {
		return false
	}
	if config == nil || config.flags == nil {
		return false
	}
	sharesOption := strings.TrimSpace(config.flags.consumableShares)
	return sharesOption != "" && sharesOption != consumableSharesDisabled
}

// applyConsumableShares marks an IMEX channel device as multi-allocatable so
// that several claims (typically the per-pod claims generated from one
// ComputeDomain's ResourceClaimTemplate) can share it on the same node. With
// an integer option, each claim consumes one share by default, which caps the
// number of claims per channel.
func applyConsumableShares(dev *resourceapi.Device, config *Config) {
	if !isConsumableSharesEnabled(config) {
		return
	}
	dev.AllowMultipleAllocations = new(true)

	val, err := strconv.Atoi(strings.TrimSpace(config.flags.consumableShares))
	if err != nil || val <= 0 {
		// 'unlimited': no capacity to consume.
		return
	}

	if dev.Capacity == nil {
		dev.Capacity = make(map[resourceapi.QualifiedName]resourceapi.DeviceCapacity)
	}
	sharesVal := *resource.NewQuantity(int64(val), resource.DecimalSI)
	oneVal := *resource.NewQuantity(1, resource.DecimalSI)
	dev.Capacity["shares"] = resourceapi.DeviceCapacity{
		Value: sharesVal,
		RequestPolicy: &resourceapi.CapacityRequestPolicy{
			Default: new(oneVal.DeepCopy()),
			ValidRange: &resourceapi.CapacityRequestPolicyRange{
				Min:  new(oneVal.DeepCopy()),
				Max:  new(sharesVal.DeepCopy()),
				Step: new(oneVal.DeepCopy()),
			},
		},
	}
}

// assertImexChannelSharableByComputeDomain is the consumable-shares variant of
// assertImexChannelNotAllocated(). A channel may be shared between claims of
// the same ComputeDomain only, as sharing it with another ComputeDomain would
// break IMEX isolation.
func (s *DeviceState) assertImexChannelSharableByComputeDomain(id int, domainID string) error {
	cp, err := s.getCheckpoint()
	if err != nil {
		return fmt.Errorf("unable to get checkpoint: %w", err)
	}

	for claimUID, claim := range cp.V2.PreparedClaims {
		// See assertImexChannelNotAllocated() for why non-completed
		// preparations are ignored.
		if claim.CheckpointState != ClaimCheckpointStatePrepareCompleted {
			continue
		}
		for _, group := range claim.PreparedDevices {
			for _, d := range group.Devices {
				if d.Channel == nil || d.Channel.Info == nil || d.Channel.Info.ID != id {
					continue
				}
				// Permanent, so that a pod placed on another ComputeDomain's
				// node fails fast instead of retrying until that one leaves.
				if group.ConfigState.ComputeDomain != domainID {
					return permanentError{fmt.Errorf("channel %d already allocated by claim %s for a different ComputeDomain %q (according to checkpoint)", id, claimUID, group.ConfigState.ComputeDomain)}
				}
			}
		}
	}
	return nil
}

// isComputeDomainChannelInUseByOtherClaims reports whether any claim other
// than claimUID still references a channel of the given ComputeDomain on this
// node. Claims still in PrepareStarted count as well: they may be waiting for
// the IMEX daemon that removing the node label would tear down.
func (s *DeviceState) isComputeDomainChannelInUseByOtherClaims(checkpoint *Checkpoint, claimUID string, domainID string) bool {
	if checkpoint == nil || checkpoint.V2 == nil {
		return false
	}
	for otherUID, otherClaim := range checkpoint.V2.PreparedClaims {
		if otherUID == claimUID {
			continue
		}
		if otherClaim.CheckpointState != ClaimCheckpointStatePrepareCompleted &&
			otherClaim.CheckpointState != ClaimCheckpointStatePrepareStarted {
			continue
		}
		if otherClaim.Status.Allocation == nil {
			continue
		}
		configResultsMap, err := s.getConfigResultsMap(&otherClaim.Status, configapi.NonstrictDecoder)
		if err != nil {
			// Err on the side of keeping the IMEX daemon around: a stale
			// label is cleaned up when that claim is unprepared.
			klog.Warningf("Unable to resolve configs of claim %s, assuming it uses ComputeDomain %s: %v", otherUID, domainID, err)
			return true
		}
		for c := range configResultsMap {
			if config, ok := c.(*configapi.ComputeDomainChannelConfig); ok && config.DomainID == domainID {
				return true
			}
		}
	}
	return false
}
