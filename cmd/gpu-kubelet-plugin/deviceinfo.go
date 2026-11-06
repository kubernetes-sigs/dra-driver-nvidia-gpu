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
	"fmt"
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// Represents a specific, full, physical GPU device.
type GpuInfo struct {
	UUID                  string                           `json:"uuid"`
	Index                 int                              `json:"index"`
	Minor                 int                              `json:"minor"`
	MigCapable            bool                             `json:"migCapable"`
	MigEnabled            bool                             `json:"migEnabled"`
	VfioEnabled           bool                             `json:"vfioEnabled"`
	MemoryBytes           *uint64                          `json:"memoryBytes"`
	ProductName           string                           `json:"productName"`
	Brand                 string                           `json:"brand"`
	Architecture          string                           `json:"architecture"`
	CudaComputeCapability string                           `json:"cudaComputeCapability"`
	DriverVersion         string                           `json:"driverVersion"`
	CudaDriverVersion     string                           `json:"cudaDriverVersion"`
	PciBusID              string                           `json:"pciBusID"`
	PciBusIDAttr          *deviceattribute.DeviceAttribute `json:"pciBusIDAttr"`
	PcieRootAttr          *deviceattribute.DeviceAttribute `json:"pcieRootAttr"`
	NumaNodeAttr          *deviceattribute.DeviceAttribute `json:"numaNodeAttr"`
	MigProfiles           []*MigProfileInfo                `json:"migProfiles"`
	AddressingMode        *string                          `json:"addressingMode"`

	// The following properties that can only be known after inspecting MIG
	// profiles.
	MaxCapacities PartCapacityMap `json:"maxCapacities"`
	MemSliceCount int             `json:"memSliceCount"`

	// NVML module ID, published when FM partitioning is enabled. This is
	// distinct from FM's physical GPU ID on some platforms.
	GpuModuleID int `json:"gpuModuleID"`

	// partitionsBySize maps an FM partition size (number of GPUs in the
	// partition) to the partitionId of the partition of that size that
	// includes this GPU. Used to publish the `partition1`/`partition2`/
	// `partition4`/`partition8` device attributes.
	PartitionsBySize map[int]int `json:"partitionsBySize"`
}

type GpuSpecTuple struct {
	Index int `json:"index"`
}

func NewGpuSpecTupleFromCanonicalName(name DeviceName) (*GpuSpecTuple, error) {
	parts := strings.Split(name, "-")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid gpu name %q", name)
	}
	if parts[0] != "gpu" {
		return nil, fmt.Errorf("invalid gpu name %q", name)
	}

	index, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid index %q", parts[1])
	}
	gst := &GpuSpecTuple{
		Index: index,
	}
	return gst, nil
}

type GpuLiveTuple struct {
	UUID  string `json:"uuid"`
	Minor *int   `json:"minor,omitempty"`
}

// Represents a specific (concrete, incarnated, created) MIG device. Annotated
// properties are stored in the checkpoint JSON upon prepare.
type MigDeviceInfo struct {
	// Selectively serialize some properties to the checkpoint JSON file (needed
	// mainly for controlled deletion in the unprepare flow).

	UUID        string `json:"uuid"`
	Profile     string `json:"profile"`
	ParentUUID  string `json:"parentUUID"`
	GiProfileID int    `json:"profileId"`

	// TODO: maybe embed MigLiveTuple.
	ParentMinor int `json:"parentMinor"`
	CIID        int `json:"ciId"`
	GIID        int `json:"giId"`

	// Store PlacementStart in the JSON checkpoint because in CanonicalName() we
	// rely on this -- and this must work after JSON deserialization.
	PlacementStart int `json:"placementStart"`
	PlacementSize  int `json:"placementSize"`

	GIInfo        *nvml.GpuInstanceInfo            `json:"giInfo"`
	CIInfo        *nvml.ComputeInstanceInfo        `json:"ciInfo"`
	Parent        *GpuInfo                         `json:"parent"`
	GiProfileInfo *nvml.GpuInstanceProfileInfo     `json:"giProfileInfo"`
	CiProfileInfo *nvml.ComputeInstanceProfileInfo `json:"ciProfileInfo"`
}

type VfioDeviceInfo struct {
	UUID        string   `json:"uuid"`
	DeviceID    string   `json:"deviceID"`
	VendorID    string   `json:"vendorID"`
	Index       int      `json:"index"`
	Parent      *GpuInfo `json:"parent"`
	ProductName string   `json:"productName"`
	// `omitempty`: postdates 25.12.0; emitting "pciBusID":"" would trip
	// CorruptCheckpointError on upgrade. See issue 1080.
	PciBusID               string                           `json:"pciBusID,omitempty"`
	PciBusIDAttr           *deviceattribute.DeviceAttribute `json:"pciBusIDAttr"`
	PcieRootAttr           *deviceattribute.DeviceAttribute `json:"pcieRootAttr"`
	NumaNodeAttr           *deviceattribute.DeviceAttribute `json:"numaNodeAttr"`
	IommuGroup             int                              `json:"iommuGroup"`
	IommuFDEnabled         bool                             `json:"iommuFDEnabled"`
	AddressableMemoryBytes uint64                           `json:"addressableMemoryBytes"`
	VfioModule             string                           `json:"vfioModule"`
}

type VfioSpecTuple struct {
	Index int `json:"index"`
}

// NewVfioSpecTupleFromCanonicalName returns a VfioSpecTuple from a canonical name.
// Format: gpu-vfio-<index>.
// The index is the only parseable field from the canonical name and it is linked
// to the PCI Bus ID of the device.
func NewVfioSpecTupleFromCanonicalName(name DeviceName) (*VfioSpecTuple, error) {
	parts := strings.Split(name, "-")
	if len(parts) != 3 || parts[0] != "gpu" || parts[1] != "vfio" {
		return nil, fmt.Errorf("invalid vfio name %q", name)
	}

	index, err := strconv.Atoi(parts[2])
	if err != nil {
		return nil, fmt.Errorf("invalid index %q", parts[1])
	}

	vst := &VfioSpecTuple{
		Index: index,
	}
	return vst, nil
}

type VfioLiveTuple struct {
	UUID     string `json:"uuid"`
	PciBusID string `json:"pciBusID"`
}

// CanonicalName returns the nameused for device announcement (in ResourceSlice
// objects). There is quite a bit of history to using the minor number for
// device announcement. Some context can be found at
// https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/issues/563#issuecomment-3345631087.
func (d *GpuInfo) CanonicalName() DeviceName {
	return fmt.Sprintf("gpu-%d", d.Index)
}

// String returns both the GPU minor for easy recognizability, but also the
// UUID for precision. It is intended for usage in log messages.
func (d *GpuInfo) String() string {
	return fmt.Sprintf("%s-%s", d.CanonicalName(), d.UUID)
}

func (d *GpuInfo) LiveTuple() *GpuLiveTuple {
	return &GpuLiveTuple{
		UUID:  d.UUID,
		Minor: &d.Minor,
	}
}

func (m *MigDeviceInfo) SpecTuple() *MigSpecTuple {
	return &MigSpecTuple{
		ParentIndex:    m.Parent.Index,
		ParentMinor:    m.ParentMinor,
		ProfileID:      m.GiProfileID,
		PlacementStart: m.PlacementStart,
	}
}

func (m *MigDeviceInfo) LiveTuple() *MigLiveTuple {
	return &MigLiveTuple{
		ParentMinor: m.ParentMinor,
		ParentUUID:  m.ParentUUID,
		GIID:        m.GIID,
		CIID:        m.CIID,
		MigUUID:     m.UUID,
	}
}

// Return the canonical MIG device name. The name unambiguously defines the
// physical configuration, but doesn't reflect the fact that this represents a
// curently-live MIG device.
func (d *MigDeviceInfo) CanonicalName() string {
	return d.SpecTuple().ToCanonicalName(d.Profile)
}

func (d *VfioDeviceInfo) CanonicalName() string {
	return fmt.Sprintf("gpu-vfio-%d", d.Index)
}

func (d *VfioDeviceInfo) LiveTuple() *VfioLiveTuple {
	return &VfioLiveTuple{
		UUID:     d.UUID,
		PciBusID: d.PciBusID,
	}
}

// Populate internal data structures -- detail that is only known after
// inspecting all individual MIG profiles associated with this physical GPU.
func (d *GpuInfo) AddDetailAfterWalkingMigProfiles(maxcap PartCapacityMap, memSliceCount int) {
	d.MaxCapacities = maxcap
	d.MemSliceCount = memSliceCount
}

func (d *GpuInfo) Attributes() map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"type": {
			StringValue: ptr.To(GpuDeviceType),
		},
		"uuid": {
			StringValue: &d.UUID,
		},
		"productName": {
			StringValue: &d.ProductName,
		},
		"brand": {
			StringValue: &d.Brand,
		},
		"architecture": {
			StringValue: &d.Architecture,
		},
		"cudaComputeCapability": {
			VersionValue: ptr.To(semver.MustParse(d.CudaComputeCapability).String()),
		},
		"driverVersion": {
			VersionValue: ptr.To(semver.MustParse(d.DriverVersion).String()),
		},
		"cudaDriverVersion": {
			VersionValue: ptr.To(semver.MustParse(d.CudaDriverVersion).String()),
		},
	}

	addDeviceAttribute(attrs, d.PciBusIDAttr)
	addDeviceAttribute(attrs, d.PcieRootAttr)
	addDeviceAttribute(attrs, d.NumaNodeAttr)

	if d.AddressingMode != nil {
		attrs["addressingMode"] = resourceapi.DeviceAttribute{
			StringValue: d.AddressingMode,
		}
	}

	if featuregates.Enabled(featuregates.FabricManagerPartitioning) {
		d.addFabricManagerAttributes(attrs)
	}

	return attrs
}

// addFabricManagerAttributes publishes the NVML-reported gpuModuleID and
// Fabric Manager partitionN attributes for this physical GPU. Partition
// membership is resolved at discovery time (see attachFabricManagerPartitions).
func (d *GpuInfo) addFabricManagerAttributes(attrs map[resourceapi.QualifiedName]resourceapi.DeviceAttribute) {
	if d == nil {
		return
	}

	if d.GpuModuleID == 0 && len(d.PartitionsBySize) == 0 {
		klog.V(4).Infof("No Fabric Manager attributes for %s", d.CanonicalName())
		return
	}

	klog.V(4).Infof("Adding Fabric Manager attributes for %s: gpuModuleID=%d partitionsBySize=%v",
		d.CanonicalName(), d.GpuModuleID, d.PartitionsBySize)
	if d.GpuModuleID != 0 {
		attrs["gpuModuleID"] = resourceapi.DeviceAttribute{
			IntValue: ptr.To(int64(d.GpuModuleID)),
		}
	}

	for size, partitionID := range d.PartitionsBySize {
		key := resourceapi.QualifiedName(fmt.Sprintf("partition%d", size))
		attrs[key] = resourceapi.DeviceAttribute{
			IntValue: ptr.To(int64(partitionID)),
		}
	}
}

func (d *GpuInfo) GetDevice() resourceapi.Device {
	device := resourceapi.Device{
		Name:       d.CanonicalName(),
		Attributes: d.Attributes(),
		Capacity:   d.fullGpuCapacity(),
	}
	return device
}

func (d *MigDeviceInfo) GetDevice() resourceapi.Device {

	attrs := CommonAttributesMig(d.Parent, d.Profile)
	attrs["uuid"] = resourceapi.DeviceAttribute{
		StringValue: &d.UUID,
	}

	device := resourceapi.Device{
		Name:       d.CanonicalName(),
		Attributes: attrs,
		Capacity:   CommonCapacitiesMig(d.GiProfileInfo),
	}

	// Note(JP): noted elsewhere; what's the purpose of announcing memory slices
	// as capacity? Do we want to allow users to request specific placement?
	for i := d.PlacementStart; i < d.PlacementStart+d.PlacementSize; i++ {
		capacity := resourceapi.QualifiedName(fmt.Sprintf("memorySlice%d", i))
		device.Capacity[capacity] = resourceapi.DeviceCapacity{
			Value: *resource.NewQuantity(1, resource.BinarySI),
		}
	}

	return device
}

func (d *VfioDeviceInfo) Attributes() map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"type": {
			StringValue: ptr.To(VfioDeviceType),
		},
		"uuid": {
			StringValue: &d.UUID,
		},
		"deviceID": {
			StringValue: &d.DeviceID,
		},
		"vendorID": {
			StringValue: &d.VendorID,
		},
		"productName": {
			StringValue: &d.ProductName,
		},
		"iommuFDEnabled": {
			BoolValue: ptr.To(d.IommuFDEnabled),
		},
	}
	addDeviceAttribute(attrs, d.PciBusIDAttr)
	addDeviceAttribute(attrs, d.PcieRootAttr)
	addDeviceAttribute(attrs, d.NumaNodeAttr)

	if featuregates.Enabled(featuregates.FabricManagerPartitioning) {
		if d.Parent == nil {
			klog.V(4).Infof("No parent GPU for %s; skipping Fabric Manager attributes", d.CanonicalName())
		} else {
			d.Parent.addFabricManagerAttributes(attrs)
		}
	}
	return attrs
}

func (d *VfioDeviceInfo) Capacities() map[resourceapi.QualifiedName]resourceapi.DeviceCapacity {
	return map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
		"addressableMemory": {
			Value: *resource.NewQuantity(int64(d.AddressableMemoryBytes), resource.BinarySI),
		},
	}
}

func (d *VfioDeviceInfo) GetDevice() resourceapi.Device {
	return resourceapi.Device{
		Name:       d.CanonicalName(),
		Attributes: d.Attributes(),
		Capacity:   d.Capacities(),
	}
}

func addDeviceAttribute(attrs map[resourceapi.QualifiedName]resourceapi.DeviceAttribute, attr *deviceattribute.DeviceAttribute) {
	if attr != nil {
		attrs[attr.Name] = attr.Value
	}
}
