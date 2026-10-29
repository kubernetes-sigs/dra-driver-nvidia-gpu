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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// The device name is the canonical device name announced by us a DRA
// ResourceSlice). It must be a (node-local) unambiguous device identifier. It's
// exposed to users in error messages. It's played back to us upon a
// NodePrepareResources request, which is when we look it up in the
// `AllocatableDevices` map. Conceptually, this the same as
// kubeletplugin.Device.DeviceName (documented with 'DeviceName identifies the
// device inside that pool').
type DeviceName = string

// Represents the PCI address (BDF format) of a physical GPU device.
type PCIBusID = string

type AllocatableDevices map[DeviceName]*AllocatableDevice
type PerGPUAllocatableDevices struct {
	allocatablesMap map[PCIBusID]AllocatableDevices
}

// AllocatableDevice represents an individual device that can be allocated.
type AllocatableDevice struct {
	Gpu        *GpuInfo        `json:"gpu,omitempty"`
	MigDynamic *MigSpec        `json:"migDynamic,omitempty"`
	MigStatic  *MigDeviceInfo  `json:"migStatic,omitempty"`
	Vfio       *VfioDeviceInfo `json:"vfio,omitempty"`

	// DeviceTaints holds DRA device taints set by the health monitor. Published
	// as part of the device's ResourceSlice entry so the scheduler and
	// kubelet honour them per KEP-5055.
	DeviceTaints []resourceapi.DeviceTaint `json:"deviceTaints,omitempty"`
}

func (d AllocatableDevice) Type() string {
	if d.Gpu != nil {
		return GpuDeviceType
	}
	if d.MigDynamic != nil {
		return MigDynamicDeviceType
	}
	if d.MigStatic != nil {
		return MigStaticDeviceType
	}
	if d.Vfio != nil {
		return VfioDeviceType
	}
	return UnknownDeviceType
}

func (d AllocatableDevice) IsStaticOrDynMigDevice() bool {
	switch d.Type() {
	case MigStaticDeviceType, MigDynamicDeviceType:
		return true
	default:
		return false
	}
}

func (d *AllocatableDevice) CanonicalName() string {
	switch d.Type() {
	case GpuDeviceType:
		return d.Gpu.CanonicalName()
	case MigStaticDeviceType:
		return d.MigStatic.CanonicalName()
	case MigDynamicDeviceType:
		return d.MigDynamic.CanonicalName()
	case VfioDeviceType:
		return d.Vfio.CanonicalName()
	}
	panic("unexpected type for AllocatableDevice")
}

func (d *AllocatableDevice) GetDevice(config *Config) resourceapi.Device {
	var dev resourceapi.Device
	switch d.Type() {
	case GpuDeviceType:
		dev = d.Gpu.GetDevice()
		applyConsumableShares(&dev, config)
	case MigStaticDeviceType:
		dev = d.MigStatic.GetDevice()
		applyConsumableShares(&dev, config)
	case MigDynamicDeviceType:
		panic("GetDevice() must currently not be called for MigDynamicDeviceType")
	case VfioDeviceType:
		// VFIO passthrough devices do not support consumable shares.
		dev = d.Vfio.GetDevice()
	default:
		panic("unexpected type for AllocatableDevice")
	}
	dev.Taints = d.Taints()
	return dev
}

// UUID() is here for `AllocatableDevices` to implement the `UUIDProvider`
// interface. Conceptually, at least since introduction of DynamicMIG, some
// allocatable devices are abstract devices that do not have a UUID before
// actualization -- hence, the idea of `AllocatableDevices` implementing
// UUIDProvider is brittle.
func (d AllocatableDevice) UUID() string {
	if d.Gpu != nil {
		return d.Gpu.UUID
	}
	if d.MigStatic != nil {
		return d.MigStatic.UUID
	}
	if d.MigDynamic != nil {
		// For now, the caller must sure to never call UUID() on such a device.
		// This method for now exists because when the DynamicMIG feature gate
		// is disabled, `AllocatableDevices` _can_ implement UUIDProvider; and
		// that feature is used throughout the code base. This needs
		// restructuring and cleanup.
		panic("unexpected UUID() call for AllocatableDevice of type MigDynamic")
	}
	if d.Vfio != nil {
		return d.Vfio.UUID
	}
	panic("unexpected type for AllocatableDevice")
}

func (d *AllocatableDevice) GetGPUPCIBusID() string {
	switch d.Type() {
	case GpuDeviceType:
		return d.Gpu.PciBusID
	case MigStaticDeviceType:
		return d.MigStatic.Parent.PciBusID
	case MigDynamicDeviceType:
		return d.MigDynamic.Parent.PciBusID
	case VfioDeviceType:
		return d.Vfio.PciBusID
	}
	panic("unexpected type for AllocatableDevice")
}

func (d AllocatableDevices) GetGPUs() []*AllocatableDevice {
	var devices []*AllocatableDevice
	for _, device := range d {
		if device.Type() == GpuDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

func (d AllocatableDevices) GetMigStaticDevices() []*AllocatableDevice {
	var devices []*AllocatableDevice
	for _, device := range d {
		if device.Type() == MigStaticDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

func (d AllocatableDevices) GetMigDynamicDevices() []*AllocatableDevice {
	var devices []*AllocatableDevice
	for _, device := range d {
		if device.Type() == MigDynamicDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

func (d AllocatableDevices) GetVfioDevices() []*AllocatableDevice {
	var devices []*AllocatableDevice
	for _, device := range d {
		if device.Type() == VfioDeviceType {
			devices = append(devices, device)
		}
	}
	return devices
}

// Required for implementing UUIDProvider. Meant to return (only) full GPU UUIDs.
func (d AllocatableDevices) GpuUUIDs() []string {
	var uuids []string
	for _, dev := range d {
		if dev.Type() == GpuDeviceType {
			uuids = append(uuids, dev.UUID())
		}
	}
	slices.Sort(uuids)
	return uuids
}

// Required for implementing UUIDProvider. Meant to return MIG device UUIDs.
// Must not be called when the DynamicMIG featuregate is enabled.
func (d AllocatableDevices) MigDeviceUUIDs() []string {
	if featuregates.Enabled(featuregates.DynamicMIG) {
		panic("MigDeviceUUIDs() unexpectedly called (DynamicMIG is enabled)")
	}
	var uuids []string
	for _, dev := range d {
		if dev.Type() == MigStaticDeviceType {
			uuids = append(uuids, dev.UUID())
		}
	}
	slices.Sort(uuids)
	return uuids
}

func (d AllocatableDevices) VfioDeviceUUIDs() []string {
	var uuids []string
	for _, device := range d {
		if device.Type() == VfioDeviceType {
			uuids = append(uuids, device.Vfio.UUID)
		}
	}
	slices.Sort(uuids)
	return uuids
}

// Required for implementing UUIDProvider. Meant to return full GPU UUIDs and
// MIG device UUIDs. Must not be used when the DynamicMIG featuregate is
// enabled. Unsure what it's supposed to return for VFIO devices.
func (d AllocatableDevices) UUIDs() []string {
	var uuids []string
	for _, dev := range d {
		uuids = append(uuids, dev.UUID())
	}
	slices.Sort(uuids)
	return uuids
}

func (d *PerGPUAllocatableDevices) GetGPUDeviceByPCIBusID(pciBusID string) *AllocatableDevice {
	if devices, ok := d.allocatablesMap[pciBusID]; ok {
		for _, device := range devices {
			if device.Type() != GpuDeviceType {
				continue
			}
			return device
		}
	}
	return nil
}

func (d *PerGPUAllocatableDevices) AddGPUAllocatables(pciBusID string, allocatables AllocatableDevices) error {
	if allocatables == nil {
		return fmt.Errorf("allocatables is nil")
	}
	if _, ok := d.allocatablesMap[pciBusID]; !ok {
		d.allocatablesMap[pciBusID] = make(AllocatableDevices)
	}
	klog.Infof("Adding allocatables for PCI bus ID: %s", pciBusID)
	d.allocatablesMap[pciBusID] = allocatables
	return nil
}

func (d *PerGPUAllocatableDevices) AddAllocatableDevice(allocatable *AllocatableDevice) error {
	if allocatable == nil {
		return fmt.Errorf("allocatable is nil")
	}
	pciBusID := allocatable.GetGPUPCIBusID()
	if _, ok := d.allocatablesMap[pciBusID]; !ok {
		d.allocatablesMap[pciBusID] = make(AllocatableDevices)
	}
	klog.V(4).Infof("Adding allocatable device %q for PCI bus ID: %s", allocatable.CanonicalName(), pciBusID)
	d.allocatablesMap[pciBusID][allocatable.CanonicalName()] = allocatable
	return nil
}

func (d *PerGPUAllocatableDevices) GetAllocatableDevice(deviceName DeviceName) *AllocatableDevice {
	for _, devices := range d.allocatablesMap {
		if device, ok := devices[deviceName]; ok {
			return device
		}
	}
	return nil
}

func (d *PerGPUAllocatableDevices) GetAllDevices(pciBusIDs ...string) AllocatableDevices {
	all := make(AllocatableDevices)
	if len(pciBusIDs) == 0 {
		for _, devices := range d.allocatablesMap {
			maps.Copy(all, devices)
		}
		return all
	}

	for _, pciBusID := range pciBusIDs {
		if devices, ok := d.allocatablesMap[pciBusID]; ok {
			maps.Copy(all, devices)
		}
	}
	return all
}

// Taints returns a copy of the device's taints to prevent data races
// when being read concurrently by the ResourceSlice builder.
func (d *AllocatableDevice) Taints() []resourceapi.DeviceTaint {
	return slices.Clone(d.DeviceTaints)
}

// AddOrUpdateTaint adds a new taint or updates an existing one with the same
// key. NoSchedule taints are sticky and remain unchanged until explicit
// recovery removes them.
// Returns true if the taint set was modified.
func (d *AllocatableDevice) AddOrUpdateTaint(taint *resourceapi.DeviceTaint) bool {
	for i, existing := range d.DeviceTaints {
		if existing.Key == taint.Key {
			// 1. If nothing actually changed, exit early to avoid API calls
			if existing.Value == taint.Value && existing.Effect == taint.Effect {
				return false
			}

			// Keep the NoSchedule taint until recovery explicitly removes the taint.
			if existing.Effect == resourceapi.DeviceTaintEffectNoSchedule {
				return false
			}

			// 2. Otherwise, update the fields and the timestamp
			d.DeviceTaints[i].Value = taint.Value
			d.DeviceTaints[i].Effect = taint.Effect
			d.DeviceTaints[i].TimeAdded = nil // reset timestamp for the API server
			return true
		}
	}

	// 3. Key doesn't exist yet, append the dereferenced struct
	d.DeviceTaints = append(d.DeviceTaints, *taint)
	return true
}

// RemoveTaint removes the taint with the specified key. It returns true when a
// taint was removed.
func (d *AllocatableDevice) RemoveTaint(key string) (resourceapi.DeviceTaint, bool) {
	for i, taint := range d.DeviceTaints {
		if taint.Key == key {
			d.DeviceTaints = slices.Delete(d.DeviceTaints, i, i+1)
			return taint, true
		}
	}
	return resourceapi.DeviceTaint{}, false
}

func (d AllocatableDevices) GetGPUDeviceByUUID(uuid string) *AllocatableDevice {
	for _, device := range d {
		if device.Type() == GpuDeviceType &&
			device.Gpu.UUID == uuid {
			return device
		}
	}

	return nil
}

func (d AllocatableDevices) GetMigStaticDeviceByLiveTuple(tuple *MigLiveTuple) *AllocatableDevice {
	if tuple == nil {
		return nil
	}
	for _, device := range d {
		if device.Type() != MigStaticDeviceType {
			continue
		}
		migTuple := device.MigStatic.LiveTuple()
		if migTuple.ParentUUID == tuple.ParentUUID &&
			migTuple.GIID == tuple.GIID &&
			migTuple.CIID == tuple.CIID {
			return device
		}
	}

	return nil
}

func (d AllocatableDevices) GetMigDynamicDeviceByTuple(tuple *MigSpecTuple) *AllocatableDevice {
	if tuple == nil {
		return nil
	}
	for _, device := range d {
		if device.Type() != MigDynamicDeviceType {
			continue
		}
		migTuple := device.MigDynamic.Tuple()
		if migTuple.ParentPCIBusID == tuple.ParentPCIBusID &&
			migTuple.ProfileID == tuple.ProfileID &&
			migTuple.PlacementStart == tuple.PlacementStart {
			return device
		}
	}
	return nil
}

func (d AllocatableDevices) List() []*AllocatableDevice {
	deviceList := make([]*AllocatableDevice, 0, len(d))
	for _, device := range d {
		deviceList = append(deviceList, device)
	}
	return deviceList
}

// Snapshot returns the allocatable devices as base64-encoded JSON. Restoring
// MIG profiles requires decoding concrete values for the nvdev.MigProfile interfaces.
func (d AllocatableDevices) Snapshot() (GPUAllocatableSnapshot, error) {
	// json.Marshal sorts map keys, keeping snapshots stable across map iteration orders.
	data, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("failed to marshal allocatable devices snapshot: %w", err)
	}
	return GPUAllocatableSnapshot(base64.StdEncoding.EncodeToString(data)), nil
}
