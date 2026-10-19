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
	"testing"

	resourceapi "k8s.io/api/resource/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/internal/common"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func setChannelReservationsGate(t *testing.T, enabled bool) {
	t.Helper()
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.ComputeDomainChannelReservations): enabled,
	}))
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
			string(featuregates.ComputeDomainChannelReservations): false,
		}))
	})
}

func testChannels(n int) (AllocatableDevices, []*common.NVcapDeviceInfo) {
	allocatable := AllocatableDevices{}
	var infos []*common.NVcapDeviceInfo
	for i := range n {
		allocatable[fmt.Sprintf("channel-%d", i)] = &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: i}}
		infos = append(infos, &common.NVcapDeviceInfo{Major: 234, Minor: i, Path: fmt.Sprintf("/dev/nvidia-caps-imex-channels/channel%d", i)})
	}
	return allocatable, infos
}

func TestComputeDomainPublishedDevicesChannelReservations(t *testing.T) {
	setChannelReservationsGate(t, true)
	allocatable, _ := testChannels(200)
	allocatable["daemon-0"] = &AllocatableDevice{Daemon: &ComputeDomainDaemonInfo{ID: 0}}
	config := driverManagedConfig()
	config.flags.imexMaxChannels = 130

	devices := computeDomainPublishedDevices(allocatable, config)
	slices := computeDomainPublishedSlices(allocatable, config)

	assert.Len(t, devices, 131, "channels 0..129 plus the daemon device")
	assert.NotContains(t, deviceNames(devices), "channel-130")
	require.Len(t, slices, 2, "a ResourceSlice holds at most 128 devices")
	assert.Len(t, slices[0].Devices, resourceapi.ResourceSliceMaxDevices)
	assert.Len(t, slices[1].Devices, 3)
}

// reservationTestState returns a DeviceState with channels 0..3 on a fabric
// node. Its ComputeDomainManager has no config/clientsets, so the IMEX daemon
// bootstrap (AddNodeLabel, AssertComputeDomainReady) would panic if called.
func reservationTestState(t *testing.T, config *Config, checkpoint *Checkpoint, reservedChannels ...int) *DeviceState {
	allocatable, infos := testChannels(4)
	return &DeviceState{
		config:               config,
		checkpointManager:    &fakeCheckpointManager{checkpoint: checkpoint},
		computeDomainManager: &ComputeDomainManager{informer: testCDInformer(t, reservedChannels...), cliqueID: "clique-a"},
		cdi:                  &CDIHandler{devRoot: t.TempDir()},
		nvdevlib:             &deviceLib{devRoot: t.TempDir(), nvCapImexChanDevInfos: infos},
		allocatable:          allocatable,
	}
}

func applyChannel(state *DeviceState, claimUID string, device string) (*DeviceConfigState, error) {
	result := allocationResult("request", DriverName, device, nil)
	return state.applyComputeDomainChannelConfig(
		context.Background(),
		channelConfig("cd-a"),
		claimWithResults(claimUID, result),
		[]*resourceapi.DeviceRequestAllocationResult{&result},
	)
}

func TestApplyChannelConfigDriverManagedChannelReservations(t *testing.T) {
	setChannelReservationsGate(t, true)

	t.Run("second template of a prepared ComputeDomain injects its own channel without bootstrap", func(t *testing.T) {
		state := reservationTestState(t, driverManagedConfig(), checkpointWithClaims(map[string]PreparedClaim{
			"first-uid": preparedChannelClaim("cd-a", 0),
		}), 0, 1, 2)

		configState, err := applyChannel(state, "second-uid", "channel-2")

		require.NoError(t, err)
		require.NotNil(t, configState.containerEdits)
		require.Len(t, configState.containerEdits.DeviceNodes, 1)
		assert.Equal(t, "/dev/nvidia-caps-imex-channels/channel2", configState.containerEdits.DeviceNodes[0].Path)
	})

	t.Run("unreserved channel is rejected permanently", func(t *testing.T) {
		state := reservationTestState(t, driverManagedConfig(), checkpointWithClaims(nil), 0, 1)

		_, err := applyChannel(state, "claim-uid", "channel-3")

		require.Error(t, err)
		assert.True(t, isPermanentError(err))
		assert.Contains(t, err.Error(), "IMEX channel 3 is not reserved for ComputeDomain cd-a")
	})

	t.Run("missing reservation is retried", func(t *testing.T) {
		state := reservationTestState(t, driverManagedConfig(), checkpointWithClaims(nil))

		_, err := applyChannel(state, "claim-uid", "channel-0")

		require.Error(t, err)
		assert.False(t, isPermanentError(err), "the informer may not have seen the reservation yet")
		assert.Contains(t, err.Error(), "no IMEX channels reserved yet")
	})

	t.Run("channel held by another claim is rejected without consumable shares", func(t *testing.T) {
		state := reservationTestState(t, driverManagedConfig(), checkpointWithClaims(map[string]PreparedClaim{
			"first-uid": preparedChannelClaim("cd-a", 1),
		}), 0, 1)

		_, err := applyChannel(state, "second-uid", "channel-1")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "channel 1 already allocated by claim first-uid")
	})
}

func TestApplyChannelConfigHostManagedChannelReservations(t *testing.T) {
	setChannelReservationsGate(t, true)

	t.Run("another ComputeDomain's channel is rejected", func(t *testing.T) {
		state := reservationTestState(t, hostManagedConfig(), checkpointWithClaims(nil), 1)

		_, err := applyChannel(state, "claim-uid", "channel-0")

		require.Error(t, err)
		assert.True(t, isPermanentError(err))
	})

	t.Run("prepared ComputeDomain skips the host IMEX readiness check", func(t *testing.T) {
		// nvidia-imex-ctl is absent under the test devRoot, so the readiness
		// check would fail if it ran.
		state := reservationTestState(t, hostManagedConfig(), checkpointWithClaims(map[string]PreparedClaim{
			"first-uid": preparedChannelClaim("cd-a", 2),
		}), 2, 3)

		configState, err := applyChannel(state, "second-uid", "channel-3")

		require.NoError(t, err)
		require.Len(t, configState.containerEdits.DeviceNodes, 1)
		assert.Equal(t, "/dev/nvidia-caps-imex-channels/channel3", configState.containerEdits.DeviceNodes[0].Path)
	})

	t.Run("first claim of a ComputeDomain checks host IMEX readiness", func(t *testing.T) {
		// Another ComputeDomain prepared on the node does not count.
		state := reservationTestState(t, hostManagedConfig(), checkpointWithClaims(map[string]PreparedClaim{
			"other-uid": preparedChannelClaim("cd-b", 0),
		}), 3)

		_, err := applyChannel(state, "claim-uid", "channel-3")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "host nvidia-imex daemon readiness check failed")
	})
}

func TestIsComputeDomainPreparedOnNode(t *testing.T) {
	started := preparedChannelClaim("cd-a", 0)
	started.CheckpointState = ClaimCheckpointStatePrepareStarted

	tests := map[string]struct {
		claims map[string]PreparedClaim
		want   bool
	}{
		"no claims":                    {},
		"completed claim, any channel": {claims: map[string]PreparedClaim{"uid": preparedChannelClaim("cd-a", 5)}, want: true},
		"started claim ignored":        {claims: map[string]PreparedClaim{"uid": started}},
		"other ComputeDomain ignored":  {claims: map[string]PreparedClaim{"uid": preparedChannelClaim("cd-b", 0)}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			state := &DeviceState{checkpointManager: &fakeCheckpointManager{checkpoint: checkpointWithClaims(tc.claims)}}

			got, err := state.isComputeDomainPreparedOnNode("cd-a")

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestUnprepareDevicesKeepsNodeLabelForOtherTemplates(t *testing.T) {
	// Without consumable shares, another claim of the same ComputeDomain on
	// a different channel must also keep the IMEX daemon on the node.
	// computeDomainManager is nil: RemoveNodeLabel must not be called.
	allocatable, _ := testChannels(2)
	state := &DeviceState{allocatable: allocatable, config: driverManagedConfig()}
	self := claimStatus(
		[]resourceapi.DeviceRequestAllocationResult{allocationResult("channel", DriverName, "channel-0", nil)},
		opaqueConfig(t, resourceapi.AllocationConfigSourceClaim, DriverName, []string{"channel"}, channelConfig("cd-a")),
	)
	other := claimStatus(
		[]resourceapi.DeviceRequestAllocationResult{allocationResult("channel", DriverName, "channel-1", nil)},
		opaqueConfig(t, resourceapi.AllocationConfigSourceClaim, DriverName, []string{"channel"}, channelConfig("cd-a")),
	)
	checkpoint := checkpointWithClaims(map[string]PreparedClaim{
		"self-uid":  {CheckpointState: ClaimCheckpointStatePrepareCompleted, Status: self},
		"other-uid": {CheckpointState: ClaimCheckpointStatePrepareCompleted, Status: other},
	})

	require.NoError(t, state.unprepareDevices(context.Background(), "self-uid", &self, checkpoint))
}

// TestApplyChannelConfigDriverManagedAllocationModeAll covers claims requesting
// all channels: they only ever expose the IMEX domain of their own
// ComputeDomain, as no other ComputeDomain can use the node.
func TestApplyChannelConfigDriverManagedAllocationModeAll(t *testing.T) {
	setChannelReservationsGate(t, true)
	setConsumableSharesGate(t, true)
	config := consumableSharesConfig(driverManagedConfig(), "unlimited")
	allChannels := channelConfig("cd-a")
	allChannels.AllocationMode = configapi.ComputeDomainChannelAllocationModeAll
	result := allocationResult("request", DriverName, "channel-0", nil)
	apply := func(state *DeviceState, claimUID string) (*DeviceConfigState, error) {
		return state.applyComputeDomainChannelConfig(context.Background(), allChannels, claimWithResults(claimUID, result), []*resourceapi.DeviceRequestAllocationResult{&result})
	}

	t.Run("shared by claims of the same ComputeDomain", func(t *testing.T) {
		state := reservationTestState(t, config, checkpointWithClaims(map[string]PreparedClaim{
			"first-uid": preparedChannelClaim("cd-a", 0),
		}), 0)

		configState, err := apply(state, "second-uid")

		require.NoError(t, err)
		assert.Len(t, configState.containerEdits.DeviceNodes, 4)
	})

	t.Run("rejected while another ComputeDomain holds the channel", func(t *testing.T) {
		state := reservationTestState(t, config, checkpointWithClaims(map[string]PreparedClaim{
			"first-uid": preparedChannelClaim("cd-b", 0),
		}), 0)

		_, err := apply(state, "second-uid")

		require.Error(t, err)
		assert.True(t, isPermanentError(err))
	})
}
