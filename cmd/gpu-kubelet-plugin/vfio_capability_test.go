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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvlib/pkg/nvpci"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/component-base/featuregate"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/internal/lookup/root"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func TestNewDeviceLibVfioCapability(t *testing.T) {
	for _, gate := range []bool{false, true} {
		for _, mode := range []string{"missing", "empty", "populated", "read error"} {
			t.Run(fmt.Sprintf("gate=%t/%s", gate, mode), func(t *testing.T) {
				setVfioTestFeatureGate(t, featuregates.PassthroughSupport, gate)
				setVfioTestFeatureGate(t, featuregates.DynamicMIG, false)
				hostRoot := t.TempDir()
				path := filepath.Join(hostRoot, kernelIommuGroupPath)
				switch mode {
				case "empty":
					require.NoError(t, os.MkdirAll(path, 0o755))
				case "populated":
					require.NoError(t, os.MkdirAll(filepath.Join(path, "0"), 0o755))
				case "read error":
					require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
					require.NoError(t, os.WriteFile(path, nil, 0o644))
				}
				// Construction locates these files but does not load NVML with DynamicMIG disabled.
				for _, name := range []string{"libnvidia-ml.so.1", "nvidia-smi"} {
					require.NoError(t, os.WriteFile(filepath.Join(hostRoot, name), nil, 0o644))
				}
				lib, err := newDeviceLib(root.New(root.WithDriverRoot(hostRoot)), hostRoot)
				if gate && mode == "read error" {
					require.ErrorContains(t, err, "error checking if IOMMU is enabled")
					var pathErr *os.PathError
					require.ErrorAs(t, err, &pathErr)
					require.Nil(t, lib)
					return
				}
				require.NoError(t, err)
				require.Equal(t, gate && mode == "populated", lib.IsVfioEnabled())
			})
		}
	}
}

// Embed the interfaces so unexpected hardware calls fail in these CPU-only tests.
type emptyVfioTestNVML struct{ nvml.Interface }

func (emptyVfioTestNVML) InitWithFlags(uint32) nvml.Return              { return nvml.SUCCESS }
func (emptyVfioTestNVML) Init() nvml.Return                             { return nvml.SUCCESS }
func (emptyVfioTestNVML) SystemGetDriverVersion() (string, nvml.Return) { return "550.0", nvml.SUCCESS }
func (emptyVfioTestNVML) Shutdown() nvml.Return                         { return nvml.SUCCESS }

type emptyVfioTestDevices struct{ nvdev.Interface }

func (emptyVfioTestDevices) VisitDevices(func(int, nvdev.Device) error) error { return nil }

func TestDeviceStateVfioCapability(t *testing.T) {
	for _, gate := range []bool{false, true} {
		for _, capable := range []bool{false, true} {
			t.Run(fmt.Sprintf("gate=%t/capable=%t", gate, capable), func(t *testing.T) {
				setVfioTestFeatureGate(t, featuregates.PassthroughSupport, gate)
				setVfioTestFeatureGate(t, featuregates.DynamicMIG, false)
				pci := &nvpci.InterfaceMock{GetGPUsFunc: func() ([]*nvpci.NvidiaPCIDevice, error) { return nil, nil }}
				lib := &deviceLib{vfioEnabled: capable, nvmllib: emptyVfioTestNVML{}, Interface: emptyVfioTestDevices{}, nvpci: pci}
				perGPUAllocatable, err := lib.enumerateAllPossibleDevices()
				require.NoError(t, err)
				require.Equal(t, capable, lib.IsVfioEnabled())
				if gate && capable {
					require.Len(t, pci.GetGPUsCalls(), 1)
				} else {
					require.Empty(t, pci.GetGPUsCalls())
					require.Empty(t, perGPUAllocatable.GetAllDevices().GetVfioDevices())
					state := &DeviceState{nvdevlib: lib, perGPUAllocatable: perGPUAllocatable}
					result, err := state.applyConfig(context.Background(), configapi.DefaultVfioDeviceConfig(), nil, nil, nil)
					require.ErrorContains(t, err, "VFIO is unavailable on this node")
					require.Nil(t, result)
				}
			})
		}
	}
}

func TestVfioUnavailableResourceGeneration(t *testing.T) {
	for _, gate := range []bool{false, true} {
		for _, capable := range []bool{false, true} {
			if gate && capable {
				continue
			}
			for _, split := range []bool{false, true} {
				t.Run(fmt.Sprintf("gate=%t/capable=%t/split=%t", gate, capable, split), func(t *testing.T) {
					setVfioTestFeatureGate(t, featuregates.PassthroughSupport, gate)
					setVfioTestFeatureGate(t, featuregates.DynamicMIG, true)
					gpu := &AllocatableDevice{Gpu: &GpuInfo{UUID: "GPU-0000", productName: "NVIDIA Test GPU", brand: "Test", architecture: "Test", cudaComputeCapability: "8.0", driverVersion: "550.0", cudaDriverVersion: "12.0", pciBusID: "0000:00:00.0", vfioEnabled: true}}
					state := &DeviceState{nvdevlib: &deviceLib{vfioEnabled: capable}, perGPUAllocatable: &PerGPUAllocatableDevices{allocatablesMap: map[PCIBusID]AllocatableDevices{gpu.Gpu.pciBusID: {gpu.CanonicalName(): gpu}}}}
					// GPU-local eligibility must not override the feature gate or node capability.
					require.NoError(t, state.discoverSiblingAllocatables(gpu))
					require.Empty(t, state.perGPUAllocatable.GetAllDevices().GetVfioDevices())
					resources := (&driver{state: state, useSplitResourceSlices: split}).GenerateDriverResources("test-node")
					slices := resources.Pools["test-node"].Slices
					expectedSlices := 1
					if split {
						expectedSlices = 2
					}
					require.Len(t, slices, expectedSlices)
					var devices []resourceapi.Device
					for _, slice := range slices {
						devices = append(devices, slice.Devices...)
					}
					require.Len(t, devices, 1)
					require.Equal(t, GpuDeviceType, *devices[0].Attributes["type"].StringValue)
				})
			}
		}
	}
}

func setVfioTestFeatureGate(t *testing.T, gate featuregate.Feature, enabled bool) {
	t.Helper()
	previous := featuregates.Enabled(gate)
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{string(gate): previous}))
	})
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{string(gate): enabled}))
}
