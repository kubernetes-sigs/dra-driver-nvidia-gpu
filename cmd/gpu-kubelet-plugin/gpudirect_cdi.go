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

	nvdevice "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi"
	"github.com/sirupsen/logrus"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

// gpuDirectCDIHandler returns the CDI edits for one GPUDirect path, found by
// one nvcdi mode: the device nodes and files that let something other than the
// GPU driver read or write GPU memory directly (gdrdrv for GDRCopy, nvidia-fs
// for GDS). It is not a DRA device; its edits are added to every GPU claim on
// nodes that have them.
type gpuDirectCDIHandler struct {
	mode nvcdi.Mode
	lib  nvcdi.Interface
}

func NewGPUDirectCDIHandler(mode nvcdi.Mode, nvdevice nvdevice.Interface, nvmllib nvml.Interface, driverRoot, devRoot, nvidiaCDIHookPath string, logger *logrus.Logger) (*gpuDirectCDIHandler, error) {
	lib, err := nvcdi.New(
		nvcdi.WithDeviceLib(nvdevice),
		nvcdi.WithDriverRoot(driverRoot),
		nvcdi.WithDevRoot(devRoot),
		nvcdi.WithLogger(logger),
		nvcdi.WithNvmlLib(nvmllib),
		nvcdi.WithMode(mode),
		nvcdi.WithNVIDIACDIHookPath(nvidiaCDIHookPath),
		nvcdi.WithFeatureFlags(nvcdi.FeatureDisableNvsandboxUtils),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to create CDI library for mode %q: %w", mode, err)
	}
	return &gpuDirectCDIHandler{mode: mode, lib: lib}, nil
}

// GetCDIContainerEdits returns empty edits on nodes without the devices.
func (h *gpuDirectCDIHandler) GetCDIContainerEdits() (*cdiapi.ContainerEdits, error) {
	// Not cached, so device nodes created after the plugin started are picked
	// up by the next prepare.
	devs, err := h.lib.GetDeviceSpecsByID("all")
	if err != nil {
		return nil, fmt.Errorf("failed to get %s CDI edits: %w", h.mode, err)
	}
	edits := &cdiapi.ContainerEdits{ContainerEdits: &cdispec.ContainerEdits{}}
	for _, dev := range devs {
		edits = edits.Append(&cdiapi.ContainerEdits{ContainerEdits: &dev.ContainerEdits})
	}
	return edits, nil
}

// appendGPUDirectEdits returns the common edits plus the GDRCopy and GDS
// edits. It copies into a new struct because the common edits are cached.
func (cdi *CDIHandler) appendGPUDirectEdits(commonEdits *cdiapi.ContainerEdits) (*cdiapi.ContainerEdits, error) {
	edits := (&cdiapi.ContainerEdits{}).Append(commonEdits)
	// GPUDirect RDMA NICs (nvcdi mofed mode) are left to a NIC DRA driver.
	for _, h := range []*gpuDirectCDIHandler{cdi.gdrcopycdi, cdi.gdscdi} {
		gpuDirectEdits, err := h.GetCDIContainerEdits()
		if err != nil {
			return nil, err
		}
		edits = edits.Append(gpuDirectEdits)
	}
	return edits, nil
}
