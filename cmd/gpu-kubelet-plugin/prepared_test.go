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
	"testing"

	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/utils/ptr"
)

// Each kind of PreparedDevice keeps its kubelet-facing Device in a different
// field. The helpers below build one device of each kind so that tests spanning
// several kinds are not dominated by nested struct literals.

func newPreparedGpu(deviceName, uuid string) PreparedDevice {
	return PreparedDevice{
		Gpu: &PreparedGpu{
			Info:   &GpuInfo{UUID: uuid},
			Device: &CheckpointedDevice{DeviceName: deviceName},
		},
	}
}

func newPreparedMigDevice(deviceName, migUUID string) PreparedDevice {
	return PreparedDevice{
		Mig: &PreparedMigDevice{
			Concrete: &MigLiveTuple{MigUUID: migUUID},
			Device:   &CheckpointedDevice{DeviceName: deviceName},
		},
	}
}

func newPreparedVfioDevice(deviceName, uuid string) PreparedDevice {
	return PreparedDevice{
		Vfio: &PreparedVfioDevice{
			Info:   &VfioDeviceInfo{UUID: uuid},
			Device: &CheckpointedDevice{DeviceName: deviceName},
		},
	}
}

// newMixedPreparedDeviceList returns a list holding two devices of every kind,
// interleaved. The interleaving is deliberate: a filter has to skip over other
// kinds between two matches, and returning more than one device per kind keeps
// "return every match" distinguishable from "return the first match".
func newMixedPreparedDeviceList() PreparedDeviceList {
	return PreparedDeviceList{
		newPreparedGpu("gpu-0", "GPU-0000"),
		newPreparedMigDevice("mig-0", "MIG-0000"),
		newPreparedVfioDevice("vfio-0", "VFIO-0000"),
		newPreparedGpu("gpu-1", "GPU-0001"),
		newPreparedMigDevice("mig-1", "MIG-0001"),
		newPreparedVfioDevice("vfio-1", "VFIO-0001"),
	}
}

// newUnsortedMixedPreparedDeviceList is like newMixedPreparedDeviceList, but
// devices appear in descending UUID order so that a missing slices.Sort in
// the *UUIDs methods shows up as a mismatch.
func newUnsortedMixedPreparedDeviceList() PreparedDeviceList {
	return PreparedDeviceList{
		newPreparedGpu("gpu-1", "GPU-0001"),
		newPreparedMigDevice("mig-1", "MIG-0001"),
		newPreparedVfioDevice("vfio-1", "VFIO-0001"),
		newPreparedGpu("gpu-0", "GPU-0000"),
		newPreparedMigDevice("mig-0", "MIG-0000"),
		newPreparedVfioDevice("vfio-0", "VFIO-0000"),
	}
}

// requireCanonicalNames asserts on the canonical names of devices instead of on
// the devices themselves, so that a mismatch reports readable names rather than
// a slice of pointers.
func requireCanonicalNames(t *testing.T, want []string, devices PreparedDeviceList) {
	t.Helper()
	var got []string
	for _, device := range devices {
		got = append(got, device.CanonicalName())
	}
	require.Equal(t, want, got)
}

// requireDeviceNames asserts on the device names of the kubelet-facing devices,
// so that a mismatch reports which devices arrived in which order.
func requireDeviceNames(t *testing.T, want []string, devices []kubeletplugin.Device) {
	t.Helper()
	var got []string
	for _, device := range devices {
		got = append(got, device.DeviceName)
	}
	require.Equal(t, want, got)
}

func TestPreparedDeviceType(t *testing.T) {
	tests := map[string]struct {
		device PreparedDevice
		want   string
	}{
		"full GPU": {
			device: newPreparedGpu("gpu-0", "GPU-0000"),
			want:   GpuDeviceType,
		},
		"MIG device": {
			device: newPreparedMigDevice("mig-0", "MIG-0000"),
			want:   PreparedMigDeviceType,
		},
		"vfio device": {
			device: newPreparedVfioDevice("vfio-0", "VFIO-0000"),
			want:   VfioDeviceType,
		},
		// A device with none of the three kinds set is not produced by the
		// prepare flow, but it is what unmarshalling a checkpoint entry with
		// all device fields null yields. Type() must classify that as unknown
		// instead of falling through to one of the concrete kinds.
		"no kind set": {
			device: PreparedDevice{},
			want:   UnknownDeviceType,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.device.Type())
		})
	}
}

func TestPreparedDeviceCanonicalName(t *testing.T) {
	tests := map[string]struct {
		device    PreparedDevice
		want      string
		wantPanic bool
	}{
		"full GPU": {
			device: newPreparedGpu("gpu-0", "GPU-0000"),
			want:   "gpu-0",
		},
		"MIG device": {
			device: newPreparedMigDevice("mig-0", "MIG-0000"),
			want:   "mig-0",
		},
		"vfio device": {
			device: newPreparedVfioDevice("vfio-0", "VFIO-0000"),
			want:   "vfio-0",
		},
		"no kind set": {
			device:    PreparedDevice{},
			wantPanic: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if tc.wantPanic {
				require.Panics(t, func() { tc.device.CanonicalName() })
				return
			}
			require.Equal(t, tc.want, tc.device.CanonicalName())
		})
	}
}

func TestPreparedDeviceListGpus(t *testing.T) {
	tests := map[string]struct {
		devices   PreparedDeviceList
		wantNames []string
	}{
		"picks every GPU, in list order, from a mixed list": {
			devices:   newMixedPreparedDeviceList(),
			wantNames: []string{"gpu-0", "gpu-1"},
		},
		"returns nothing when no GPU is present": {
			devices: PreparedDeviceList{
				newPreparedMigDevice("mig-0", "MIG-0000"),
				newPreparedVfioDevice("vfio-0", "VFIO-0000"),
			},
			wantNames: nil,
		},
		"returns nothing for an empty list": {
			devices:   nil,
			wantNames: nil,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			requireCanonicalNames(t, tc.wantNames, tc.devices.Gpus())
		})
	}
}

func TestPreparedDeviceListMigDevices(t *testing.T) {
	tests := map[string]struct {
		devices   PreparedDeviceList
		wantNames []string
	}{
		"picks every MIG device, in list order, from a mixed list": {
			devices:   newMixedPreparedDeviceList(),
			wantNames: []string{"mig-0", "mig-1"},
		},
		"returns nothing when no MIG device is present": {
			devices: PreparedDeviceList{
				newPreparedGpu("gpu-0", "GPU-0000"),
				newPreparedVfioDevice("vfio-0", "VFIO-0000"),
			},
			wantNames: nil,
		},
		"returns nothing for an empty list": {
			devices:   nil,
			wantNames: nil,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			requireCanonicalNames(t, tc.wantNames, tc.devices.MigDevices())
		})
	}
}

func TestPreparedDeviceListVfioDevices(t *testing.T) {
	tests := map[string]struct {
		devices   PreparedDeviceList
		wantNames []string
	}{
		"picks every vfio device, in list order, from a mixed list": {
			devices:   newMixedPreparedDeviceList(),
			wantNames: []string{"vfio-0", "vfio-1"},
		},
		"returns nothing when no vfio device is present": {
			devices: PreparedDeviceList{
				newPreparedGpu("gpu-0", "GPU-0000"),
				newPreparedMigDevice("mig-0", "MIG-0000"),
			},
			wantNames: nil,
		},
		"returns nothing for an empty list": {
			devices:   nil,
			wantNames: nil,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			requireCanonicalNames(t, tc.wantNames, tc.devices.VfioDevices())
		})
	}
}

func TestPreparedDeviceGroupGetDevices(t *testing.T) {
	// Devices carrying every field that GetDevices has to pass through, so that
	// dropping one of them shows up as a mismatch.
	gpu := CheckpointedDevice{
		Requests:     []string{"req-gpu"},
		PoolName:     "pool-a",
		DeviceName:   "gpu-0",
		CDIDeviceIDs: []string{"nvidia.com/gpu=0"},
	}
	mig := CheckpointedDevice{
		Requests:     []string{"req-mig"},
		PoolName:     "pool-a",
		DeviceName:   "mig-0",
		CDIDeviceIDs: []string{"nvidia.com/gpu=1:0"},
	}
	vfio := CheckpointedDevice{
		Requests:     []string{"req-vfio"},
		PoolName:     "pool-a",
		DeviceName:   "gpu-vfio-0",
		CDIDeviceIDs: []string{"nvidia.com/pgpu=0"},
	}

	tests := map[string]struct {
		group PreparedDeviceGroup
		want  []kubeletplugin.Device
	}{
		"converts every kind and keeps list order": {
			group: PreparedDeviceGroup{
				Devices: PreparedDeviceList{
					{Gpu: &PreparedGpu{Device: &gpu}},
					{Mig: &PreparedMigDevice{Device: &mig}},
					{Vfio: &PreparedVfioDevice{Device: &vfio}},
				},
			},
			want: []kubeletplugin.Device{
				kubeletplugin.Device(gpu),
				kubeletplugin.Device(mig),
				kubeletplugin.Device(vfio),
			},
		},
		"drops devices that cannot be classified": {
			group: PreparedDeviceGroup{
				Devices: PreparedDeviceList{
					{Gpu: &PreparedGpu{Device: &gpu}},
					{},
					{Vfio: &PreparedVfioDevice{Device: &vfio}},
				},
			},
			want: []kubeletplugin.Device{
				kubeletplugin.Device(gpu),
				kubeletplugin.Device(vfio),
			},
		},
		"returns nothing for a group with no devices": {
			group: PreparedDeviceGroup{},
			want:  nil,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.group.GetDevices())
		})
	}
}

func TestPreparedDevicesGetDevices(t *testing.T) {
	tests := map[string]struct {
		devices   PreparedDevices
		wantNames []string
	}{
		"concatenates the devices of every group, in group order": {
			devices: PreparedDevices{
				{Devices: PreparedDeviceList{
					newPreparedGpu("gpu-0", "GPU-0000"),
					newPreparedMigDevice("mig-0", "MIG-0000"),
				}},
				{Devices: PreparedDeviceList{
					newPreparedVfioDevice("vfio-0", "VFIO-0000"),
				}},
			},
			wantNames: []string{"gpu-0", "mig-0", "vfio-0"},
		},
		"keeps the devices of later groups when an earlier group is empty": {
			devices: PreparedDevices{
				{Devices: nil},
				{Devices: PreparedDeviceList{
					newPreparedGpu("gpu-0", "GPU-0000"),
				}},
			},
			wantNames: []string{"gpu-0"},
		},
		"returns nothing when there are no groups": {
			devices:   nil,
			wantNames: nil,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			requireDeviceNames(t, tc.wantNames, tc.devices.GetDevices())
		})
	}
}

func TestPreparedDeviceGroupGetDeviceNames(t *testing.T) {
	tests := map[string]struct {
		group PreparedDeviceGroup
		want  []DeviceName
	}{
		"returns the canonical name of every device, in list order": {
			group: PreparedDeviceGroup{Devices: newMixedPreparedDeviceList()},
			want:  []DeviceName{"gpu-0", "mig-0", "vfio-0", "gpu-1", "mig-1", "vfio-1"},
		},
		"returns nothing for a group with no devices": {
			group: PreparedDeviceGroup{},
			want:  nil,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.group.GetDeviceNames())
		})
	}
}

func TestPreparedDevicesGetDeviceNames(t *testing.T) {
	tests := map[string]struct {
		devices PreparedDevices
		want    []DeviceName
	}{
		"concatenates the names of every group, in group order": {
			devices: PreparedDevices{
				{Devices: PreparedDeviceList{
					newPreparedGpu("gpu-0", "GPU-0000"),
					newPreparedMigDevice("mig-0", "MIG-0000"),
				}},
				{Devices: PreparedDeviceList{
					newPreparedVfioDevice("vfio-0", "VFIO-0000"),
				}},
			},
			want: []DeviceName{"gpu-0", "mig-0", "vfio-0"},
		},
		"returns nothing when there are no groups": {
			devices: nil,
			want:    nil,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.devices.GetDeviceNames())
		})
	}
}

func TestPreparedDeviceListUUIDs(t *testing.T) {
	tests := map[string]struct {
		uuids func() []string
		want  []string
	}{
		"GpuUUIDs returns GPU uuids only, sorted": {
			uuids: newUnsortedMixedPreparedDeviceList().GpuUUIDs,
			want:  []string{"GPU-0000", "GPU-0001"},
		},
		"MigDeviceUUIDs returns MIG uuids only, sorted": {
			uuids: newUnsortedMixedPreparedDeviceList().MigDeviceUUIDs,
			want:  []string{"MIG-0000", "MIG-0001"},
		},
		"VfioDeviceUUIDs returns VFIO uuids only, sorted": {
			uuids: newUnsortedMixedPreparedDeviceList().VfioDeviceUUIDs,
			want:  []string{"VFIO-0000", "VFIO-0001"},
		},
		"UUIDs returns every kind, sorted": {
			uuids: newUnsortedMixedPreparedDeviceList().UUIDs,
			want:  []string{"GPU-0000", "GPU-0001", "MIG-0000", "MIG-0001", "VFIO-0000", "VFIO-0001"},
		},
		"returns nothing for an empty list": {
			uuids: PreparedDeviceList(nil).UUIDs,
			want:  nil,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.uuids())
		})
	}
}

func TestPreparedDeviceGroupUUIDs(t *testing.T) {
	group := PreparedDeviceGroup{Devices: newUnsortedMixedPreparedDeviceList()}

	tests := map[string]struct {
		uuids func() []string
		want  []string
	}{
		"GpuUUIDs returns GPU uuids only, sorted": {
			uuids: group.GpuUUIDs,
			want:  []string{"GPU-0000", "GPU-0001"},
		},
		"MigDeviceUUIDs returns MIG uuids only, sorted": {
			uuids: group.MigDeviceUUIDs,
			want:  []string{"MIG-0000", "MIG-0001"},
		},
		"VfioDeviceUUIDs returns VFIO uuids only, sorted": {
			uuids: group.VfioDeviceUUIDs,
			want:  []string{"VFIO-0000", "VFIO-0001"},
		},
		"UUIDs returns every kind, sorted": {
			uuids: group.UUIDs,
			want:  []string{"GPU-0000", "GPU-0001", "MIG-0000", "MIG-0001", "VFIO-0000", "VFIO-0001"},
		},
		"returns nothing for a group with no devices": {
			uuids: (&PreparedDeviceGroup{}).UUIDs,
			want:  nil,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.uuids())
		})
	}
}

func TestPreparedDevicesUUIDs(t *testing.T) {
	// Two groups, so that a bug returning only the first group's uuids shows up.
	devices := PreparedDevices{
		{Devices: PreparedDeviceList{
			newPreparedGpu("gpu-1", "GPU-0001"),
			newPreparedMigDevice("mig-1", "MIG-0001"),
			newPreparedVfioDevice("vfio-1", "VFIO-0001"),
		}},
		{Devices: PreparedDeviceList{
			newPreparedGpu("gpu-0", "GPU-0000"),
			newPreparedMigDevice("mig-0", "MIG-0000"),
			newPreparedVfioDevice("vfio-0", "VFIO-0000"),
		}},
	}
	tests := map[string]struct {
		uuids func() []string
		want  []string
	}{
		"GpuUUIDs collects GPU uuids from every group, sorted": {
			uuids: devices.GpuUUIDs,
			want:  []string{"GPU-0000", "GPU-0001"},
		},
		"MigDeviceUUIDs collects MIG uuids from every group, sorted": {
			uuids: devices.MigDeviceUUIDs,
			want:  []string{"MIG-0000", "MIG-0001"},
		},
		"VfioDeviceUUIDs collects vfio uuids from every group, sorted": {
			uuids: devices.VfioDeviceUUIDs,
			want:  []string{"VFIO-0000", "VFIO-0001"},
		},
		"UUIDs returns every kind, sorted": {
			uuids: devices.UUIDs,
			want:  []string{"GPU-0000", "GPU-0001", "MIG-0000", "MIG-0001", "VFIO-0000", "VFIO-0001"},
		},
		"returns nothing when there are no groups": {
			uuids: PreparedDevices(nil).UUIDs,
			want:  nil,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.uuids())
		})
	}
}

func TestPreparedClaimGetNonAdminDevices(t *testing.T) {
	tests := map[string]struct {
		claim PreparedClaim
		want  map[string]struct{}
	}{
		"keeps only this driver's devices that were requested without admin access": {
			claim: PreparedClaim{
				Status: resourceapi.ResourceClaimStatus{
					Allocation: &resourceapi.AllocationResult{
						Devices: resourceapi.DeviceAllocationResult{
							Results: []resourceapi.DeviceRequestAllocationResult{
								{Driver: DriverName, Device: "gpu-0"},
								{Driver: "other.example.com", Device: "nic-0"},
								{Driver: DriverName, Device: "gpu-1", AdminAccess: ptr.To(true)},
								{Driver: DriverName, Device: "gpu-2", AdminAccess: ptr.To(false)},
							},
						},
					},
				},
			},
			want: map[string]struct{}{"gpu-0": {}, "gpu-2": {}},
		},
		"returns an empty set when no result belongs to this driver": {
			claim: PreparedClaim{
				Status: resourceapi.ResourceClaimStatus{
					Allocation: &resourceapi.AllocationResult{
						Devices: resourceapi.DeviceAllocationResult{
							Results: []resourceapi.DeviceRequestAllocationResult{
								{Driver: "other.example.com", Device: "nic-0"},
							},
						},
					},
				},
			},
			want: map[string]struct{}{},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.claim.GetNonAdminDevices())
		})
	}
}
