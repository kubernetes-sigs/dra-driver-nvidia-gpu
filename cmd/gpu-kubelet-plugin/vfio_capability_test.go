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
	"github.com/NVIDIA/go-nvlib/pkg/nvpassthrough"
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
				lib, err := newDeviceLib(root.New(root.WithDriverRoot(hostRoot)), hostRoot, false)
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

func TestVfioOnlyStartupWithoutNvidiaDriver(t *testing.T) {
	setVfioTestFeatureGate(t, featuregates.PassthroughSupport, true)
	setVfioTestFeatureGate(t, featuregates.DynamicMIG, false)
	setVfioTestFeatureGate(t, featuregates.FabricManagerPartitioning, false)
	hostRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(hostRoot, kernelIommuGroupPath, "0"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(hostRoot, pciDevicesPath), 0o755))
	config := &Config{flags: &Flags{
		vfioOnly: true, hostRoot: hostRoot, containerDriverRoot: t.TempDir(),
		kubeletPluginsDirectoryPath: t.TempDir(), cdiRoot: t.TempDir(),
	}}
	for range 2 {
		// A second start exercises recovery of the checkpoint without NVIDIA libraries.
		state, err := NewDeviceState(context.Background(), config)
		require.NoError(t, err)
		require.True(t, state.nvdevlib.vfioOnly)
		require.False(t, state.vfioPciManager.nvidiaEnabled)
		require.Empty(t, state.nvdevlib.driverLibraryPath)
		require.Empty(t, state.nvdevlib.nvidiaSMIPath)
	}
}

func TestVfioOnlyPrerequisites(t *testing.T) {
	for _, mode := range []string{"no gate", "no iommu", "nvswitch", "nvlink5", "pci error"} {
		t.Run(mode, func(t *testing.T) {
			setVfioTestFeatureGate(t, featuregates.PassthroughSupport, mode != "no gate")
			setVfioTestFeatureGate(t, featuregates.DynamicMIG, false)
			hostRoot := t.TempDir()
			if mode != "no iommu" {
				require.NoError(t, os.MkdirAll(filepath.Join(hostRoot, kernelIommuGroupPath, "0"), 0o755))
			}
			if mode != "pci error" {
				require.NoError(t, os.MkdirAll(filepath.Join(hostRoot, pciDevicesPath), 0o755))
			}
			if mode == "nvswitch" {
				devicePath := filepath.Join(hostRoot, pciDevicesPath, "0000:01:00.0")
				require.NoError(t, os.MkdirAll(devicePath, 0o755))
				for name, value := range map[string]string{"vendor": "0x10de", "device": "0x22a3", "class": "0x068000", "resource": "0 0 0", "numa_node": "0"} {
					require.NoError(t, os.WriteFile(filepath.Join(devicePath, name), []byte(value), 0o644))
				}
			}
			if mode == "nvlink5" {
				devicePath := filepath.Join(hostRoot, "sys/class/infiniband/mlx5_0/device")
				require.NoError(t, os.MkdirAll(devicePath, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(devicePath, "vpd"), []byte("SW_MNG"), 0o644))
			}
			_, err := newDeviceLib(root.New(root.WithDriverRoot(t.TempDir())), hostRoot, true)
			require.Error(t, err)
			if mode == "nvswitch" || mode == "nvlink5" {
				require.ErrorContains(t, err, "unsupported on nodes with NVSwitch/NVLink fabric")
			}
		})
	}
}

type vfioOnlyTestPassthrough struct{ nvpassthrough.Interface }

func (vfioOnlyTestPassthrough) FindBestVFIOVariant(string) (string, error) {
	return "vfio-pci", nil
}

func TestVfioOnlyDiscoveryAndRelease(t *testing.T) {
	setVfioTestFeatureGate(t, featuregates.PassthroughSupport, true)
	setVfioTestFeatureGate(t, featuregates.DynamicMIG, false)
	setVfioTestFeatureGate(t, featuregates.FabricManagerPartitioning, false)
	pci := &nvpci.InterfaceMock{GetGPUsFunc: func() ([]*nvpci.NvidiaPCIDevice, error) {
		return []*nvpci.NvidiaPCIDevice{{
			Address: "0000:01:00.0", Vendor: 0x10de, Device: 0x2330,
			Resources: nvpci.MemoryResources{}, IommuGroup: 1,
		}}, nil
	}}
	// Any NVML or nvdev call panics, proving discovery and release use only PCI.
	lib := &deviceLib{
		vfioOnly: true, vfioEnabled: true, hostRoot: t.TempDir(),
		nvmllib: &mockNVMLLibrary{}, Interface: nil,
		nvpci: pci, nvpasst: vfioOnlyTestPassthrough{},
	}
	devices, err := lib.enumerateAllPossibleDevices()
	require.NoError(t, err)
	all := devices.GetAllDevices()
	require.Len(t, all, 1)
	state := &DeviceState{
		nvdevlib: lib, perGPUAllocatable: devices, config: &Config{flags: &Flags{}},
		vfioPciManager: NewVfioPciManager("", "", lib, false),
	}
	for _, device := range all {
		require.Equal(t, VfioDeviceType, device.Type())
		require.Nil(t, device.Vfio.parent)
		require.NoError(t, state.vfioPciManager.Unconfigure(context.Background(), device.Vfio))
		require.NoError(t, state.discoverSiblingAllocatables(device))
	}
	var published []resourceapi.Device
	for _, device := range devices.GetAllDevices() {
		// Passthrough publication uses GetDevice, not DynamicMIG's PartGetDevice.
		published = append(published, device.GetDevice(state.config))
	}
	require.Len(t, published, 1)
	require.Equal(t, VfioDeviceType, *published[0].Attributes["type"].StringValue)
}
