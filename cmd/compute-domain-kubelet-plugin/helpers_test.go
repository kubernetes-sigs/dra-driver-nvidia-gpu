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
	"os"
	"path/filepath"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAllocatableDeviceAccessors(t *testing.T) {
	t.Run("channel device", func(t *testing.T) {
		device := &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: 3}}

		assert.Equal(t, ComputeDomainChannelType, device.Type())
		assert.Equal(t, "channel-3", device.CanonicalName())
		assert.Equal(t, "3", device.CanonicalIndex())
		assert.Equal(t, "channel-3", device.GetDevice().Name)
	})

	t.Run("daemon device", func(t *testing.T) {
		device := &AllocatableDevice{Daemon: &ComputeDomainDaemonInfo{ID: 7}}

		assert.Equal(t, ComputeDomainDaemonType, device.Type())
		assert.Equal(t, "daemon-7", device.CanonicalName())
		assert.Equal(t, "7", device.CanonicalIndex())
		assert.Equal(t, "daemon-7", device.GetDevice().Name)
	})

	t.Run("unknown device type", func(t *testing.T) {
		device := &AllocatableDevice{}
		assert.Equal(t, UnknownDeviceType, device.Type())
	})
}

func TestPreparedDeviceHelpers(t *testing.T) {
	channelDevice := CheckpointedDevice(kubeletplugin.Device{DeviceName: "channel-device"})
	daemonDevice := CheckpointedDevice(kubeletplugin.Device{DeviceName: "daemon-device"})

	channel := PreparedDevice{
		Channel: &PreparedComputeDomainChannel{
			Info:   &ComputeDomainChannelInfo{ID: 1},
			Device: &channelDevice,
		},
	}
	daemon := PreparedDevice{
		Daemon: &PreparedComputeDomainDaemon{
			Info:   &ComputeDomainDaemonInfo{ID: 2},
			Device: &daemonDevice,
		},
	}

	devices := PreparedDeviceList{channel, daemon, {}}

	assert.Equal(t, PreparedDeviceList{channel}, devices.ComputeDomainChannels())
	assert.Equal(t, PreparedDeviceList{daemon}, devices.ComputeDomainDaemons())

	group := &PreparedDeviceGroup{Devices: devices}
	assert.Equal(t,
		[]kubeletplugin.Device{
			kubeletplugin.Device(channelDevice),
			kubeletplugin.Device(daemonDevice),
		},
		group.GetDevices(),
	)

	prepared := PreparedDevices{group}
	assert.Equal(t, group.GetDevices(), prepared.GetDevices())
}

func TestPreparedClaimGetNonAdminDevices(t *testing.T) {
	t.Run("nil allocation returns empty set", func(t *testing.T) {
		claim := &PreparedClaim{}
		assert.Empty(t, claim.GetNonAdminDevices())
	})

	t.Run("filters to this driver and non-admin devices", func(t *testing.T) {
		admin := true
		claim := &PreparedClaim{
			Status: resourceapi.ResourceClaimStatus{
				Allocation: &resourceapi.AllocationResult{
					Devices: resourceapi.DeviceAllocationResult{
						Results: []resourceapi.DeviceRequestAllocationResult{
							{Driver: DriverName, Device: "keep-default"},
							{Driver: DriverName, Device: "keep-false", AdminAccess: new(bool)},
							{Driver: DriverName, Device: "drop-admin", AdminAccess: &admin},
							{Driver: "other.example.com", Device: "drop-other-driver"},
						},
					},
				},
			},
		}

		assert.Equal(t, map[string]struct{}{
			"keep-default": {},
			"keep-false":   {},
		}, claim.GetNonAdminDevices())
	})
}

func TestCheckpointConversions(t *testing.T) {
	t.Run("to latest initializes empty v2 claims", func(t *testing.T) {
		cp := (&Checkpoint{}).ToLatestVersion()

		require.NotNil(t, cp.V2)
		assert.NotNil(t, cp.V2.PreparedClaims)
		assert.Empty(t, cp.V2.PreparedClaims)
	})

	t.Run("to latest upgrades v1 claims to completed state", func(t *testing.T) {
		cp := (&Checkpoint{
			V1: &CheckpointV1{
				PreparedClaims: PreparedClaimsByUIDV1{
					"claim-uid": {},
				},
			},
		}).ToLatestVersion()

		require.Contains(t, cp.V2.PreparedClaims, "claim-uid")
		assert.Equal(t, ClaimCheckpointStatePrepareCompleted, cp.V2.PreparedClaims["claim-uid"].CheckpointState)
	})

	t.Run("to v1 drops non-completed claims", func(t *testing.T) {
		v1 := (&CheckpointV2{
			PreparedClaims: PreparedClaimsByUIDV2{
				"completed": {CheckpointState: ClaimCheckpointStatePrepareCompleted},
				"aborted":   {CheckpointState: ClaimCheckpointStatePrepareAborted},
			},
		}).ToV1()

		require.Contains(t, v1.PreparedClaims, "completed")
		assert.NotContains(t, v1.PreparedClaims, "aborted")
	})
}

func TestRootHelpers(t *testing.T) {
	tmpDir := t.TempDir()
	driverRoot := root(tmpDir)

	require.NoError(t, os.Mkdir(filepath.Join(tmpDir, "dev"), 0o755))
	assert.True(t, driverRoot.isDevRoot())
	assert.Equal(t, tmpDir, driverRoot.getDevRoot())
	assert.Equal(t, "/", root(filepath.Join(tmpDir, "missing")).getDevRoot())

	targetDir := filepath.Join(tmpDir, "usr", "bin")
	require.NoError(t, os.MkdirAll(targetDir, 0o755))

	targetFile := filepath.Join(targetDir, "nvidia-smi.real")
	require.NoError(t, os.WriteFile(targetFile, []byte("test"), 0o644))
	require.NoError(t, os.Symlink("nvidia-smi.real", filepath.Join(targetDir, "nvidia-smi")))

	found, err := driverRoot.findFile("nvidia-smi", "/usr/bin")
	require.NoError(t, err)
	assert.Equal(t, targetFile, found)

	_, err = driverRoot.findFile("does-not-exist", "/usr/bin")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `error locating "does-not-exist"`)
}
