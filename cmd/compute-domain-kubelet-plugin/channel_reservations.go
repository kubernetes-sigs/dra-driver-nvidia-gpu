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

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
)

func channelReservationsEnabled() bool {
	return featuregates.Enabled(featuregates.ComputeDomainChannelReservations)
}

// legacySharedChannel is the channel that host-managed ComputeDomains created
// before channel reservations share, as before.
const legacySharedChannel = 0

// admittedChannel returns the channel the controller assigned to the shared
// channel device of a claim at admission, from the claim's device status.
func admittedChannel(claim *resourceapi.ResourceClaim, result *resourceapi.DeviceRequestAllocationResult) (*imex.ChannelAllocation, error) {
	for _, status := range claim.Status.Devices {
		if status.Driver != result.Driver || status.Pool != result.Pool || status.Device != result.Device ||
			ptr.Deref(status.ShareID, "") != string(ptr.Deref(result.ShareID, "")) {
			continue
		}
		allocation, err := imex.DecodeChannelAllocation(status.Data)
		if err != nil {
			return nil, err
		}
		if allocation != nil {
			return allocation, nil
		}
	}
	return nil, fmt.Errorf("the controller assigned no channel, check that it runs with the %s feature gate", featuregates.ComputeDomainChannelReservations)
}

// isComputeDomainPreparedOnNode reports whether a claim of the ComputeDomain
// completed Prepare() on this node, which includes the per-node IMEX setup.
func (s *DeviceState) isComputeDomainPreparedOnNode(domainID string) (bool, error) {
	cp, err := s.getCheckpoint()
	if err != nil {
		return false, fmt.Errorf("unable to get checkpoint: %w", err)
	}
	for _, claim := range cp.V2.PreparedClaims {
		if claim.CheckpointState != ClaimCheckpointStatePrepareCompleted {
			continue
		}
		for _, group := range claim.PreparedDevices {
			if group.ConfigState.ComputeDomain == domainID && len(group.Devices.ComputeDomainChannels()) > 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

// assertChannelAvailable fails if another claim holds the channel, unless
// channel reservations let claims of the same ComputeDomain share it.
func (s *DeviceState) assertChannelAvailable(channelID int, domainID string) error {
	if channelReservationsEnabled() && channelID == legacySharedChannel && s.config.imexConfig.EffectiveHostManaged() {
		return nil
	}
	if channelReservationsEnabled() {
		return s.assertImexChannelSharableByComputeDomain(channelID, domainID)
	}
	return s.assertImexChannelNotAllocated(channelID)
}

// assertImexChannelSharableByComputeDomain is the channel reservations
// variant of assertImexChannelNotAllocated(). Claims of a template share its
// channel, but a channel is never shared with another ComputeDomain, as that
// would break IMEX isolation.
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
