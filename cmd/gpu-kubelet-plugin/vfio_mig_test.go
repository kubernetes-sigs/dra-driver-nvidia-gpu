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
	"slices"
	"testing"

	"github.com/NVIDIA/go-nvlib/pkg/nvpassthrough"
	"github.com/NVIDIA/go-nvlib/pkg/nvpci"
	"github.com/stretchr/testify/require"
)

// Embed the interface so unexpected passthrough calls fail in these CPU-only tests.
type vfioTestPassthrough struct{ nvpassthrough.Interface }

func (vfioTestPassthrough) FindBestVFIOVariant(string) (string, error) { return "vfio_pci", nil }

type vfioTestGPU struct {
	pciBusID   string
	migEnabled bool
	migDevices int
}

// newVfioTestState returns a deviceLib and allocatables in the state that
// GetPerGpuAllocatableDevices leaves them in for the given GPUs (with
// PassthroughSupport enabled). nvpci reports a PCI device for each GPU, plus
// one for each of notInNVML (GPUs that NVML does not see, e.g. because they
// are bound to vfio-pci at startup).
func newVfioTestState(t *testing.T, gpus []vfioTestGPU, notInNVML ...string) (*deviceLib, *PerGPUAllocatableDevices) {
	t.Helper()
	lib := &deviceLib{
		hostRoot:          t.TempDir(),
		nvpasst:           vfioTestPassthrough{},
		gpuInfosByUUID:    make(map[string]*GpuInfo),
		gpuUUIDbyPCIBusID: make(map[PCIBusID]string),
	}
	perGPUAllocatable := &PerGPUAllocatableDevices{allocatablesMap: make(map[PCIBusID]AllocatableDevices)}

	var pciDevices []*nvpci.NvidiaPCIDevice
	for i, g := range gpus {
		gpuInfo := &GpuInfo{
			UUID:        fmt.Sprintf("GPU-%d", i),
			minor:       i,
			pciBusID:    g.pciBusID,
			migEnabled:  g.migEnabled,
			vfioEnabled: g.migDevices == 0,
		}
		lib.gpuInfosByUUID[gpuInfo.UUID] = gpuInfo
		lib.gpuUUIDbyPCIBusID[gpuInfo.pciBusID] = gpuInfo.UUID

		// A GPU in MIG mode is announced through its MIG devices only.
		allocatables := make(AllocatableDevices)
		if !g.migEnabled {
			allocatables[gpuInfo.CanonicalName()] = &AllocatableDevice{Gpu: gpuInfo}
		}
		for j := range g.migDevices {
			allocatables[fmt.Sprintf("gpu-%d-mig-%d", i, j)] = &AllocatableDevice{
				MigStatic: &MigDeviceInfo{UUID: fmt.Sprintf("MIG-%d-%d", i, j), Profile: "1g.18gb", parent: gpuInfo},
			}
		}
		require.NoError(t, perGPUAllocatable.AddGPUAllocatables(gpuInfo.pciBusID, allocatables))
		pciDevices = append(pciDevices, &nvpci.NvidiaPCIDevice{Address: g.pciBusID})
	}
	for _, pciBusID := range notInNVML {
		pciDevices = append(pciDevices, &nvpci.NvidiaPCIDevice{Address: pciBusID})
	}
	lib.nvpci = &nvpci.InterfaceMock{GetGPUsFunc: func() ([]*nvpci.NvidiaPCIDevice, error) { return pciDevices, nil }}

	return lib, perGPUAllocatable
}

func TestEnumerateGpuVfioDevicesSkipsGPUsWithMigDevices(t *testing.T) {
	tests := map[string]struct {
		gpus      []vfioTestGPU
		notInNVML []string
		wantVfio  []string
	}{
		"GPU with MIG devices and GPU without MIG": {
			gpus: []vfioTestGPU{
				{pciBusID: "0000:ae:00.0", migEnabled: true, migDevices: 4},
				{pciBusID: "0000:d8:00.0"},
			},
			wantVfio: []string{"0000:d8:00.0"},
		},
		"all GPUs have MIG devices": {
			gpus: []vfioTestGPU{
				{pciBusID: "0000:ae:00.0", migEnabled: true, migDevices: 4},
				{pciBusID: "0000:b0:00.0", migEnabled: true, migDevices: 2},
			},
			wantVfio: nil,
		},
		"MIG mode enabled without MIG devices": {
			gpus: []vfioTestGPU{
				{pciBusID: "0000:ae:00.0", migEnabled: true},
			},
			wantVfio: []string{"0000:ae:00.0"},
		},
		"GPU not seen by NVML": {
			gpus: []vfioTestGPU{
				{pciBusID: "0000:d8:00.0"},
			},
			notInNVML: []string{"0000:ae:00.0"},
			wantVfio:  []string{"0000:ae:00.0", "0000:d8:00.0"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			lib, perGPUAllocatable := newVfioTestState(t, tc.gpus, tc.notInNVML...)

			require.NoError(t, lib.enumerateGpuVfioDevices(perGPUAllocatable))

			var gotVfio []string
			for _, dev := range perGPUAllocatable.GetAllDevices().GetVfioDevices() {
				gotVfio = append(gotVfio, dev.Vfio.PciBusID)

				// Every GPU seen by NVML is linked as the parent, including
				// one in MIG mode that has no full-GPU allocatable.
				if slices.Contains(tc.notInNVML, dev.Vfio.PciBusID) {
					require.Nil(t, dev.Vfio.parent)
				} else {
					require.NotNil(t, dev.Vfio.parent)
					require.Equal(t, dev.Vfio.PciBusID, dev.Vfio.parent.pciBusID)
				}
			}
			slices.Sort(gotVfio)
			require.Equal(t, tc.wantVfio, gotVfio)
		})
	}
}

func TestAddRediscoveredGPU(t *testing.T) {
	for _, migEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("migEnabled=%t", migEnabled), func(t *testing.T) {
			gpu := &AllocatableDevice{Gpu: &GpuInfo{UUID: "GPU-0", pciBusID: "0000:ae:00.0", migEnabled: migEnabled}}
			state := &DeviceState{perGPUAllocatable: &PerGPUAllocatableDevices{allocatablesMap: make(map[PCIBusID]AllocatableDevices)}}

			require.NoError(t, state.addRediscoveredGPU(gpu))

			// Like startup discovery, a GPU in MIG mode is not announced as a full GPU.
			if migEnabled {
				require.Nil(t, state.perGPUAllocatable.GetAllocatableDevice(gpu.Gpu.CanonicalName()))
			} else {
				require.Same(t, gpu, state.perGPUAllocatable.GetAllocatableDevice(gpu.Gpu.CanonicalName()))
			}
		})
	}
}
