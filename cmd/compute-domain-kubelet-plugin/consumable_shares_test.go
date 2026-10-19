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
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/internal/common"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
	nvfake "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/clientset/versioned/fake"
	nvinformers "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/informers/externalversions"
)

func setConsumableSharesGate(t *testing.T, enabled bool) {
	t.Helper()
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.ConsumableShares): enabled,
	}))
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
			string(featuregates.ConsumableShares): false,
		}))
	})
}

func consumableSharesConfig(base *Config, option string) *Config {
	base.flags.consumableShares = option
	return base
}

func TestValidateConsumableSharesFlag(t *testing.T) {
	tests := map[string]struct {
		gate    bool
		value   string
		wantErr string
	}{
		"disabled without gate":   {value: "disabled"},
		"empty without gate":      {value: ""},
		"unlimited requires gate": {value: "unlimited", wantErr: "requires feature gate"},
		"unlimited with gate":     {gate: true, value: "unlimited"},
		"integer with gate":       {gate: true, value: "8"},
		"memory is not a CD mode": {gate: true, value: "memory", wantErr: "invalid value"},
		"zero rejected":           {gate: true, value: "0", wantErr: "invalid value"},
		"negative rejected":       {gate: true, value: "-1", wantErr: "invalid value"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			setConsumableSharesGate(t, tc.gate)

			err := validateConsumableSharesFlag(tc.value)

			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestApplyConsumableShares(t *testing.T) {
	tests := map[string]struct {
		gate           bool
		option         string
		wantMulti      bool
		expectedShares *int64
	}{
		"gate disabled":  {gate: false, option: "unlimited"},
		"flag disabled":  {gate: true, option: "disabled"},
		"unlimited":      {gate: true, option: "unlimited", wantMulti: true},
		"integer shares": {gate: true, option: "3", wantMulti: true, expectedShares: ptr.To(int64(3))},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			setConsumableSharesGate(t, tc.gate)
			dev := (&ComputeDomainChannelInfo{ID: 0}).GetDevice()

			applyConsumableShares(&dev, consumableSharesConfig(driverManagedConfig(), tc.option))

			if !tc.wantMulti {
				assert.Nil(t, dev.AllowMultipleAllocations)
				assert.Empty(t, dev.Capacity)
				return
			}
			assert.Equal(t, ptr.To(true), dev.AllowMultipleAllocations)
			if tc.expectedShares == nil {
				assert.Empty(t, dev.Capacity, "unlimited sharing publishes no capacity to consume")
				return
			}
			shares, ok := dev.Capacity["shares"]
			require.True(t, ok)
			assert.Equal(t, *tc.expectedShares, shares.Value.Value())
			require.NotNil(t, shares.RequestPolicy)
			assert.Equal(t, int64(1), shares.RequestPolicy.Default.Value(), "each claim consumes one share by default")
			assert.Equal(t, *tc.expectedShares, shares.RequestPolicy.ValidRange.Max.Value())
		})
	}
}

func preparedChannelClaim(domainID string, channelID int) PreparedClaim {
	return PreparedClaim{
		CheckpointState: ClaimCheckpointStatePrepareCompleted,
		Status: claimStatus(
			[]resourceapi.DeviceRequestAllocationResult{allocationResult("channel", DriverName, "channel-0", nil)},
		),
		PreparedDevices: PreparedDevices{
			{
				Devices:     PreparedDeviceList{preparedChannel(channelID)},
				ConfigState: DeviceConfigState{Type: ComputeDomainChannelType, ComputeDomain: domainID},
			},
		},
	}
}

func TestAssertImexChannelSharableByComputeDomain(t *testing.T) {
	tests := map[string]struct {
		checkpoint *Checkpoint
		wantShared bool
		wantErr    string
	}{
		"no prior claim": {
			checkpoint: checkpointWithClaims(nil),
		},
		"same ComputeDomain shares": {
			checkpoint: checkpointWithClaims(map[string]PreparedClaim{
				"existing-uid": preparedChannelClaim("cd-a", 0),
			}),
			wantShared: true,
		},
		"different ComputeDomain rejected": {
			checkpoint: checkpointWithClaims(map[string]PreparedClaim{
				"existing-uid": preparedChannelClaim("cd-b", 0),
			}),
			wantErr: `channel 0 already allocated by claim existing-uid for a different ComputeDomain "cd-b"`,
		},
		"different channel ignored": {
			checkpoint: checkpointWithClaims(map[string]PreparedClaim{
				"existing-uid": preparedChannelClaim("cd-b", 1),
			}),
		},
		"incomplete claim ignored": {
			checkpoint: checkpointWithClaims(map[string]PreparedClaim{
				"existing-uid": func() PreparedClaim {
					pc := preparedChannelClaim("cd-b", 0)
					pc.CheckpointState = ClaimCheckpointStatePrepareStarted
					return pc
				}(),
			}),
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			state := &DeviceState{checkpointManager: &fakeCheckpointManager{checkpoint: tc.checkpoint}}

			shared, err := state.assertImexChannelSharableByComputeDomain(0, "cd-a")

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.True(t, isPermanentError(err), "a different ComputeDomain on the channel is not retried")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantShared, shared)
		})
	}
}

func TestValidateNoOverlappingPreparedDevicesConsumableShares(t *testing.T) {
	setConsumableSharesGate(t, true)

	newState := func() *DeviceState {
		state := testDeviceState()
		state.config = consumableSharesConfig(driverManagedConfig(), "unlimited")
		return state
	}

	t.Run("allows channel overlap", func(t *testing.T) {
		checkpoint := checkpointWithClaims(map[string]PreparedClaim{
			"existing-uid": preparedClaim(ClaimCheckpointStatePrepareCompleted, allocationResult("request", DriverName, "channel-0", nil)),
		})
		claim := claimWithResults("incoming-uid", allocationResult("request", DriverName, "channel-0", nil))

		require.NoError(t, newState().validateNoOverlappingPreparedDevices(checkpoint, claim))
	})

	t.Run("still rejects daemon overlap", func(t *testing.T) {
		checkpoint := checkpointWithClaims(map[string]PreparedClaim{
			"existing-uid": preparedClaim(ClaimCheckpointStatePrepareCompleted, allocationResult("request", DriverName, "daemon-0", nil)),
		})
		claim := claimWithResults("incoming-uid", allocationResult("request", DriverName, "daemon-0", nil))

		err := newState().validateNoOverlappingPreparedDevices(checkpoint, claim)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "requested device daemon-0 is already allocated to different claim existing-uid")
	})
}

func testCDInformer(t *testing.T) cache.SharedIndexInformer {
	t.Helper()
	cd := &configapi.ComputeDomain{
		ObjectMeta: metav1.ObjectMeta{Name: "cd", Namespace: "default", UID: "cd-a"},
	}
	factory := nvinformers.NewSharedInformerFactory(nvfake.NewSimpleClientset(), 0)
	informer := factory.Resource().V1beta1().ComputeDomains().Informer()
	require.NoError(t, informer.AddIndexers(cache.Indexers{"computeDomainUID": uidIndexer[*configapi.ComputeDomain]}))
	require.NoError(t, informer.GetIndexer().Add(cd))
	return informer
}

func TestApplyComputeDomainChannelConfigDriverManagedConsumableShares(t *testing.T) {
	setConsumableSharesGate(t, true)
	result := allocationResult("request", DriverName, "channel-0", nil)

	newState := func(t *testing.T, checkpoint *Checkpoint) *DeviceState {
		// The manager has no config/clientsets: calling AddNodeLabel or
		// AssertComputeDomainReady would panic, which proves they are skipped.
		return &DeviceState{
			config:               consumableSharesConfig(driverManagedConfig(), "unlimited"),
			checkpointManager:    &fakeCheckpointManager{checkpoint: checkpoint},
			computeDomainManager: &ComputeDomainManager{informer: testCDInformer(t), cliqueID: "clique-a"},
			cdi:                  &CDIHandler{devRoot: t.TempDir()},
			nvdevlib: &deviceLib{
				nvCapImexChanDevInfos: []*common.NVcapDeviceInfo{{Major: 234, Minor: 0, Path: "/dev/nvidia-caps-imex-channels/channel0"}},
			},
			allocatable: AllocatableDevices{
				"channel-0": &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: 0}},
			},
		}
	}

	t.Run("subsequent claim of the same ComputeDomain skips IMEX daemon bootstrap", func(t *testing.T) {
		state := newState(t, checkpointWithClaims(map[string]PreparedClaim{
			"first-uid": preparedChannelClaim("cd-a", 0),
		}))

		configState, err := state.applyComputeDomainChannelConfig(
			context.Background(),
			channelConfig("cd-a"),
			claimWithResults("second-uid", result),
			[]*resourceapi.DeviceRequestAllocationResult{&result},
		)

		require.NoError(t, err)
		require.NotNil(t, configState)
		assert.Equal(t, "cd-a", configState.ComputeDomain)
		require.NotNil(t, configState.containerEdits, "the shared channel must still be injected")
		assert.Len(t, configState.containerEdits.DeviceNodes, 1)
	})

	t.Run("claim of a different ComputeDomain is rejected", func(t *testing.T) {
		state := newState(t, checkpointWithClaims(map[string]PreparedClaim{
			"first-uid": preparedChannelClaim("cd-b", 0),
		}))

		_, err := state.applyComputeDomainChannelConfig(
			context.Background(),
			channelConfig("cd-a"),
			claimWithResults("second-uid", result),
			[]*resourceapi.DeviceRequestAllocationResult{&result},
		)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "for a different ComputeDomain")
	})
}

func TestApplyComputeDomainChannelConfigHostManagedConsumableSharesSkipsReadiness(t *testing.T) {
	setConsumableSharesGate(t, true)
	result := allocationResult("request", DriverName, "channel-0", nil)

	// nvidia-imex-ctl is absent under this devRoot, so the readiness check
	// would fail if it ran.
	state := &DeviceState{
		config:               consumableSharesConfig(hostManagedConfig(), "unlimited"),
		checkpointManager:    &fakeCheckpointManager{checkpoint: checkpointWithClaims(map[string]PreparedClaim{"first-uid": preparedChannelClaim("cd-a", 0)})},
		computeDomainManager: &ComputeDomainManager{informer: testCDInformer(t), cliqueID: "clique-a"},
		cdi:                  &CDIHandler{devRoot: t.TempDir()},
		nvdevlib: &deviceLib{
			devRoot:               t.TempDir(),
			nvCapImexChanDevInfos: []*common.NVcapDeviceInfo{{Major: 234, Minor: 0, Path: "/dev/nvidia-caps-imex-channels/channel0"}},
		},
		allocatable: AllocatableDevices{
			"channel-0": &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: 0}},
		},
	}

	configState, err := state.applyComputeDomainChannelConfig(
		context.Background(),
		channelConfig("cd-a"),
		claimWithResults("second-uid", result),
		[]*resourceapi.DeviceRequestAllocationResult{&result},
	)

	require.NoError(t, err)
	require.NotNil(t, configState.containerEdits)
	assert.Len(t, configState.containerEdits.DeviceNodes, 1)
}

func TestIsComputeDomainChannelInUseByOtherClaims(t *testing.T) {
	channelClaim := func(state ClaimCheckpointState, domainID string) PreparedClaim {
		return PreparedClaim{
			CheckpointState: state,
			Status: claimStatus(
				[]resourceapi.DeviceRequestAllocationResult{allocationResult("channel", DriverName, "channel-0", nil)},
				opaqueConfig(t, resourceapi.AllocationConfigSourceClaim, DriverName, []string{"channel"}, channelConfig(domainID)),
			),
		}
	}

	tests := map[string]struct {
		claims map[string]PreparedClaim
		want   bool
	}{
		"only the unprepared claim": {
			claims: map[string]PreparedClaim{"self-uid": channelClaim(ClaimCheckpointStatePrepareCompleted, "cd-a")},
		},
		"completed claim of the same ComputeDomain": {
			claims: map[string]PreparedClaim{"other-uid": channelClaim(ClaimCheckpointStatePrepareCompleted, "cd-a")},
			want:   true,
		},
		"started claim of the same ComputeDomain": {
			claims: map[string]PreparedClaim{"other-uid": channelClaim(ClaimCheckpointStatePrepareStarted, "cd-a")},
			want:   true,
		},
		"aborted claim ignored": {
			claims: map[string]PreparedClaim{"other-uid": channelClaim(ClaimCheckpointStatePrepareAborted, "cd-a")},
		},
		"different ComputeDomain ignored": {
			claims: map[string]PreparedClaim{"other-uid": channelClaim(ClaimCheckpointStatePrepareCompleted, "cd-b")},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			state := testDeviceState()

			got := state.isComputeDomainChannelInUseByOtherClaims(checkpointWithClaims(tc.claims), "self-uid", "cd-a")

			assert.Equal(t, tc.want, got)
		})
	}
}

func TestUnprepareDevicesConsumableSharesKeepsNodeLabel(t *testing.T) {
	setConsumableSharesGate(t, true)

	// computeDomainManager is intentionally nil: RemoveNodeLabel must not be
	// called while another claim of the same ComputeDomain is prepared.
	state := testDeviceState()
	state.config = consumableSharesConfig(driverManagedConfig(), "unlimited")
	status := claimStatus(
		[]resourceapi.DeviceRequestAllocationResult{allocationResult("channel", DriverName, "channel-0", nil)},
		opaqueConfig(t, resourceapi.AllocationConfigSourceClaim, DriverName, []string{"channel"}, channelConfig("cd-a")),
	)
	checkpoint := checkpointWithClaims(map[string]PreparedClaim{
		"self-uid":  {CheckpointState: ClaimCheckpointStatePrepareCompleted, Status: status},
		"other-uid": {CheckpointState: ClaimCheckpointStatePrepareCompleted, Status: status},
	})

	require.NoError(t, state.unprepareDevices(context.Background(), "self-uid", &status, checkpoint))
}
