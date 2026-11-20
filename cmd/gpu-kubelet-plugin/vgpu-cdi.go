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
	"path/filepath"

	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

// vgpuCDIHandler builds the per-device CDI spec entries for concrete vGPU
// devices (design: docs/design/vgpu-support.md, section 6.8).
type vgpuCDIHandler struct {
	deviceLib *deviceLib
}

func NewVgpuCDIHandler(deviceLib *deviceLib) *vgpuCDIHandler {
	return &vgpuCDIHandler{deviceLib: deviceLib}
}

// GetDeviceSpecs returns the CDI container edits giving the container access
// to a concrete vGPU device. For the mdev framework this is the mediated
// device's VFIO group node; for the vdev framework there is nothing to
// inject (the VF is bound to vfio-pci by the consumer, e.g. KubeVirt's
// virt-launcher, based on the Device Metadata PCI address).
func (h *vgpuCDIHandler) GetDeviceSpecs(concrete *VgpuConcrete) ([]cdispec.Device, error) {
	switch concrete.Framework {
	case vgpuFrameworkMdev:
		group, err := h.mdevIommuGroup(concrete.MdevUUID)
		if err != nil {
			return nil, fmt.Errorf("error resolving IOMMU group of vGPU instance %s: %w", concrete.MdevUUID, err)
		}
		return []cdispec.Device{{
			ContainerEdits: cdispec.ContainerEdits{
				DeviceNodes: []*cdispec.DeviceNode{
					{Path: filepath.Join(vfioDevicesRoot, group)},
				},
			},
		}}, nil
	case vgpuFrameworkVf:
		return []cdispec.Device{{
			ContainerEdits: cdispec.ContainerEdits{},
		}}, nil
	}
	return nil, fmt.Errorf("unknown vGPU framework %q", concrete.Framework)
}

// vgpuCommonEdits returns the claim-group-level CDI container edits for vGPU
// devices. Besides suppressing container-style GPU injection
// (NVIDIA_VISIBLE_DEVICES=void), the consuming VM runtime — libvirt/QEMU
// mediated host-device assignment in KubeVirt's virt-launcher — needs the
// VFIO control device /dev/vfio/vfio in addition to the per-device
// /dev/vfio/<group> nodes. Without it, libvirt fails at QEMU build time with
// "Mediated host device assignment requires VFIO support".
func vgpuCommonEdits() *cdiapi.ContainerEdits {
	return &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			Env: []string{"NVIDIA_VISIBLE_DEVICES=void"},
			DeviceNodes: []*cdispec.DeviceNode{
				{Path: filepath.Join(vfioDevicesRoot, "vfio")},
			},
		},
	}
}

// mdevIommuGroup resolves the VFIO IOMMU group of a mediated device via its
// sysfs iommu_group symlink.
func (h *vgpuCDIHandler) mdevIommuGroup(devUUID string) (string, error) {
	sysfsRoot := h.deviceLib.sysfsRoot
	if sysfsRoot == "" {
		sysfsRoot = "/"
	}
	link := filepath.Join(sysfsMdevDeviceDir(sysfsRoot, devUUID), "iommu_group")
	target, err := filepath.EvalSymlinks(link)
	if err != nil {
		return "", err
	}
	return filepath.Base(target), nil
}
