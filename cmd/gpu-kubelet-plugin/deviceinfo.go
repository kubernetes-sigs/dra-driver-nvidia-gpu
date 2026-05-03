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

	"github.com/Masterminds/semver/v3"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"
)

// Represents a specific, full, physical GPU device.
type GpuInfo struct {
	UUID                  string `json:"uuid"`
	minor                 int
	migEnabled            bool
	vfioEnabled           bool
	memoryBytes           uint64
	productName           string
	brand                 string
	architecture          string
	cudaComputeCapability string
	driverVersion         string
	cudaDriverVersion     string
	pciBusID              string
	pciBusIDAttr          *deviceattribute.DeviceAttribute
	pcieRootAttr          *deviceattribute.DeviceAttribute
	migProfiles           []*MigProfileInfo
	addressingMode        *string

	// The following properties that can only be known after inspecting MIG
	// profiles.
	maxCapacities PartCapacityMap
	memSliceCount int
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

	gIInfo        *nvml.GpuInstanceInfo
	cIInfo        *nvml.ComputeInstanceInfo
	parent        *GpuInfo
	giProfileInfo *nvml.GpuInstanceProfileInfo
	ciProfileInfo *nvml.ComputeInstanceProfileInfo
}

type VfioDeviceInfo struct {
	UUID                   string `json:"uuid"`
	deviceID               string
	vendorID               string
	index                  int
	parent                 *GpuInfo
	productName            string
	PciBusID               string `json:"pciBusID"`
	pciBusIDAttr           *deviceattribute.DeviceAttribute
	pcieRootAttr           *deviceattribute.DeviceAttribute
	numaNode               int
	iommuGroup             int
	iommuFDEnabled         bool
	addressableMemoryBytes uint64
}

// CanonicalName returns the nameused for device announcement (in ResourceSlice
// objects). There is quite a bit of history to using the minor number for
// device announcement. Some context can be found at
// https://sigs.k8s.io/dra-driver-nvidia-gpu/issues/563#issuecomment-3345631087.
func (d *GpuInfo) CanonicalName() DeviceName {
	return fmt.Sprintf("gpu-%d", d.minor)
}

// String returns both the GPU minor for easy recognizability, but also the
// UUID for precision. It is intended for usage in log messages.
func (d *GpuInfo) String() string {
	return fmt.Sprintf("%s-%s", d.CanonicalName(), d.UUID)
}

func (m *MigDeviceInfo) SpecTuple() *MigSpecTuple {
	return &MigSpecTuple{
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
	return fmt.Sprintf("gpu-vfio-%d", d.index)
}

// Populate internal data structures -- detail that is only known after
// inspecting all individual MIG profiles associated with this physical GPU.
func (d *GpuInfo) AddDetailAfterWalkingMigProfiles(maxcap PartCapacityMap, memSliceCount int) {
	d.maxCapacities = maxcap
	d.memSliceCount = memSliceCount
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
			StringValue: &d.productName,
		},
		"brand": {
			StringValue: &d.brand,
		},
		"architecture": {
			StringValue: &d.architecture,
		},
		"cudaComputeCapability": {
			VersionValue: ptr.To(semver.MustParse(d.cudaComputeCapability).String()),
		},
		"driverVersion": {
			VersionValue: ptr.To(semver.MustParse(d.driverVersion).String()),
		},
		"cudaDriverVersion": {
			VersionValue: ptr.To(semver.MustParse(d.cudaDriverVersion).String()),
		},
	}

	if d.pciBusIDAttr != nil {
		attrs[d.pciBusIDAttr.Name] = d.pciBusIDAttr.Value
	}
	if d.pcieRootAttr != nil {
		attrs[d.pcieRootAttr.Name] = d.pcieRootAttr.Value
	}

	if d.pciBusIDAttr != nil {
		attrs[d.pciBusIDAttr.Name] = d.pciBusIDAttr.Value
	}

	if d.addressingMode != nil {
		attrs["addressingMode"] = resourceapi.DeviceAttribute{
			StringValue: d.addressingMode,
		}
	}

	return attrs
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

	attrs := CommonAttributesMig(d.parent, d.Profile)
	attrs["uuid"] = resourceapi.DeviceAttribute{
		StringValue: &d.UUID,
	}

	device := resourceapi.Device{
		Name:       d.CanonicalName(),
		Attributes: attrs,
		Capacity:   CommonCapacitiesMig(d.giProfileInfo),
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

func (d *VfioDeviceInfo) GetDevice() resourceapi.Device {
	device := resourceapi.Device{
		Name: d.CanonicalName(),
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"type": {
				StringValue: ptr.To(VfioDeviceType),
			},
			"uuid": {
				StringValue: &d.UUID,
			},
			"deviceID": {
				StringValue: &d.deviceID,
			},
			"vendorID": {
				StringValue: &d.vendorID,
			},
			"numa": {
				IntValue: ptr.To(int64(d.numaNode)),
			},
			"productName": {
				StringValue: &d.productName,
			},
			"iommuFDEnabled": {
				BoolValue: ptr.To(d.iommuFDEnabled),
			},
		},
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"addressableMemory": {
				Value: *resource.NewQuantity(int64(d.addressableMemoryBytes), resource.BinarySI),
			},
		},
	}

	if d.pciBusIDAttr != nil {
		device.Attributes[d.pciBusIDAttr.Name] = d.pciBusIDAttr.Value
	}

	if d.pcieRootAttr != nil {
		device.Attributes[d.pcieRootAttr.Name] = d.pcieRootAttr.Value
	}

	return device
}
