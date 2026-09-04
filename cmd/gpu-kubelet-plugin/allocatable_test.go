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
	"testing"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// setDynamicMIG saves and restores the process-wide DynamicMIG gate; not parallel-safe.
func setDynamicMIG(t *testing.T, enabled bool) {
	t.Helper()
	original := featuregates.Enabled(featuregates.DynamicMIG)
	require.NoError(t, featuregates.FeatureGates().SetFromMap(
		map[string]bool{string(featuregates.DynamicMIG): enabled}))
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(
			map[string]bool{string(featuregates.DynamicMIG): original}))
	})
}

func newMigSpec(parentMinor, profileID, placementStart int, pci string) *MigSpec {
	return &MigSpec{
		Parent:        &GpuInfo{minor: parentMinor, pciBusID: pci},
		Profile:       &nvdev.MigProfileInfo{C: 1, G: 1, GB: 5},
		GIProfileInfo: nvml.GpuInstanceProfileInfo{Id: uint32(profileID)},
		Placement:     nvml.GpuInstancePlacement{Start: uint32(placementStart)},
	}
}

func TestAllocatableDeviceType(t *testing.T) {
	tests := map[string]struct {
		dev     AllocatableDevice
		want    string
		wantMig bool
	}{
		"gpu":         {AllocatableDevice{Gpu: &GpuInfo{}}, GpuDeviceType, false},
		"mig-dynamic": {AllocatableDevice{MigDynamic: &MigSpec{}}, MigDynamicDeviceType, true},
		"mig-static":  {AllocatableDevice{MigStatic: &MigDeviceInfo{}}, MigStaticDeviceType, true},
		"vfio":        {AllocatableDevice{Vfio: &VfioDeviceInfo{}}, VfioDeviceType, false},
		"none":        {AllocatableDevice{}, UnknownDeviceType, false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.dev.Type())
			assert.Equal(t, tc.wantMig, tc.dev.IsStaticOrDynMigDevice())
		})
	}
}

func TestCanonicalName(t *testing.T) {
	t.Run("gpu", func(t *testing.T) {
		d := &AllocatableDevice{Gpu: &GpuInfo{minor: 3}}
		assert.Equal(t, "gpu-3", d.CanonicalName())
	})

	// Static and dynamic specs must name the identical configuration.
	t.Run("mig-static and mig-dynamic agree", func(t *testing.T) {
		static := &AllocatableDevice{MigStatic: &MigDeviceInfo{
			Profile: "1g.5gb", ParentMinor: 2, GiProfileID: 19, PlacementStart: 0,
		}}
		dynamic := &AllocatableDevice{MigDynamic: newMigSpec(2, 19, 0, "0000:65:00.0")}
		assert.Equal(t, "gpu-2-mig-1g5gb-19-0", static.CanonicalName())
		assert.Equal(t, "gpu-2-mig-1g5gb-19-0", dynamic.CanonicalName())
	})

	t.Run("vfio", func(t *testing.T) {
		d := &AllocatableDevice{Vfio: &VfioDeviceInfo{index: 7}}
		assert.Equal(t, "gpu-vfio-7", d.CanonicalName())
	})

	t.Run("unknown panics", func(t *testing.T) {
		require.Panics(t, func() { _ = (&AllocatableDevice{}).CanonicalName() })
	})
}

func TestUUID(t *testing.T) {
	t.Run("gpu", func(t *testing.T) {
		assert.Equal(t, "GPU-abc", AllocatableDevice{Gpu: &GpuInfo{UUID: "GPU-abc"}}.UUID())
	})
	t.Run("mig-static", func(t *testing.T) {
		assert.Equal(t, "MIG-def", AllocatableDevice{MigStatic: &MigDeviceInfo{UUID: "MIG-def"}}.UUID())
	})
	t.Run("vfio", func(t *testing.T) {
		assert.Equal(t, "GPU-vfio", AllocatableDevice{Vfio: &VfioDeviceInfo{UUID: "GPU-vfio"}}.UUID())
	})

	// MigDynamic has no UUID yet; unknown has no device.
	t.Run("mig-dynamic panics", func(t *testing.T) {
		require.Panics(t, func() { _ = AllocatableDevice{MigDynamic: &MigSpec{}}.UUID() })
	})
	t.Run("unknown panics", func(t *testing.T) {
		require.Panics(t, func() { _ = AllocatableDevice{}.UUID() })
	})
}

func TestGetGPUPCIBusID(t *testing.T) {
	tests := map[string]struct {
		dev  AllocatableDevice
		want string
	}{
		"gpu":         {AllocatableDevice{Gpu: &GpuInfo{pciBusID: "0000:01:00.0"}}, "0000:01:00.0"},
		"mig-static":  {AllocatableDevice{MigStatic: &MigDeviceInfo{parent: &GpuInfo{pciBusID: "0000:02:00.0"}}}, "0000:02:00.0"},
		"mig-dynamic": {AllocatableDevice{MigDynamic: &MigSpec{Parent: &GpuInfo{pciBusID: "0000:03:00.0"}}}, "0000:03:00.0"},
		"vfio":        {AllocatableDevice{Vfio: &VfioDeviceInfo{PciBusID: "0000:04:00.0"}}, "0000:04:00.0"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.dev.GetGPUPCIBusID())
		})
	}
	t.Run("unknown panics", func(t *testing.T) {
		require.Panics(t, func() { _ = (&AllocatableDevice{}).GetGPUPCIBusID() })
	})
}

func TestGetDevice(t *testing.T) {
	// Empty Config leaves AllowMultipleAllocations unset.
	t.Run("vfio", func(t *testing.T) {
		d := &AllocatableDevice{Vfio: &VfioDeviceInfo{
			index: 5, UUID: "GPU-vfio", deviceID: "0x1234", vendorID: "0x10de",
		}}
		dev := d.GetDevice(&Config{})
		assert.Equal(t, "gpu-vfio-5", dev.Name)
		assert.Nil(t, dev.AllowMultipleAllocations)
	})

	t.Run("gpu", func(t *testing.T) {
		// Version fields must be valid semver.
		d := &AllocatableDevice{Gpu: &GpuInfo{
			minor: 1, UUID: "GPU-1",
			cudaComputeCapability: "8.0.0", driverVersion: "550.54.15", cudaDriverVersion: "12.4.0",
		}}
		dev := d.GetDevice(&Config{})
		assert.Equal(t, "gpu-1", dev.Name)
		assert.Nil(t, dev.AllowMultipleAllocations)
	})

	t.Run("mig-static", func(t *testing.T) {
		d := &AllocatableDevice{MigStatic: &MigDeviceInfo{
			UUID: "MIG-1", Profile: "1g.5gb", ParentMinor: 2, GiProfileID: 19, PlacementStart: 0,
			parent: &GpuInfo{
				cudaComputeCapability: "8.0.0", driverVersion: "550.54.15", cudaDriverVersion: "12.4.0",
			},
			giProfileInfo: &nvml.GpuInstanceProfileInfo{},
		}}
		dev := d.GetDevice(&Config{})
		assert.Equal(t, "gpu-2-mig-1g5gb-19-0", dev.Name)
		assert.Nil(t, dev.AllowMultipleAllocations)
	})

	t.Run("mig-dynamic panics", func(t *testing.T) {
		require.Panics(t, func() { _ = (&AllocatableDevice{MigDynamic: &MigSpec{}}).GetDevice(&Config{}) })
	})
	t.Run("unknown panics", func(t *testing.T) {
		require.Panics(t, func() { _ = (&AllocatableDevice{}).GetDevice(&Config{}) })
	})
}

func buildMixedDevices() AllocatableDevices {
	return AllocatableDevices{
		"gpu-1":  {Gpu: &GpuInfo{minor: 1, UUID: "GPU-bbb"}},
		"gpu-0":  {Gpu: &GpuInfo{minor: 0, UUID: "GPU-aaa"}},
		"vfio-1": {Vfio: &VfioDeviceInfo{index: 1, UUID: "GPU-vfio-yyy"}},
		"vfio-0": {Vfio: &VfioDeviceInfo{index: 0, UUID: "GPU-vfio-xxx"}},
		"mig-0":  {MigStatic: &MigDeviceInfo{UUID: "MIG-mmm", Profile: "1g.5gb", ParentMinor: 0, GiProfileID: 19}},
		"mig-d":  {MigDynamic: newMigSpec(3, 19, 0, "0000:66:00.0")},
	}
}

func TestAllocatableDevicesFilters(t *testing.T) {
	devices := buildMixedDevices()

	gpus := devices.GetGPUs()
	require.Len(t, gpus, 2)
	for _, g := range gpus {
		assert.Equal(t, GpuDeviceType, g.Type())
	}

	vfios := devices.GetVfioDevices()
	require.Len(t, vfios, 2)
	for _, v := range vfios {
		assert.Equal(t, VfioDeviceType, v.Type())
	}

	migStatics := devices.GetMigStaticDevices()
	require.Len(t, migStatics, 1)
	assert.Equal(t, MigStaticDeviceType, migStatics[0].Type())

	migDynamics := devices.GetMigDynamicDevices()
	require.Len(t, migDynamics, 1)
	assert.Equal(t, MigDynamicDeviceType, migDynamics[0].Type())

	want := make([]*AllocatableDevice, 0, len(devices))
	for _, d := range devices {
		want = append(want, d)
	}
	assert.ElementsMatch(t, want, devices.List())

	assert.Equal(t, []string{"GPU-aaa", "GPU-bbb"}, devices.GpuUUIDs())
	assert.Equal(t, []string{"GPU-vfio-xxx", "GPU-vfio-yyy"}, devices.VfioDeviceUUIDs())
}

func TestUUIDsSortedAcrossTypes(t *testing.T) {
	// UUIDs() panics on MigDynamic/unknown, so use only UUID-bearing types.
	devices := AllocatableDevices{
		"gpu-1": {Gpu: &GpuInfo{UUID: "GPU-bbb"}},
		"gpu-0": {Gpu: &GpuInfo{UUID: "GPU-aaa"}},
		"mig-0": {MigStatic: &MigDeviceInfo{UUID: "MIG-mmm"}},
	}
	assert.Equal(t, []string{"GPU-aaa", "GPU-bbb", "MIG-mmm"}, devices.UUIDs())
}

func TestMigDeviceUUIDs(t *testing.T) {
	t.Run("static only, sorted", func(t *testing.T) {
		setDynamicMIG(t, false)
		devices := AllocatableDevices{
			"gpu-0": {Gpu: &GpuInfo{UUID: "GPU-excluded"}},
			"mig-1": {MigStatic: &MigDeviceInfo{UUID: "MIG-bbb"}},
			"mig-0": {MigStatic: &MigDeviceInfo{UUID: "MIG-aaa"}},
			"vfio":  {Vfio: &VfioDeviceInfo{UUID: "GPU-vfio-excluded"}},
		}
		assert.Equal(t, []string{"MIG-aaa", "MIG-bbb"}, devices.MigDeviceUUIDs())
	})

	t.Run("panics when DynamicMIG enabled", func(t *testing.T) {
		setDynamicMIG(t, true)
		devices := AllocatableDevices{"mig-0": {MigStatic: &MigDeviceInfo{UUID: "MIG-aaa"}}}
		require.Panics(t, func() { _ = devices.MigDeviceUUIDs() })
	})
}

func newPerGPU() *PerGPUAllocatableDevices {
	// No constructor; the map must be initialized explicitly.
	return &PerGPUAllocatableDevices{allocatablesMap: make(map[PCIBusID]AllocatableDevices)}
}

func TestPerGPUAddGPUAllocatables(t *testing.T) {
	p := newPerGPU()

	require.Error(t, p.AddGPUAllocatables("0000:01:00.0", nil), "nil allocatables must error")

	gpu := &AllocatableDevice{Gpu: &GpuInfo{minor: 0, UUID: "GPU-a", pciBusID: "0000:01:00.0"}}
	require.NoError(t, p.AddGPUAllocatables("0000:01:00.0", AllocatableDevices{"gpu-0": gpu}))

	got := p.GetGPUDeviceByPCIBusID("0000:01:00.0")
	require.NotNil(t, got)
	assert.Equal(t, "GPU-a", got.UUID())

	assert.Nil(t, p.GetGPUDeviceByPCIBusID("0000:99:00.0"), "unknown bus id returns nil")

	require.NoError(t, p.AddGPUAllocatables("0000:02:00.0",
		AllocatableDevices{"vfio-0": {Vfio: &VfioDeviceInfo{index: 0, PciBusID: "0000:02:00.0"}}}))
	assert.Nil(t, p.GetGPUDeviceByPCIBusID("0000:02:00.0"))
}

func TestPerGPUAddAllocatableDevice(t *testing.T) {
	p := newPerGPU()

	require.Error(t, p.AddAllocatableDevice(nil), "nil device must error")

	gpu := &AllocatableDevice{Gpu: &GpuInfo{minor: 4, UUID: "GPU-d", pciBusID: "0000:05:00.0"}}
	require.NoError(t, p.AddAllocatableDevice(gpu))

	// Keyed by CanonicalName ("gpu-4").
	got := p.GetAllocatableDevice("gpu-4")
	require.NotNil(t, got)
	assert.Equal(t, "GPU-d", got.UUID())
	assert.Nil(t, p.GetAllocatableDevice("does-not-exist"))
}

func TestPerGPUGetAllDevicesMerges(t *testing.T) {
	p := newPerGPU()
	require.NoError(t, p.AddAllocatableDevice(
		&AllocatableDevice{Gpu: &GpuInfo{minor: 0, UUID: "GPU-0", pciBusID: "0000:01:00.0"}}))
	require.NoError(t, p.AddAllocatableDevice(
		&AllocatableDevice{Gpu: &GpuInfo{minor: 1, UUID: "GPU-1", pciBusID: "0000:02:00.0"}}))
	require.NoError(t, p.AddAllocatableDevice(
		&AllocatableDevice{Vfio: &VfioDeviceInfo{index: 0, UUID: "VF-0", PciBusID: "0000:02:00.0"}}))

	all := p.GetAllDevices()
	require.Len(t, all, 3)
	assert.Contains(t, all, "gpu-0")
	assert.Contains(t, all, "gpu-1")
	assert.Contains(t, all, "gpu-vfio-0")
}

func TestPerGPURemoveSiblingDevices(t *testing.T) {
	pci := "0000:01:00.0"
	gpu := &AllocatableDevice{Gpu: &GpuInfo{minor: 0, UUID: "GPU-0", pciBusID: pci}}
	vfio := &AllocatableDevice{Vfio: &VfioDeviceInfo{index: 0, UUID: "VF-0", PciBusID: pci}}

	newBucket := func(t *testing.T) *PerGPUAllocatableDevices {
		p := newPerGPU()
		require.NoError(t, p.AddAllocatableDevice(gpu))
		require.NoError(t, p.AddAllocatableDevice(vfio))
		require.Len(t, p.GetAllDevices(), 2)
		return p
	}

	t.Run("gpu selected removes vfio sibling", func(t *testing.T) {
		p := newBucket(t)
		p.RemoveSiblingDevices(gpu)
		all := p.GetAllDevices()
		require.Len(t, all, 1)
		assert.Contains(t, all, "gpu-0")
		assert.NotContains(t, all, "gpu-vfio-0")
	})

	t.Run("vfio selected removes gpu sibling", func(t *testing.T) {
		p := newBucket(t)
		p.RemoveSiblingDevices(vfio)
		all := p.GetAllDevices()
		require.Len(t, all, 1)
		assert.Contains(t, all, "gpu-vfio-0")
		assert.NotContains(t, all, "gpu-0")
	})
}

func TestTaintsReturnsClone(t *testing.T) {
	d := &AllocatableDevice{
		taints: []resourceapi.DeviceTaint{
			{Key: "xid", Value: "48", Effect: resourceapi.DeviceTaintEffectNoSchedule},
		},
	}

	got := d.Taints()
	require.Len(t, got, 1)

	// Mutating the result must not affect internal state.
	got[0].Value = "mutated"
	assert.Equal(t, "48", d.taints[0].Value)
	assert.Len(t, d.taints, 1)

	assert.Empty(t, (&AllocatableDevice{}).Taints())
}

func TestRemoveTaint(t *testing.T) {
	d := &AllocatableDevice{taints: []resourceapi.DeviceTaint{
		{Key: "xid", Value: "48"},
		{Key: "ecc", Value: "1"},
	}}

	removed, ok := d.RemoveTaint("xid")
	assert.True(t, ok)
	assert.Equal(t, "48", removed.Value)
	require.Len(t, d.taints, 1)
	assert.Equal(t, "ecc", d.taints[0].Key, "only the matching taint is removed")

	_, ok = d.RemoveTaint("absent")
	assert.False(t, ok)
	require.Len(t, d.taints, 1)
}
