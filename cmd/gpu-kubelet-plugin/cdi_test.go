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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	nvcdspec "github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi/spec"
	resourceapi "k8s.io/api/resource/v1"
	utilcache "k8s.io/apimachinery/pkg/util/cache"
	"k8s.io/utils/ptr"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"sigs.k8s.io/dra-driver-nvidia-gpu/internal/common"
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
			"missing":           "",
			"wrong root":        "/tmp/nvidia0",
			"control node":      "/dev/nvidiactl",
			"missing minor":     "/dev/nvidia",
			"negative minor":    "/dev/nvidia-1",
			"leading zero":      "/dev/nvidia01",
			"double zero":       "/dev/nvidia00",
			"three-digit minor": "/dev/nvidia100",
			"MIG capability":    "/dev/nvidia-caps/nvidia-cap30",
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

func TestCreateClaimSpecFileRejectsMismatchedGPUDeviceMinor(t *testing.T) {
	fake := &fakeNVCDI{
		getCommonEdits: func() (*cdiapi.ContainerEdits, error) {
			return containerEdits("/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools"), nil
		},
		getDeviceSpecs: func(...string) ([]cdispec.Device, error) {
			return deviceSpecs("/dev/nvidia1"), nil
		},
	}
	handler := &CDIHandler{
		nvcdiClaim: fake,
		specCache:  utilcache.NewExpiring(),
		cdiRoot:    t.TempDir(),
	}
	preparedDevices := PreparedDevices{{
		Devices: PreparedDeviceList{{
			Gpu: &PreparedGpu{
				Info: &GpuInfo{UUID: "GPU-0", minor: 0},
				Device: &CheckpointedDevice{
					DeviceName: "gpu-0",
				},
			},
		}},
	}}

	err := handler.CreateClaimSpecFile("claim", preparedDevices)
	require.ErrorContains(t, err, "expected device node /dev/nvidia0")
	require.Equal(t, 2, fake.deviceSpecCalls)
}

func TestGetDeviceSpecsByUUIDAndMinorRefreshesStaleCache(t *testing.T) {
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

	_, err := handler.GetDeviceSpecsByUUIDCached("GPU-0")
	require.NoError(t, err)

	devicePath = "/dev/nvidia1"
	specs, err := handler.getDeviceSpecsByUUIDAndMinor("GPU-0", 1)
	require.NoError(t, err)
	require.Equal(t, "/dev/nvidia1", specs[0].ContainerEdits.DeviceNodes[0].Path)
	require.Equal(t, 2, fake.deviceSpecCalls)
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

// useProcDevicesPath points the nvcaps helpers at path for the duration of the
// test. "/proc/devices" selects the real-driver behaviour regardless of any
// ALT_PROC_DEVICES_PATH in the test environment.
func useProcDevicesPath(t *testing.T, path string) {
	t.Helper()
	common.ConfigureProcDevicesPath(path)
	t.Cleanup(common.ResetProcDevicesPath)
}

func TestNVCapsRoot(t *testing.T) {
	tests := map[string]struct {
		procDevicesPath string
		driverRoot      string
		expected        string
	}{
		"real driver reads procfs": {
			procDevicesPath: "/proc/devices",
			driverRoot:      "/driver-root",
			expected:        "/proc/driver/nvidia/capabilities",
		},
		"mock NVML reads under the driver root": {
			procDevicesPath: "/alt-proc-devices",
			driverRoot:      "/driver-root",
			expected:        "/driver-root/proc/driver/nvidia/capabilities",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			useProcDevicesPath(t, tc.procDevicesPath)
			handler := &CDIHandler{driverRoot: tc.driverRoot}
			require.Equal(t, tc.expected, handler.nvCapsRoot())
		})
	}
}

// Regression test: under mock NVML, MIG capability files exist only under the
// driver root, so preparing a MIG device must not read /proc/driver/nvidia.
func TestGetDevNodesForMigDeviceReadsCapsUnderDriverRootWithMockNVML(t *testing.T) {
	procDevices := filepath.Join(t.TempDir(), "proc-devices")
	require.NoError(t, os.WriteFile(procDevices,
		[]byte("Character devices:\n195 nvidia-frontend\n236 nvidia-caps\n\nBlock devices:\n  8 sd\n"), 0o644))
	useProcDevicesPath(t, procDevices)

	driverRoot := t.TempDir()
	giDir := filepath.Join(driverRoot, "proc/driver/nvidia/capabilities/gpu2/mig/gi5")
	require.NoError(t, os.MkdirAll(filepath.Join(giDir, "ci0"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(giDir, "access"),
		[]byte("DeviceFileMinor: 35\nDeviceFileMode: 292\nDeviceFileModify: 1\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(giDir, "ci0", "access"),
		[]byte("DeviceFileMinor: 36\nDeviceFileMode: 292\nDeviceFileModify: 1\n"), 0o644))

	handler := &CDIHandler{driverRoot: driverRoot}
	nodes, err := handler.GetDevNodesForMigDevice(&MigLiveTuple{ParentMinor: 2, GIID: 5, CIID: 0})
	require.NoError(t, err)
	require.Equal(t, []*cdispec.DeviceNode{
		{Path: "/dev/nvidia-caps/nvidia-cap35", Type: "c", FileMode: ptr.To(os.FileMode(292)), Major: 236, Minor: 35},
		{Path: "/dev/nvidia-caps/nvidia-cap36", Type: "c", FileMode: ptr.To(os.FileMode(292)), Major: 236, Minor: 36},
	}, nodes)
}
