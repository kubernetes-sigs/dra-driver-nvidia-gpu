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
	"fmt"
	"slices"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func channelReservationsEnabled() bool {
	return featuregates.Enabled(featuregates.ComputeDomainChannelReservations)
}

// assertChannelReservedForComputeDomain backs up the controller's admission:
// the channel must be reserved in the ComputeDomain's status.
func (s *DeviceState) assertChannelReservedForComputeDomain(ctx context.Context, domainID string, channelID int) error {
	cd, err := s.computeDomainManager.GetComputeDomain(ctx, domainID)
	if err != nil {
		return fmt.Errorf("error getting ComputeDomain: %w", err)
	}
	// Retryable: the informer may not have caught up yet.
	if cd == nil {
		return fmt.Errorf("ComputeDomain not found: %s", domainID)
	}
	if len(cd.Status.Channels) == 0 {
		return fmt.Errorf("no IMEX channels reserved yet for ComputeDomain %s", domainID)
	}
	if !slices.Contains(cd.Status.Channels, channelID) {
		return permanentError{fmt.Errorf("IMEX channel %d is not reserved for ComputeDomain %s (reserved: %v)", channelID, domainID, cd.Status.Channels)}
	}
	return nil
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
// consumable shares let the same ComputeDomain share it.
func (s *DeviceState) assertChannelAvailable(channelID int, domainID string) error {
	if isConsumableSharesEnabled(s.config) {
		return s.assertImexChannelSharableByComputeDomain(channelID, domainID)
	}
	return s.assertImexChannelNotAllocated(channelID)
}
