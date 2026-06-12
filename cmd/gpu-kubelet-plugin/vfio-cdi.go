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
	"strings"

	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

type vfioCDIHandler struct {
	deviceLib      *deviceLib
	iommuFDEnabled bool
}

func NewVfioCDIHandler(deviceLib *deviceLib) (*vfioCDIHandler, error) {
	iommuFDEnabled, err := checkIommuFDEnabled(deviceLib.hostRoot)
	if err != nil {
		return nil, err
	}

	handler := &vfioCDIHandler{
		deviceLib:      deviceLib,
		iommuFDEnabled: iommuFDEnabled,
	}
	return handler, nil
}

// GetCommonEdits returns the common CDI container edits required by all vfio device(s)
// in the claim.
//
// We currently only support 2 IOMMU backend policies: LegacyOnly and PreferIommuFD.
// We automatically assume we want the legacy device if PreferIommuFD policy is not selected.
// If more policies are added in the future, the handler needs to be enhanced to support them.
func (h *vfioCDIHandler) GetCommonEdits(enableAPIDevice bool, preferIommuFD bool) (*cdiapi.ContainerEdits, error) {
	edits := &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			// Make sure that NVIDIA_VISIBLE_DEVICES is set to void to avoid the
			// nvidia-container-runtime honoring it in addition to the underlying
			// runtime honoring CDI.
			Env:         []string{"NVIDIA_VISIBLE_DEVICES=void"},
			DeviceNodes: make([]*cdispec.DeviceNode, 0),
		},
	}

	// IOMMU API device is not requested. Exit early.
	if !enableAPIDevice {
		return edits, nil
	}

	// Use IOMMUFD if it is supported and preferred.
	if preferIommuFD && h.iommuFDEnabled {
		edits.DeviceNodes = append(edits.DeviceNodes, &cdispec.DeviceNode{
			Path: iommuDevicePath,
		})
	} else {
		edits.DeviceNodes = append(edits.DeviceNodes, &cdispec.DeviceNode{
			Path: filepath.Join(vfioDevicesRoot, "vfio"),
		})
	}

	return edits, nil
}

// GetDeviceSpecsByPCIBusID returns the CDI spec for a container to have access to the GPU
// while bound on vfio-pci driver.
//
// We currently only support 2 IOMMU backend policies: LegacyOnly and PreferIommuFD.
// We automatically assume we want the legacy device if PreferIommuFD policy is not selected.
// If more policies are added in the future, the handler needs to be enhanced to support them.
func (h *vfioCDIHandler) GetDeviceSpecsByPCIBusID(pciBusID string, preferIommuFD bool) ([]cdispec.Device, error) {
	pciDeviceInfo, err := h.deviceLib.nvpci.GetGPUByPciBusID(pciBusID)
	if err != nil {
		return nil, fmt.Errorf("error getting PCI device info for GPU %q: %w", pciBusID, err)
	}

	devNodes := make([]*cdispec.DeviceNode, 0)

	if preferIommuFD && h.iommuFDEnabled {
		// The IOMMUFD cdev is located at /dev/vfio/devices/<vfioX> and is
		// expected to be available if IOMMUFD is enabled on the node and the GPU is
		// bound to the vfio driver.
		if !strings.HasPrefix(pciDeviceInfo.IommuFD, "vfio") {
			return nil, fmt.Errorf("missing iommufd cdev for GPU %q", pciDeviceInfo.Address)
		}
		devNodes = append(devNodes, &cdispec.DeviceNode{
			Path: filepath.Join(vfioDevicesPath, pciDeviceInfo.IommuFD),
		})
	} else {
		devNodes = append(devNodes, &cdispec.DeviceNode{
			Path: filepath.Join(vfioDevicesRoot, fmt.Sprintf("%d", pciDeviceInfo.IommuGroup)),
		})
	}
	devSpecs := []cdispec.Device{
		{
			ContainerEdits: cdispec.ContainerEdits{
				DeviceNodes: devNodes,
			},
		},
	}
	return devSpecs, nil
}
