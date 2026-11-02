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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/internal/common"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
	nvfake "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/clientset/versioned/fake"
	nvinformers "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/informers/externalversions"
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

// testChannels returns n channels of a node and the shared channel device.
func testChannels(n int) (AllocatableDevices, []*common.NVcapDeviceInfo) {
	allocatable := AllocatableDevices{}
	var infos []*common.NVcapDeviceInfo
	for i := range n {
		allocatable[fmt.Sprintf("channel-%d", i)] = &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: i}}
		infos = append(infos, &common.NVcapDeviceInfo{Major: 234, Minor: i, Path: fmt.Sprintf("/dev/nvidia-caps-imex-channels/channel%d", i)})
	}
	allocatable[imex.ChannelDeviceName] = &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: sharedChannelID}}
	return allocatable, infos
}

// admitChannel sets the channel the controller assigned to a claim's
// allocated device at admission.
func admitChannel(t *testing.T, claim *resourceapi.ResourceClaim, result resourceapi.DeviceRequestAllocationResult, channel int) {
	t.Helper()
	data, err := imex.ChannelAllocation{Channel: channel, IMEXDomain: "clique-a"}.Encode()
	require.NoError(t, err)
	claim.Status.Devices = append(claim.Status.Devices, resourceapi.AllocatedDeviceStatus{
		Driver: result.Driver,
		Pool:   result.Pool,
		Device: result.Device,
		Data:   data,
	})
}

func TestComputeDomainPublishedDevicesChannelReservations(t *testing.T) {
	setChannelReservationsGate(t, true)
	allocatable, _ := testChannels(2048)
	allocatable["daemon-0"] = &AllocatableDevice{Daemon: &ComputeDomainDaemonInfo{ID: 0}}

	t.Run("per-ComputeDomain IMEX daemons", func(t *testing.T) {
		devices := computeDomainPublishedDevices(allocatable, driverManagedConfig(), channelDeviceState{cliqueID: "clique"})

		require.Equal(t, []string{imex.ChannelDeviceName, "channel-0", "daemon-0"}, deviceNames(devices), "one channel device instead of one per channel")
		channel, legacy, daemon := findDevice(t, devices, imex.ChannelDeviceName), findDevice(t, devices, "channel-0"), findDevice(t, devices, "daemon-0")
		assert.Nil(t, legacy.AllowMultipleAllocations, "channel 0 stays as before for ComputeDomains from before the gate")
		assert.Empty(t, legacy.BindingConditions)
		assert.Equal(t, ptr.To(int64(0)), legacy.Attributes["id"].IntValue, "templates from before the gate select channel 0 by its ID")
		assert.Nil(t, daemon.AllowMultipleAllocations, "daemon devices back one IMEX daemon per node and stay exclusive")
		assert.Empty(t, daemon.BindingConditions, "daemon claims are not gated by the controller")

		assert.Equal(t, ptr.To(true), channel.AllowMultipleAllocations)
		assert.Empty(t, channel.Capacity, "claims share the device without consuming capacity")
		assert.ElementsMatch(t, []resourceapi.QualifiedName{"type", imex.ChannelCliqueAttribute}, keys(channel.Attributes), "the device has no channel ID")
		assert.Equal(t, ptr.To(imex.ChannelDeviceType), channel.Attributes["type"].StringValue, "DeviceClasses tell the shared device from channel 0")
		assert.Equal(t, ptr.To("channel"), legacy.Attributes["type"].StringValue)
		assert.Empty(t, channel.Taints, "a free node has no owner")
		assert.Equal(t, []string{imex.ChannelAdmittedCondition}, channel.BindingConditions)
		assert.Equal(t, []string{imex.ChannelRejectedCondition}, channel.BindingFailureConditions)
	})

	t.Run("the owner of a node is published", func(t *testing.T) {
		devices := computeDomainPublishedDevices(allocatable, driverManagedConfig(), channelDeviceState{owner: "cd-a"})

		assert.Equal(t, []resourceapi.DeviceTaint{{Key: imex.ChannelOwnerTaintKey, Value: "cd-a", Effect: resourceapi.DeviceTaintEffectNoSchedule}}, findDevice(t, devices, imex.ChannelDeviceName).Taints, "NoSchedule leaves claims already allocated alone")
		assert.Empty(t, findDevice(t, devices, "channel-0").Taints, "channel 0 is not admitted, the IMEX daemon node label keeps its ComputeDomain alone")
	})

	t.Run("a shared IMEX domain admits claims for their channel, without node owners", func(t *testing.T) {
		devices := computeDomainPublishedDevices(allocatable, hostManagedConfig(), channelDeviceState{owner: "ignored", cliqueID: "clique"})

		require.Equal(t, []string{imex.ChannelDeviceName, "channel-0"}, deviceNames(devices))
		channel := findDevice(t, devices, imex.ChannelDeviceName)
		assert.Equal(t, []string{imex.ChannelAdmittedCondition}, channel.BindingConditions, "the controller assigns the channel at admission")
		assert.Equal(t, ptr.To(true), channel.AllowMultipleAllocations)
		assert.Empty(t, channel.Capacity)
		assert.Empty(t, channel.Taints, "ComputeDomains share nodes")
		clique := channel.Attributes[imex.ChannelCliqueAttribute]
		assert.Equal(t, ptr.To("clique"), clique.StringValue)
	})

	t.Run("a node without a clique publishes none", func(t *testing.T) {
		devices := computeDomainPublishedDevices(allocatable, hostManagedConfig(), channelDeviceState{})

		assert.NotContains(t, findDevice(t, devices, imex.ChannelDeviceName).Attributes, resourceapi.QualifiedName(imex.ChannelCliqueAttribute))
	})

	t.Run("without the gate only channel 0 is published", func(t *testing.T) {
		setChannelReservationsGate(t, false)

		devices := computeDomainPublishedDevices(allocatable, driverManagedConfig(), channelDeviceState{owner: "cd-a"})

		assert.Equal(t, []string{"channel-0", "daemon-0"}, deviceNames(devices))
		assert.Empty(t, findDevice(t, devices, "channel-0").Taints)
	})
}

func findDevice(t *testing.T, devices []resourceapi.Device, name string) resourceapi.Device {
	t.Helper()
	for _, d := range devices {
		if d.Name == name {
			return d
		}
	}
	require.Failf(t, "device not published", "%s", name)
	return resourceapi.Device{}
}

// channelOwner returns the ComputeDomain a channel device is tainted with.
func channelOwner(device resourceapi.Device) string {
	for _, taint := range device.Taints {
		if taint.Key == imex.ChannelOwnerTaintKey {
			return taint.Value
		}
	}
	return ""
}

func keys[V any](m map[resourceapi.QualifiedName]V) []resourceapi.QualifiedName {
	var out []resourceapi.QualifiedName
	for k := range m {
		out = append(out, k)
	}
	return out
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

// reservationTestState returns a DeviceState with channels 0..3 on a fabric
// node. Its ComputeDomainManager has no config/clientsets, so the IMEX daemon
// bootstrap (AddNodeLabel, AssertComputeDomainReady) would panic if called.
func reservationTestState(t *testing.T, config *Config, checkpoint *Checkpoint) *DeviceState {
	allocatable, infos := testChannels(4)
	return &DeviceState{
		config:               config,
		checkpointManager:    &fakeCheckpointManager{checkpoint: checkpoint},
		computeDomainManager: &ComputeDomainManager{informer: testCDInformer(t), cliqueID: "clique-a"},
		cdi:                  &CDIHandler{devRoot: t.TempDir()},
		nvdevlib:             &deviceLib{devRoot: t.TempDir(), nvCapImexChanDevInfos: infos},
		allocatable:          allocatable,
	}
}

// preparedChannelClaim returns a claim prepared with a channel of a
// ComputeDomain.
func preparedChannelClaim(domainID string, channelID int) PreparedClaim {
	return PreparedClaim{
		CheckpointState: ClaimCheckpointStatePrepareCompleted,
		Status: claimStatus(
			[]resourceapi.DeviceRequestAllocationResult{allocationResult("channel", DriverName, imex.ChannelDeviceName, nil)},
		),
		PreparedDevices: PreparedDevices{
			{
				Devices:     PreparedDeviceList{preparedChannel(channelID)},
				ConfigState: DeviceConfigState{Type: ComputeDomainChannelType, ComputeDomain: domainID},
			},
		},
	}
}

// applyChannelConfig prepares a claim allocated a channel device, with the
// channel the controller assigned at admission if given.
func applyChannelConfig(t *testing.T, state *DeviceState, claimUID string, device string, config *configapi.ComputeDomainChannelConfig, channel ...int) (*DeviceConfigState, error) {
	t.Helper()
	result := allocationResult("request", DriverName, device, nil)
	claim := claimWithResults(claimUID, result)
	for _, c := range channel {
		admitChannel(t, claim, result, c)
	}
	return state.applyComputeDomainChannelConfig(
		context.Background(),
		config,
		claim,
		[]*resourceapi.DeviceRequestAllocationResult{&result},
	)
}

func injectedChannels(configState *DeviceConfigState) []string {
	var paths []string
	for _, node := range configState.containerEdits.DeviceNodes {
		paths = append(paths, node.Path)
	}
	return paths
}

func TestApplyChannelConfigDriverManagedChannelReservations(t *testing.T) {
	setChannelReservationsGate(t, true)

	t.Run("injects the channel of the claim status", func(t *testing.T) {
		state := reservationTestState(t, driverManagedConfig(), checkpointWithClaims(map[string]PreparedClaim{
			"first-uid": preparedChannelClaim("cd-a", 0),
		}))

		configState, err := applyChannelConfig(t, state, "second-uid", imex.ChannelDeviceName, channelConfig("cd-a"), 2)

		require.NoError(t, err)
		assert.Equal(t, []string{"/dev/nvidia-caps-imex-channels/channel2"}, injectedChannels(configState))
		assert.Equal(t, 2, configState.channelID)
	})

	t.Run("pods of a template share its channel", func(t *testing.T) {
		state := reservationTestState(t, driverManagedConfig(), checkpointWithClaims(map[string]PreparedClaim{
			"first-uid": preparedChannelClaim("cd-a", 1),
		}))

		configState, err := applyChannelConfig(t, state, "second-uid", imex.ChannelDeviceName, channelConfig("cd-a"), 1)

		require.NoError(t, err)
		assert.Equal(t, []string{"/dev/nvidia-caps-imex-channels/channel1"}, injectedChannels(configState))
	})

	t.Run("another ComputeDomain's channel on the node is rejected permanently", func(t *testing.T) {
		state := reservationTestState(t, driverManagedConfig(), checkpointWithClaims(map[string]PreparedClaim{
			"first-uid": preparedChannelClaim("cd-b", 0),
		}))

		_, err := applyChannelConfig(t, state, "second-uid", imex.ChannelDeviceName, channelConfig("cd-a"), 0)

		require.Error(t, err)
		assert.True(t, isPermanentError(err))
		assert.Contains(t, err.Error(), "for a different ComputeDomain")
	})

	permanent := map[string]struct {
		channel []int
		wantErr string
	}{
		"no channel from the controller": {wantErr: "the controller assigned no channel"},
		"channel not on node":            {channel: []int{4}, wantErr: "channel 4 out of range (node has 4 IMEX channels)"},
	}
	for name, tc := range permanent {
		t.Run(name, func(t *testing.T) {
			state := reservationTestState(t, driverManagedConfig(), checkpointWithClaims(nil))

			_, err := applyChannelConfig(t, state, "claim-uid", imex.ChannelDeviceName, channelConfig("cd-a"), tc.channel...)

			require.Error(t, err)
			assert.True(t, isPermanentError(err))
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}

	t.Run("per-channel devices allocated before the upgrade keep their channel", func(t *testing.T) {
		state := reservationTestState(t, driverManagedConfig(), checkpointWithClaims(map[string]PreparedClaim{
			"first-uid": preparedChannelClaim("cd-a", 0),
		}))

		configState, err := applyChannelConfig(t, state, "claim-uid", "channel-1", channelConfig("cd-a"))

		require.NoError(t, err)
		assert.Equal(t, []string{"/dev/nvidia-caps-imex-channels/channel1"}, injectedChannels(configState))
	})

	t.Run("ComputeDomains from before the gate keep channel 0", func(t *testing.T) {
		state := reservationTestState(t, driverManagedConfig(), checkpointWithClaims(map[string]PreparedClaim{
			"running-uid": preparedChannelClaim("cd-a", 0),
		}))

		configState, err := applyChannelConfig(t, state, "new-pod-uid", "channel-0", channelConfig("cd-a"))

		require.NoError(t, err)
		assert.Equal(t, []string{"/dev/nvidia-caps-imex-channels/channel0"}, injectedChannels(configState))
	})
}

func TestApplyChannelConfigHostManagedChannelReservations(t *testing.T) {
	setChannelReservationsGate(t, true)

	t.Run("another ComputeDomain's channel on the node is rejected", func(t *testing.T) {
		state := reservationTestState(t, hostManagedConfig(), checkpointWithClaims(map[string]PreparedClaim{
			"other-uid": preparedChannelClaim("cd-b", 1),
		}))

		_, err := applyChannelConfig(t, state, "claim-uid", imex.ChannelDeviceName, channelConfig("cd-a"), 1)

		require.Error(t, err)
		assert.True(t, isPermanentError(err))
		assert.Contains(t, err.Error(), "for a different ComputeDomain")
	})

	t.Run("legacy ComputeDomains keep sharing channel 0", func(t *testing.T) {
		state := reservationTestState(t, hostManagedConfig(), checkpointWithClaims(map[string]PreparedClaim{
			"other-uid": preparedChannelClaim("cd-b", legacySharedChannel),
		}))

		_, err := applyChannelConfig(t, state, "claim-uid", imex.ChannelDeviceName, channelConfig("cd-a"), legacySharedChannel)

		require.Error(t, err, "nvidia-imex-ctl is absent under the test devRoot")
		assert.Contains(t, err.Error(), "host nvidia-imex daemon readiness check failed", "the channel is not refused")
	})

	t.Run("prepared ComputeDomain skips the host IMEX readiness check", func(t *testing.T) {
		// nvidia-imex-ctl is absent under the test devRoot, so the readiness
		// check would fail if it ran.
		state := reservationTestState(t, hostManagedConfig(), checkpointWithClaims(map[string]PreparedClaim{
			"first-uid": preparedChannelClaim("cd-a", 2),
		}))

		configState, err := applyChannelConfig(t, state, "second-uid", imex.ChannelDeviceName, channelConfig("cd-a"), 3)

		require.NoError(t, err)
		assert.Equal(t, []string{"/dev/nvidia-caps-imex-channels/channel3"}, injectedChannels(configState))
	})

	t.Run("first claim of a ComputeDomain checks host IMEX readiness", func(t *testing.T) {
		// Another ComputeDomain prepared on the node does not count.
		state := reservationTestState(t, hostManagedConfig(), checkpointWithClaims(map[string]PreparedClaim{
			"other-uid": preparedChannelClaim("cd-b", 0),
		}))

		_, err := applyChannelConfig(t, state, "claim-uid", imex.ChannelDeviceName, channelConfig("cd-a"), 3)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "host nvidia-imex daemon readiness check failed")
	})
}

// TestPrepareDevicesRecordsChannel checks that the checkpoint records the
// channel of each claim, as claims share the channel device.
func TestPrepareDevicesRecordsChannel(t *testing.T) {
	setChannelReservationsGate(t, true)
	state := reservationTestState(t, driverManagedConfig(), checkpointWithClaims(map[string]PreparedClaim{
		"first-uid": preparedChannelClaim("cd-a", 0),
	}))
	state.cdi.vendor = "k8s.compute-domain.nvidia.com"
	state.cdi.claimClass = "claim"
	result := allocationResult("channel", DriverName, imex.ChannelDeviceName, nil)
	claim := claimWithResults("claim-uid", result)
	claim.Status.Allocation.Devices.Config = []resourceapi.DeviceAllocationConfiguration{
		opaqueConfig(t, resourceapi.AllocationConfigSourceClaim, DriverName, []string{"channel"}, channelConfig("cd-a")),
	}
	admitChannel(t, claim, result, 1)

	prepared, err := state.prepareDevices(context.Background(), claim)

	require.NoError(t, err)
	require.Len(t, prepared, 1)
	require.Len(t, prepared[0].Devices, 1)
	channel := prepared[0].Devices[0].Channel
	assert.Equal(t, 1, channel.Info.ID)
	assert.Equal(t, imex.ChannelDeviceName, channel.Device.DeviceName)
	assert.Equal(t, sharedChannelID, state.allocatable[imex.ChannelDeviceName].Channel.ID, "the shared device is not changed")
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

func TestAssertImexChannelSharableByComputeDomain(t *testing.T) {
	tests := map[string]struct {
		checkpoint *Checkpoint
		wantErr    string
	}{
		"no prior claim": {
			checkpoint: checkpointWithClaims(nil),
		},
		"same ComputeDomain shares": {
			checkpoint: checkpointWithClaims(map[string]PreparedClaim{
				"existing-uid": preparedChannelClaim("cd-a", 0),
			}),
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

			err := state.assertImexChannelSharableByComputeDomain(0, "cd-a")

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.True(t, isPermanentError(err), "a different ComputeDomain on the channel is not retried")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestValidateNoOverlappingPreparedDevicesSharedChannel(t *testing.T) {
	allocatable, _ := testChannels(1)
	allocatable["daemon-0"] = &AllocatableDevice{Daemon: &ComputeDomainDaemonInfo{ID: 0}}
	state := &DeviceState{allocatable: allocatable, config: driverManagedConfig()}

	t.Run("claims share the channel device", func(t *testing.T) {
		checkpoint := checkpointWithClaims(map[string]PreparedClaim{
			"existing-uid": preparedClaim(ClaimCheckpointStatePrepareCompleted, allocationResult("request", DriverName, imex.ChannelDeviceName, nil)),
		})
		claim := claimWithResults("incoming-uid", allocationResult("request", DriverName, imex.ChannelDeviceName, nil))

		require.NoError(t, state.validateNoOverlappingPreparedDevices(checkpoint, claim))
	})

	for _, device := range []string{"channel-0", "daemon-0"} {
		t.Run(device+" stays exclusive", func(t *testing.T) {
			checkpoint := checkpointWithClaims(map[string]PreparedClaim{
				"existing-uid": preparedClaim(ClaimCheckpointStatePrepareCompleted, allocationResult("request", DriverName, device, nil)),
			})
			claim := claimWithResults("incoming-uid", allocationResult("request", DriverName, device, nil))

			err := state.validateNoOverlappingPreparedDevices(checkpoint, claim)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "requested device "+device+" is already allocated to different claim existing-uid")
		})
	}
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

func TestUnprepareDevicesKeepsNodeLabelForOtherClaims(t *testing.T) {
	// Another claim of the same ComputeDomain, of the same or another
	// template, keeps the IMEX daemon on the node. computeDomainManager is
	// nil: RemoveNodeLabel must not be called.
	allocatable, _ := testChannels(2)
	state := &DeviceState{allocatable: allocatable, config: driverManagedConfig()}
	status := func(channel int) resourceapi.ResourceClaimStatus {
		return claimStatus(
			[]resourceapi.DeviceRequestAllocationResult{allocationResult("channel", DriverName, imex.ChannelDeviceName, nil)},
			opaqueConfig(t, resourceapi.AllocationConfigSourceClaim, DriverName, []string{"channel"}, channelConfig("cd-a")),
		)
	}
	self := status(0)
	for name, other := range map[string]resourceapi.ResourceClaimStatus{"same template": status(0), "other template": status(1)} {
		t.Run(name, func(t *testing.T) {
			checkpoint := checkpointWithClaims(map[string]PreparedClaim{
				"self-uid":  {CheckpointState: ClaimCheckpointStatePrepareCompleted, Status: self},
				"other-uid": {CheckpointState: ClaimCheckpointStatePrepareCompleted, Status: other},
			})

			require.NoError(t, state.unprepareDevices(context.Background(), "self-uid", &self, checkpoint))
		})
	}
}

// TestApplyChannelConfigDriverManagedAllocationModeAll covers claims requesting
// all channels: they only ever expose the IMEX domain of their own
// ComputeDomain, as no other ComputeDomain can use the node.
func TestApplyChannelConfigDriverManagedAllocationModeAll(t *testing.T) {
	setChannelReservationsGate(t, true)
	allChannels := channelConfig("cd-a")
	allChannels.AllocationMode = configapi.ComputeDomainChannelAllocationModeAll

	t.Run("shared by claims of the same ComputeDomain", func(t *testing.T) {
		state := reservationTestState(t, driverManagedConfig(), checkpointWithClaims(map[string]PreparedClaim{
			"first-uid": preparedChannelClaim("cd-a", 0),
		}))

		configState, err := applyChannelConfig(t, state, "second-uid", imex.ChannelDeviceName, allChannels, 0)

		require.NoError(t, err)
		assert.Len(t, configState.containerEdits.DeviceNodes, 4)
	})

	t.Run("rejected while another ComputeDomain holds the channel", func(t *testing.T) {
		state := reservationTestState(t, driverManagedConfig(), checkpointWithClaims(map[string]PreparedClaim{
			"first-uid": preparedChannelClaim("cd-b", 0),
		}))

		_, err := applyChannelConfig(t, state, "second-uid", imex.ChannelDeviceName, allChannels, 0)

		require.Error(t, err)
		assert.True(t, isPermanentError(err))
	})
}

func TestApplyChannelConfigDeletedComputeDomain(t *testing.T) {
	setChannelReservationsGate(t, true)
	state := reservationTestState(t, hostManagedConfig(), checkpointWithClaims(nil))

	_, err := applyChannelConfig(t, state, "claim-uid", imex.ChannelDeviceName, channelConfig("cd-deleted"), 0)

	require.Error(t, err, "a released channel is not prepared for its former ComputeDomain")
	assert.Contains(t, err.Error(), "ComputeDomain not found")
}
