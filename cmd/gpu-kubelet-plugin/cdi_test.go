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
	"testing"

	"github.com/stretchr/testify/require"

	nvcdspec "github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi/spec"
	resourceapi "k8s.io/api/resource/v1"
	utilcache "k8s.io/apimachinery/pkg/util/cache"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

type fakeNVCDI struct {
	getCommonEdits  func() (*cdiapi.ContainerEdits, error)
	getDeviceSpecs  func(...string) ([]cdispec.Device, error)
	commonEditCalls int
	deviceSpecCalls int
}

func (f *fakeNVCDI) GetSpec(...string) (nvcdspec.Interface, error) {
	return nil, nil
}

func (f *fakeNVCDI) GetCommonEdits() (*cdiapi.ContainerEdits, error) {
	f.commonEditCalls++
	return f.getCommonEdits()
}

func (f *fakeNVCDI) GetDeviceSpecsByID(ids ...string) ([]cdispec.Device, error) {
	f.deviceSpecCalls++
	return f.getDeviceSpecs(ids...)
}

func (f *fakeNVCDI) GetAllDeviceSpecs() ([]cdispec.Device, error) {
	return f.GetDeviceSpecsByID("all")
}

func containerEdits(paths ...string) *cdiapi.ContainerEdits {
	var nodes []*cdispec.DeviceNode
	for _, path := range paths {
		nodes = append(nodes, &cdispec.DeviceNode{Path: path})
	}
	return &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{DeviceNodes: nodes},
	}
}

func deviceSpecs(paths ...string) []cdispec.Device {
	return []cdispec.Device{{
		Name:           "gpu",
		ContainerEdits: *containerEdits(paths...).ContainerEdits,
	}}
}

func TestRequiresNVIDIADeviceNodes(t *testing.T) {
	testCases := map[string]struct {
		devices  PreparedDevices
		required bool
	}{
		"no devices": {},
		"GPU": {
			devices: PreparedDevices{
				{Devices: PreparedDeviceList{
					{Gpu: &PreparedGpu{}},
				}},
			},
			required: true,
		},
		"MIG": {
			devices: PreparedDevices{
				{Devices: PreparedDeviceList{
					{Mig: &PreparedMigDevice{}},
				}},
			},
			required: true,
		},
		"VFIO": {
			devices: PreparedDevices{
				{Devices: PreparedDeviceList{
					{Vfio: &PreparedVfioDevice{}},
				}},
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.required, requiresNVIDIADeviceNodes(tc.devices))
		})
	}
}

func TestValidateClaimCDIDeviceNodes(t *testing.T) {
	devices := AllocatableDevices{
		"gpu-0": {
			Gpu: &GpuInfo{UUID: "GPU-0", minor: 0},
		},
		"mig-static": {
			MigStatic: &MigDeviceInfo{ParentUUID: "GPU-1", ParentMinor: 1},
		},
		"mig-dynamic": {
			MigDynamic: &MigSpec{Parent: &GpuInfo{UUID: "GPU-2", minor: 2}},
		},
		"vfio-0": {
			Vfio: &VfioDeviceInfo{},
		},
	}
	testCases := []struct {
		name             string
		deviceName       DeviceName
		expectedCDICalls int
	}{
		{name: "GPU", deviceName: "gpu-0", expectedCDICalls: 1},
		{name: "static MIG", deviceName: "mig-static", expectedCDICalls: 1},
		{name: "dynamic MIG", deviceName: "mig-dynamic", expectedCDICalls: 1},
		{name: "VFIO", deviceName: "vfio-0"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeNVCDI{
				getCommonEdits: func() (*cdiapi.ContainerEdits, error) {
					return containerEdits("/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools"), nil
				},
				getDeviceSpecs: func(...string) ([]cdispec.Device, error) {
					return deviceSpecs("/dev/nvidia0"), nil
				},
			}
			state := &DeviceState{
				cdi: &CDIHandler{
					nvcdiClaim: fake,
					specCache:  utilcache.NewExpiring(),
				},
				perGPUAllocatable: &PerGPUAllocatableDevices{
					allocatablesMap: map[PCIBusID]AllocatableDevices{"pci": devices},
				},
			}
			claim := &resourceapi.ResourceClaim{
				Status: resourceapi.ResourceClaimStatus{
					Allocation: &resourceapi.AllocationResult{
						Devices: resourceapi.DeviceAllocationResult{
							Results: []resourceapi.DeviceRequestAllocationResult{{
								Driver: DriverName,
								Device: tc.deviceName,
							}},
						},
					},
				},
			}

			require.NoError(t, state.validateClaimCDIDeviceNodes(claim))
			require.Equal(t, tc.expectedCDICalls, fake.commonEditCalls)
			require.Equal(t, tc.expectedCDICalls, fake.deviceSpecCalls)
		})
	}
}

func TestValidateCommonDeviceNodes(t *testing.T) {
	required := []string{"/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools"}
	require.NoError(t, validateCommonDeviceNodes(containerEdits(required...)))
	require.Error(t, validateCommonDeviceNodes(nil))

	testCases := map[string]string{
		"missing control node":   "/dev/nvidiactl",
		"missing UVM node":       "/dev/nvidia-uvm",
		"missing UVM tools node": "/dev/nvidia-uvm-tools",
	}
	for name, missing := range testCases {
		t.Run(name, func(t *testing.T) {
			var paths []string
			for _, path := range required {
				if path != missing {
					paths = append(paths, path)
				}
			}
			require.ErrorContains(t, validateCommonDeviceNodes(containerEdits(paths...)), missing)
		})
	}

	wrongRoot := append([]string(nil), required...)
	wrongRoot[0] = "/tmp/nvidiactl"
	require.ErrorContains(t, validateCommonDeviceNodes(containerEdits(wrongRoot...)), "/dev/nvidiactl")
}

func TestGetCommonEditsCachedRejectsIncompleteEdits(t *testing.T) {
	fake := &fakeNVCDI{
		getCommonEdits: func() (*cdiapi.ContainerEdits, error) {
			return containerEdits(), nil
		},
	}
	handler := &CDIHandler{
		nvcdiClaim: fake,
		specCache:  utilcache.NewExpiring(),
	}

	_, err := handler.GetCommonEditsCached()
	require.ErrorContains(t, err, "failed to validate NVIDIA CDI common edits")
	_, err = handler.GetCommonEditsCached()
	require.ErrorContains(t, err, "failed to validate NVIDIA CDI common edits")
	require.Equal(t, 2, fake.commonEditCalls, "incomplete common edits must not be cached")
}

func TestGetCommonEditsCachedCachesCompleteEdits(t *testing.T) {
	fake := &fakeNVCDI{
		getCommonEdits: func() (*cdiapi.ContainerEdits, error) {
			return containerEdits("/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools"), nil
		},
	}
	handler := &CDIHandler{
		nvcdiClaim: fake,
		specCache:  utilcache.NewExpiring(),
	}

	_, err := handler.GetCommonEditsCached()
	require.NoError(t, err)
	_, err = handler.GetCommonEditsCached()
	require.NoError(t, err)
	require.Equal(t, 1, fake.commonEditCalls)
}

func TestGetDeviceSpecsByUUIDCached(t *testing.T) {
	t.Run("complete specs are cached", func(t *testing.T) {
		fake := &fakeNVCDI{
			getDeviceSpecs: func(...string) ([]cdispec.Device, error) {
				return deviceSpecs("/dev/nvidia0"), nil
			},
		}
		handler := &CDIHandler{
			nvcdiClaim: fake,
			specCache:  utilcache.NewExpiring(),
		}

		_, err := handler.GetDeviceSpecsByUUIDCached("GPU-0")
		require.NoError(t, err)
		_, err = handler.GetDeviceSpecsByUUIDCached("GPU-0")
		require.NoError(t, err)
		require.Equal(t, 1, fake.deviceSpecCalls)
	})

	t.Run("incomplete specs are rejected and not cached", func(t *testing.T) {
		testCases := map[string]string{
			"missing":       "",
			"wrong root":    "/tmp/nvidia0",
			"control node":  "/dev/nvidiactl",
			"missing minor": "/dev/nvidia",
			"invalid minor": "/dev/nvidia-1",
		}
		for name, path := range testCases {
			t.Run(name, func(t *testing.T) {
				fake := &fakeNVCDI{
					getDeviceSpecs: func(...string) ([]cdispec.Device, error) {
						return deviceSpecs(path), nil
					},
				}
				handler := &CDIHandler{
					nvcdiClaim: fake,
					specCache:  utilcache.NewExpiring(),
				}

				_, err := handler.GetDeviceSpecsByUUIDCached("GPU-0")
				require.ErrorContains(t, err, "failed to validate NVIDIA CDI device spec")
				_, err = handler.GetDeviceSpecsByUUIDCached("GPU-0")
				require.ErrorContains(t, err, "failed to validate NVIDIA CDI device spec")
				require.Equal(t, 2, fake.deviceSpecCalls, "incomplete device specs must not be cached")
			})
		}
	})
}

func TestInvalidateDeviceSpec(t *testing.T) {
	devicePath := "/dev/nvidia0"
	fake := &fakeNVCDI{
		getDeviceSpecs: func(...string) ([]cdispec.Device, error) {
			return deviceSpecs(devicePath), nil
		},
	}
	handler := &CDIHandler{
		nvcdiClaim: fake,
		specCache:  utilcache.NewExpiring(),
	}

	specs, err := handler.GetDeviceSpecsByUUIDCached("GPU-0")
	require.NoError(t, err)
	require.Equal(t, "/dev/nvidia0", specs[0].ContainerEdits.DeviceNodes[0].Path)

	devicePath = "/dev/nvidia1"
	handler.InvalidateDeviceSpec("GPU-0")

	specs, err = handler.GetDeviceSpecsByUUIDCached("GPU-0")
	require.NoError(t, err)
	require.Equal(t, "/dev/nvidia1", specs[0].ContainerEdits.DeviceNodes[0].Path)
	require.Equal(t, 2, fake.deviceSpecCalls)
}
