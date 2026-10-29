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
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/api/validate/constraints"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

type PartCapacityMap map[resourceapi.QualifiedName]resourceapi.DeviceCapacity

// This is the single source of truth for the non-MIG full-GPU capacity
// field and MUST be used by both the GetDevice() path and the
// partitions PartCapacities(). The two publish paths are
// dispatched separately (feature-gate-gated in publishResources()), so
// without a shared helper they may silently drift.
func (d *GpuInfo) fullGpuCapacity() PartCapacityMap {
	if d.MemoryBytes == nil {
		return nil
	}
	return PartCapacityMap{
		"memory": resourceapi.DeviceCapacity{
			Value: *resource.NewQuantity(int64(*d.MemoryBytes), resource.BinarySI),
		},
	}
}

// KEP 4815 device announcement: return the full device capacity for this device
// (uses information from looking at all MIG profiles beforehand).
//
// When MIG profiles have not been inspected for Full GPU (e.g. on Ampere
// with MIG disabled), maxCapacities is empty. Fall back to advertising
// at least the GPU's total memory, matching the
// legacy non-DynamicMIG GetDevice() path so that the full GPU device is
// announced with quantitative capacity in both paths.
func (d *GpuInfo) PartCapacities() PartCapacityMap {
	if len(d.MaxCapacities) > 0 {
		return d.MaxCapacities
	}
	return d.fullGpuCapacity()
}

// KEP 4815 device announcement: return the name for the shared counter
// representing this full device.
func (d *GpuInfo) GetSharedCounterSetName() string {
	return toRFC1123Compliant(fmt.Sprintf("%s-counter-set", d.CanonicalName()))
}

// KEP 4815 device announcement: for now, define exactly one CounterSet per full
// GPU device. Individual partitions consume from that. In that CounterSet,
// define one counter per device capacity dimension, and add one counter
// (capacity 1) per memory slice.
func (d *GpuInfo) PartSharedCounterSets() []resourceapi.CounterSet {
	// Returns nil when no MIG profile data has been collected for this GPU
	// (e.g. on Ampere with MIG disabled, or for vGPU guests). Such GPUs have
	// no partitions, so a per-GPU CounterSet has no consumers and the
	// Kubernetes API would reject an empty Counters map.
	// error: spec.sharedCounters[0].counters: Required value
	// For passthrough support, we still need to publish a counter set for the GPU,
	// even if we don't have any MIG profile data.
	if len(d.MaxCapacities) == 0 && !featuregates.Enabled(featuregates.PassthroughSupport) {
		return nil
	}
	counters := addCountersForMemSlices(capacitiesToCounters(d.MaxCapacities), 0, d.MemSliceCount)
	if featuregates.Enabled(featuregates.PassthroughSupport) {
		memory := d.fullGpuCapacity()["memory"].Value
		counters = addCountersForGpuShare(counters, memory.Value(), memory.Value())
	}
	return []resourceapi.CounterSet{{
		Name:     d.GetSharedCounterSetName(),
		Counters: counters,
	}}
}

// KEP 4815 device announcement: define what this full GPU consumes when allocated.
// Let the full device consume everything. Goals: 1) when the full device is
// allocated, all available counters drop to zero. 2) when the smallest
// partition gets allocated, the full device cannot be allocated anymore.
func (d *GpuInfo) PartConsumesCounters() []resourceapi.DeviceCounterConsumption {
	// Returns nil when no MIG profile data has been collected for this GPU
	// (matches PartSharedCounterSets — there is no CounterSet to consume from).
	// For passthrough support, the GPU needs to consume from its own counter set.
	if len(d.MaxCapacities) == 0 && !featuregates.Enabled(featuregates.PassthroughSupport) {
		return nil
	}
	counters := addCountersForMemSlices(capacitiesToCounters(d.MaxCapacities), 0, d.MemSliceCount)
	if featuregates.Enabled(featuregates.PassthroughSupport) {
		memory := d.PartCapacities()["memory"].Value
		counters = addCountersForGpuShare(counters, memory.Value(), memory.Value())
	}
	return []resourceapi.DeviceCounterConsumption{{
		CounterSet: d.GetSharedCounterSetName(),
		Counters:   counters,
	}}
}

// KEP 4815 device announcement: return the 'full' device description.
func (d *GpuInfo) PartGetDevice() resourceapi.Device {
	dev := resourceapi.Device{
		Name:             d.CanonicalName(),
		Attributes:       d.Attributes(),
		Capacity:         d.PartCapacities(),
		ConsumesCounters: d.PartConsumesCounters(),
	}

	// Not available in all environments, enrich advertised device only
	// conditionally.
	if d.PcieRootAttr != nil {
		dev.Attributes[d.PcieRootAttr.Name] = d.PcieRootAttr.Value
	}

	return dev
}

// Return the full KEP 4815 representation of an abstract MIG device.
func (i *MigSpec) PartGetDevice() resourceapi.Device {
	d := resourceapi.Device{
		Name:             i.CanonicalName(),
		Attributes:       i.Attributes(),
		Capacity:         i.Capacities(),
		ConsumesCounters: i.PartConsumesCounters(),
	}

	// Not available in all environments, enrich advertised device only
	// conditionally.
	if i.Parent.PcieRootAttr != nil {
		d.Attributes[i.Parent.PcieRootAttr.Name] = i.Parent.PcieRootAttr.Value
	}

	return d
}

// Return the KEP 4818 capacities of an abstract MIG device.
//
// TODOMIG(JP): announce memory slices as capacities or not?
//
// Note(JP): for now, I feel like we may want to decouple capacity from
// placement. That would imply not announcing specific memory slices as part of
// capacity (specific memory slices encode placement). That makes sense to me,
// but I may of course miss something here.
//
// I noticed that in an example spec in KEP 4815 we enumerate memory slices in a
// partition's capacity. Example:
//
//   - name: gpu-2-mig-1g24gb-19-0
//     attributes:
//     ...
//     capacity:
//     ...
//     decoders:
//     value: "1"
//     encoders:
//     value: "0"
//     ...
//     memorySlice0:
//     value: "1"
//     memorySlice1:
//     value: "1"
//     multiprocessors:
//     value: "28"
//     ...
//
// 1) There, we only announce those slices with value 1 but we do _not_ announce
// memory slices not consumed (value: 0). That's inconsistent with other
// capacity dimensions which (in the example above) are enumerated despite
// having a value of zero (e.g. `encoders` above).
//
// 2) Semantically, to me, capacity I think can (and should?) be independent of
// placement. I am happy to be convinced otherwise.
//
// 3) If `capacity“ is in our case always encoding the _same_ information as
// `consumesCounters` then that is a lot of duplication and feels a bit wrong.
// They serve a different need, and hence there may be differences.
func (i MigSpec) Capacities() PartCapacityMap {
	return CommonCapacitiesMig(&i.GIProfileInfo)
}

func (i MigSpec) Attributes() map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	// All profiles are expected to have the same profile name.
	// So pick the first one.
	return CommonAttributesMig(i.Parent, i.CandidateProfiles[0].String())
}

func capacitiesToCounters(m PartCapacityMap) map[string]resourceapi.Counter {
	counters := make(map[string]resourceapi.Counter)
	for name, cap := range m {
		// Automatically derive counter name from capacity to ensure consistency.
		counters[camelToDNSName(string(name))] = resourceapi.Counter{Value: cap.Value}
	}
	return counters
}

// Construct and return the KEP 4815 DeviceCounterConsumption representation of
// an abstract MIG device.
//
// This device is a partition of a physical GPU. The returned
// `DeviceCounterConsumption` object describes which aspects precisely this
// partition consumes of the the full device.
//
// Each entry in capacity is modeled as a counter (consuming from the parent
// device). In addition, this MIG device, if allocated, consumes at least one
// specific memory slice. Each memory slice is modeled with its own counter
// (capacity: 1). Note that for example on a B200 GPU, the `3g.90gb` device
// consumes 4 out of 8 memory slices in total, but only 3 out of seven SMs. That
// is, with two `3g.90gb` devices allocated all memory slices are consumed, and
// one SM -- while unallocated -- cannot be used anymore. The parent is a full
// GPU device.
//
// When this device is allocated, it consumes from the parent's CounterSet. The
// parent's counter set is referred to by name. Use a naming convention:
// currently, a full GPU has precisely one counter set associated with it, and
// its name has the form 'gpu-%d-counter-set' where the placeholder is the GPU
// minor.
func (i MigSpec) PartConsumesCounters() []resourceapi.DeviceCounterConsumption {
	counters := addCountersForMemSlices(capacitiesToCounters(i.Capacities()), int(i.Placement.Start), int(i.Placement.Size))
	if featuregates.Enabled(featuregates.PassthroughSupport) {
		memory := i.Capacities()["memory"].Value
		totalMemory := i.Parent.PartCapacities()["memory"].Value
		counters = addCountersForGpuShare(counters, memory.Value(), totalMemory.Value())
	}
	return []resourceapi.DeviceCounterConsumption{{
		CounterSet: i.Parent.GetSharedCounterSetName(),
		Counters:   counters,
	}}
}

func (v *VfioDeviceInfo) PartCapacities() PartCapacityMap {
	return v.Capacities()
}

func (v *VfioDeviceInfo) PartConsumesCounters() []resourceapi.DeviceCounterConsumption {
	counters := make(map[string]resourceapi.Counter)
	counterSetName := v.GetSharedCounterSetName()
	if v.Parent != nil {
		counterSetName = v.Parent.GetSharedCounterSetName()
	}
	return []resourceapi.DeviceCounterConsumption{{
		CounterSet: counterSetName,
		Counters:   addCountersForGpuShare(counters, 0, 0),
	}}
}

func (v *VfioDeviceInfo) GetSharedCounterSetName() string {
	if v.Parent != nil {
		return v.Parent.GetSharedCounterSetName()
	}
	// Adapted from expected parent GPU canonical name.
	return toRFC1123Compliant(fmt.Sprintf("gpu-%d-counter-set", v.Index))
}

func (v *VfioDeviceInfo) PartSharedCounterSets() []resourceapi.CounterSet {
	// We expect this to be called only when the GPU is in passthrough mode
	// and we don't have its parent GPU snapshot. In case we reach here, then
	// its a mistake and dont publish any counter sets.
	if v.Parent != nil {
		return nil
	}
	counters := make(map[string]resourceapi.Counter)
	counters = addCountersForGpuShare(counters, 0, 0)
	return []resourceapi.CounterSet{{
		Name:     v.GetSharedCounterSetName(),
		Counters: counters,
	}}
}
func (v *VfioDeviceInfo) PartGetDevice() resourceapi.Device {
	dev := resourceapi.Device{
		Name:             v.CanonicalName(),
		Attributes:       v.Attributes(),
		Capacity:         v.PartCapacities(),
		ConsumesCounters: v.PartConsumesCounters(),
	}

	// Not available in all environments, enrich advertised device only
	// conditionally.
	if v.PcieRootAttr != nil {
		dev.Attributes[v.PcieRootAttr.Name] = v.PcieRootAttr.Value
	}
	return dev
}

// A variant of the legacy `GetDevice()`, for the Partitionable Devices paradigm.
func (d *AllocatableDevice) PartGetDevice(config *Config) resourceapi.Device {
	var dev resourceapi.Device
	switch d.Type() {
	case GpuDeviceType:
		dev = d.Gpu.PartGetDevice()
		applyConsumableShares(&dev, config)
	case MigStaticDeviceType:
		panic("PartGetDevice() called for MigStaticDeviceType")
	case MigDynamicDeviceType:
		dev = d.MigDynamic.PartGetDevice()
		applyConsumableShares(&dev, config)
	case VfioDeviceType:
		dev = d.Vfio.PartGetDevice()
	default:
		panic("unexpected type for AllocatableDevice")
	}
	dev.Taints = d.Taints()
	return dev
}

// Insert one counter for each memory slice consumed, as given by the `start`
// and `size` parameters (from a nvml.GpuInstancePlacement). Mutate the input
// map in place, and (also) return it.
func addCountersForMemSlices(counters map[string]resourceapi.Counter, start int, size int) map[string]resourceapi.Counter {
	for i := start; i < start+size; i++ {
		counters[memsliceCounterName(i)] = resourceapi.Counter{Value: *resource.NewQuantity(1, resource.BinarySI)}
	}
	return counters
}

func addCountersForGpuShare(counters map[string]resourceapi.Counter, memory int64, totalMemory int64) map[string]resourceapi.Counter {
	// If we don't have memory & total memory, then it means we've a gpu in passthrough mode,
	// so set the gpu-share to 100%.
	// For all other scenarios, we can calculate the gpu-share as the gpu memory is available via nvml APIs.
	if memory == 0 && totalMemory == 0 {
		counters["gpu-share"] = resourceapi.Counter{Value: *resource.NewQuantity(100, resource.BinarySI)}
		return counters
	}
	// Calcuate the gpu-share as a percentage of the total memory consumed by the device.
	// Full GPU: always at 100%.
	// VFIO: always at 100%.
	// MIG: calculate the gpu-share as a percentage of the total memory consumed by the device.
	gpuShare := (memory * 100) / totalMemory
	// Unlikely to happen, but cap the gpu-share at 100%.
	if gpuShare > 100 {
		gpuShare = 100
	}
	counters["gpu-share"] = resourceapi.Counter{Value: *resource.NewQuantity(gpuShare, resource.BinarySI)}
	return counters
}

// Return canonical name for memory slice (placement) `i` (a zero-based index).
// Note that this name must be used for memslice-N counters in a SharedCounters
// counter set, and for corresponding counters in a ConsumesCounters counter
// set. Counters (as opposed to capacities) are allowed to have hyphens in their
// name.
func memsliceCounterName(i int) string {
	return fmt.Sprintf("memory-slice-%d", i)
}

// Helper for creating an integer-based DeviceCapacity. Accept any integer type.
func intcap[T constraints.Integer](i T) resourceapi.DeviceCapacity {
	return resourceapi.DeviceCapacity{Value: *resource.NewQuantity(int64(i), resource.BinarySI)}
}
