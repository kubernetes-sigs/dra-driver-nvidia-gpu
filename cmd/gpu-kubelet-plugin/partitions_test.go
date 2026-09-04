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

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"
)

// newPartTestGpu adds the fields the partition paths read to the shared
// newTestGpuInfo fixture.
func newPartTestGpu(maxCapacities PartCapacityMap, memSliceCount int) *GpuInfo {
	gpu := newTestGpuInfo(newScalarNumaNodeAttribute(0))
	gpu.memoryBytes = ptr.To(uint64(4096))
	gpu.maxCapacities = maxCapacities
	gpu.memSliceCount = memSliceCount
	return gpu
}

// newPartTestMigSpec builds a 1g.10gb partition placed at memory slice `start`.
// nvdev.MigProfileInfo is a plain struct that already satisfies the
// nvdev.MigProfile interface, so no fake is needed.
func newPartTestMigSpec(parent *GpuInfo, start, size uint32) *MigSpec {
	return &MigSpec{
		Parent:  parent,
		Profile: &nvdev.MigProfileInfo{C: 1, G: 1, GB: 10, GIProfileID: 19},
		GIProfileInfo: nvml.GpuInstanceProfileInfo{
			Id: 19, MultiprocessorCount: 28, CopyEngineCount: 1,
			DecoderCount: 1, JpegCount: 1, MemorySizeMB: 10240,
		},
		Placement: nvml.GpuInstancePlacement{Start: start, Size: size},
	}
}

// resource.Quantity caches a format string, so require.Equal on two logically
// equal quantities can fail. Compare the int64 values instead.
func capValue(t *testing.T, m PartCapacityMap, name resourceapi.QualifiedName) int64 {
	t.Helper()
	c, ok := m[name]
	require.True(t, ok, "capacity %q missing from %v", name, m)
	return c.Value.Value()
}

func counterValue(t *testing.T, m map[string]resourceapi.Counter, name string) int64 {
	t.Helper()
	c, ok := m[name]
	require.True(t, ok, "counter %q missing from %v", name, m)
	return c.Value.Value()
}

func TestPartCapacities(t *testing.T) {
	t.Run("maxCapacities wins over full GPU memory", func(t *testing.T) {
		gpu := newPartTestGpu(PartCapacityMap{"multiprocessors": intcap(132)}, 8)

		caps := gpu.PartCapacities()

		require.Equal(t, int64(132), capValue(t, caps, "multiprocessors"))
		require.NotContains(t, caps, resourceapi.QualifiedName("memory"))
	})

	// Ampere with MIG disabled, or a vGPU guest: no MIG profiles were
	// inspected, so fall back to advertising total memory, matching the
	// legacy non-DynamicMIG GetDevice() path.
	t.Run("falls back to full GPU memory", func(t *testing.T) {
		gpu := newPartTestGpu(nil, 0)

		require.Equal(t, int64(4096), capValue(t, gpu.PartCapacities(), "memory"))
	})

	t.Run("nil when memory size is unknown too", func(t *testing.T) {
		gpu := newPartTestGpu(nil, 0)
		gpu.memoryBytes = nil

		require.Nil(t, gpu.PartCapacities())
	})
}

func TestPartGpuCounters(t *testing.T) {
	// A GPU with no MIG profile data has no partitions, so a per-GPU
	// CounterSet would have no consumers and the API would reject the
	// ResourceSlice: spec.sharedCounters[0].counters: Required value.
	t.Run("nil when no MIG profile data was collected", func(t *testing.T) {
		gpu := newPartTestGpu(nil, 0)

		require.Nil(t, gpu.PartSharedCounterSets())
		require.Nil(t, gpu.PartConsumesCounters())
	})

	t.Run("one set of capacity and memory-slice counters", func(t *testing.T) {
		gpu := newPartTestGpu(PartCapacityMap{
			"multiprocessors": intcap(132),
			"copyEngines":     intcap(8),
		}, 2)

		sets := gpu.PartSharedCounterSets()
		require.Len(t, sets, 1)
		require.Equal(t, "gpu-0-counter-set", sets[0].Name)

		shared := sets[0].Counters
		require.Len(t, shared, 4)
		require.Equal(t, int64(132), counterValue(t, shared, "multiprocessors"))
		// Counter names are derived from the capacity names.
		require.Equal(t, int64(8), counterValue(t, shared, "copy-engines"))
		require.Equal(t, int64(1), counterValue(t, shared, "memory-slice-0"))
		require.Equal(t, int64(1), counterValue(t, shared, "memory-slice-1"))

		// The full GPU consumes its own set entirely, so allocating it drops
		// every counter to zero. The two paths are published separately and
		// would otherwise drift.
		cc := gpu.PartConsumesCounters()
		require.Len(t, cc, 1)
		require.Equal(t, sets[0].Name, cc[0].CounterSet)
		require.Len(t, cc[0].Counters, len(shared))
		for name, c := range shared {
			require.Equal(t, c.Value.Value(), counterValue(t, cc[0].Counters, name), name)
		}
	})
}

func TestPartMigSpecGetDevice(t *testing.T) {
	parent := newPartTestGpu(PartCapacityMap{"multiprocessors": intcap(132)}, 8)
	// A partition occupying memory slices 2 and 3.
	dev := newPartTestMigSpec(parent, 2, 2).PartGetDevice()

	// gpu-<parentMinor>-mig-<profile>-<profileID>-<placementStart>
	require.Equal(t, "gpu-0-mig-1g10gb-19-2", dev.Name)
	// Static and dynamic MIG are deliberately indistinguishable in the API
	// (see types.go); both announce type "mig".
	require.Equal(t, "mig", *dev.Attributes["type"].StringValue)
	require.Equal(t, parent.UUID, *dev.Attributes["parentUUID"].StringValue)
	require.Equal(t, "1g.10gb", *dev.Attributes["profile"].StringValue)

	// MemorySizeMB is MiB per nvml.h, so 10240 MiB is 10 GiB.
	require.Equal(t, int64(28), capValue(t, dev.Capacity, "multiprocessors"))
	require.Equal(t, int64(10*1024*1024*1024), capValue(t, dev.Capacity, "memory"))
	// Capacity must not encode placement; slices belong in consumesCounters.
	require.NotContains(t, dev.Capacity, resourceapi.QualifiedName("memorySlice2"))

	require.Len(t, dev.ConsumesCounters, 1)
	// The partition consumes from its parent GPU's counter set, by name.
	require.Equal(t, parent.GetSharedCounterSetName(), dev.ConsumesCounters[0].CounterSet)

	counters := dev.ConsumesCounters[0].Counters
	require.Equal(t, int64(28), counterValue(t, counters, "multiprocessors"))
	// Only the slices this partition is placed on, so that a partition at a
	// different offset cannot claim the same physical memory.
	for _, i := range []int{2, 3} {
		require.Equal(t, int64(1), counterValue(t, counters, memsliceCounterName(i)))
	}
	for _, i := range []int{0, 1, 4} {
		require.NotContains(t, counters, memsliceCounterName(i))
	}
}

func TestPartGpuInfoGetDevice(t *testing.T) {
	gpu := newPartTestGpu(PartCapacityMap{"multiprocessors": intcap(132)}, 2)

	dev := gpu.PartGetDevice()

	require.Equal(t, "gpu-0", dev.Name)
	require.Equal(t, GpuDeviceType, *dev.Attributes["type"].StringValue)
	require.Equal(t, int64(132), capValue(t, dev.Capacity, "multiprocessors"))
	require.Len(t, dev.ConsumesCounters, 1)
	require.Equal(t, "gpu-0-counter-set", dev.ConsumesCounters[0].CounterSet)
	// pcieRoot is not discoverable in all environments, so it is advertised
	// only once discovered.
	require.NotContains(t, dev.Attributes, deviceattribute.StandardDeviceAttributePCIeRoot)

	gpu.pcieRootAttr = &deviceattribute.DeviceAttribute{
		Name:  deviceattribute.StandardDeviceAttributePCIeRoot,
		Value: resourceapi.DeviceAttribute{StringValue: ptr.To("pci0000:0d")},
	}

	attrs := gpu.PartGetDevice().Attributes
	require.Equal(t, "pci0000:0d",
		*attrs[deviceattribute.StandardDeviceAttributePCIeRoot].StringValue)
}

func TestPartAllocatableDeviceGetDevice(t *testing.T) {
	// A nil config keeps applyConsumableShares a no-op (see
	// isConsumableSharesEnabled).
	t.Run("dispatches on device type", func(t *testing.T) {
		gpu := &AllocatableDevice{Gpu: newPartTestGpu(nil, 0)}
		require.Equal(t, "gpu-0", gpu.PartGetDevice(nil).Name)

		mig := &AllocatableDevice{
			MigDynamic: newPartTestMigSpec(newPartTestGpu(nil, 0), 2, 2),
		}
		require.Equal(t, "gpu-0-mig-1g10gb-19-2", mig.PartGetDevice(nil).Name)
	})

	for _, tc := range []struct {
		name   string
		device *AllocatableDevice
		panics string
	}{
		{"static MIG", &AllocatableDevice{MigStatic: &MigDeviceInfo{}},
			"PartGetDevice() called for MigStaticDeviceType"},
		{"VFIO", &AllocatableDevice{Vfio: &VfioDeviceInfo{}},
			"not yet implemented"},
		{"an unset device", &AllocatableDevice{},
			"unexpected type for AllocatableDevice"},
	} {
		t.Run("panics for "+tc.name, func(t *testing.T) {
			require.PanicsWithValue(t, tc.panics,
				func() { tc.device.PartGetDevice(nil) })
		})
	}
}
