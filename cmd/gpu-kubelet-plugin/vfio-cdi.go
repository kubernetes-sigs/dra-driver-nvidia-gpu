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
	"os"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/go-nvlib/pkg/nvpci"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

type vfioCDIHandler struct {
	iommuFDEnabled bool
}

func NewVfioCDIHandler() (*vfioCDIHandler, error) {
	iommuFDEnabled, err := checkIommuFDEnabled()
	if err != nil {
		return nil, err
	}

	handler := &vfioCDIHandler{
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

	// Always include /dev/vfio/vfio for legacy VFIO — it's required by
	// libvirt to detect VFIO support. enableAPIDevice controls whether the
	// preferred IOMMU backend device is also added.
	edits.DeviceNodes = append(edits.DeviceNodes, &cdispec.DeviceNode{
		Path: filepath.Join(vfioDevicesRoot, "vfio"),
	})

	if !enableAPIDevice {
		return edits, nil
	}

	// Use IOMMUFD if it is supported and preferred.
	if preferIommuFD && h.iommuFDEnabled {
		edits.DeviceNodes = append(edits.DeviceNodes, &cdispec.DeviceNode{
			Path: iommuDevicePath,
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
	devNodes := make([]*cdispec.DeviceNode, 0)

	if preferIommuFD && h.iommuFDEnabled {
		nvpci := nvpci.New()
		pciDeviceInfo, err := nvpci.GetGPUByPciBusID(pciBusID)
		if err != nil {
			return nil, fmt.Errorf("error getting PCI device info for GPU %q: %w", pciBusID, err)
		}
		if !strings.HasPrefix(pciDeviceInfo.IommuFD, "vfio") {
			return nil, fmt.Errorf("missing iommufd cdev for GPU %q", pciDeviceInfo.Address)
		}
		devNodes = append(devNodes, &cdispec.DeviceNode{
			Path: filepath.Join(vfioDevicesPath, pciDeviceInfo.IommuFD),
		})
	} else {
		// Read IOMMU group directly from sysfs — works for GPUs on any
		// driver including vfio-pci (nvpci may not find vfio-bound GPUs).
		iommuGroup, err := getIommuGroupFromSysfs(pciBusID)
		if err != nil {
			return nil, fmt.Errorf("error getting IOMMU group for GPU %q: %w", pciBusID, err)
		}
		devNodes = append(devNodes, &cdispec.DeviceNode{
			Path: filepath.Join(vfioDevicesRoot, iommuGroup),
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

func getIommuGroupFromSysfs(pciBusID string) (string, error) {
	iommuLink := filepath.Join(pciDevicesPath, pciBusID, "iommu_group")
	target, err := os.Readlink(iommuLink)
	if err != nil {
		return "", fmt.Errorf("failed to read IOMMU group symlink for %s: %w", pciBusID, err)
	}
	return filepath.Base(target), nil
}
