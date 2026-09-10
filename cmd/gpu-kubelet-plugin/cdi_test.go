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
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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

	err := handler.CreateClaimSpecFile("claim", preparedDevices, nil)
	require.ErrorContains(t, err, "expected device node /dev/nvidia0")
	require.Equal(t, 2, fake.deviceSpecCalls)
}

func TestCreateClaimSpecFileMergesClaimContainerEdits(t *testing.T) {
	fake := &fakeNVCDI{
		getCommonEdits: func() (*cdiapi.ContainerEdits, error) {
			return containerEdits("/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools"), nil
		},
		getDeviceSpecs: func(...string) ([]cdispec.Device, error) {
			return deviceSpecs("/dev/nvidia0"), nil
		},
	}
	cdiRoot := t.TempDir()
	handler := &CDIHandler{
		nvcdiClaim: fake,
		specCache:  utilcache.NewExpiring(),
		cdiRoot:    cdiRoot,
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
	ipcInfo := &LocalIPCInfo{HostPath: t.TempDir(), ContainerPath: localIPCContainerPath}

	err := handler.CreateClaimSpecFile("claim", preparedDevices, ipcInfo.GetCDIContainerEdits())
	require.NoError(t, err)

	specName := cdiapi.GenerateTransientSpecName(cdiVendor, cdiClaimClass, "claim")
	spec, err := cdiapi.ReadSpec(filepath.Join(cdiRoot, specName+".yaml"), 0)
	require.NoError(t, err)
	edits := spec.ContainerEdits
	require.Contains(t, edits.Env, "NVIDIA_DRA_LOCAL_IPC_DIR="+localIPCContainerPath)
	require.Len(t, edits.Mounts, 1)
	require.Equal(t, ipcInfo.HostPath, edits.Mounts[0].HostPath)
	require.Equal(t, localIPCContainerPath, edits.Mounts[0].ContainerPath)
	// Claim edits are appended to, not substituted for, the common NVIDIA edits.
	require.Len(t, edits.DeviceNodes, 3)
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

// Regression test: on a mock NVML node the MIG capability tree is staged away
// from /proc, and preparing a MIG device must read it from ALT_PROC_DRIVER_PATH.
func TestGetDevNodesForMigDeviceReadsAltProcDriverPath(t *testing.T) {
	procDevices := filepath.Join(t.TempDir(), "proc-devices")
	require.NoError(t, os.WriteFile(procDevices,
		[]byte("Character devices:\n195 nvidia-frontend\n236 nvidia-caps\n\nBlock devices:\n  8 sd\n"), 0o644))
	common.ConfigureProcDevicesPath(procDevices)
	t.Cleanup(common.ResetProcDevicesPath)

	procDriver := filepath.Join(t.TempDir(), "proc", "driver")
	giDir := filepath.Join(procDriver, "nvidia/capabilities/gpu2/mig/gi5")
	require.NoError(t, os.MkdirAll(filepath.Join(giDir, "ci0"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(giDir, "access"),
		[]byte("DeviceFileMinor: 35\nDeviceFileMode: 292\nDeviceFileModify: 1\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(giDir, "ci0", "access"),
		[]byte("DeviceFileMinor: 36\nDeviceFileMode: 292\nDeviceFileModify: 1\n"), 0o644))
	common.ConfigureProcDriverPath(procDriver)
	t.Cleanup(common.ResetProcDriverPath)

	handler := &CDIHandler{}
	nodes, err := handler.GetDevNodesForMigDevice(&MigLiveTuple{ParentMinor: 2, GIID: 5, CIID: 0})
	require.NoError(t, err)
	require.Equal(t, []*cdispec.DeviceNode{
		{Path: "/dev/nvidia-caps/nvidia-cap35", Type: "c", FileMode: ptr.To(os.FileMode(292)), Major: 236, Minor: 35},
		{Path: "/dev/nvidia-caps/nvidia-cap36", Type: "c", FileMode: ptr.To(os.FileMode(292)), Major: 236, Minor: 36},
	}, nodes)
}

func TestGetCommonEditsCached(t *testing.T) {
	tests := map[string]struct {
		commonEditsErr     error
		cachedValue        any
		callCount          int
		wantUnderlyingCall int
		wantErr            string
	}{
		"underlying errors are not cached": {
			commonEditsErr:     errors.New("mock error"),
			callCount:          2,
			wantUnderlyingCall: 2,
			wantErr:            "mock error",
		},
		"invalid cached value returns an error": {
			cachedValue:        "invalid value",
			callCount:          1,
			wantUnderlyingCall: 0,
			wantErr:            "expected *cdiapi.ContainerEdits",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fakeNVCDIClaim := &fakeNVCDI{
				getCommonEdits: func() (*cdiapi.ContainerEdits, error) {
					return nil, tc.commonEditsErr
				},
			}
			handler := &CDIHandler{
				nvcdiClaim: fakeNVCDIClaim,
				specCache:  utilcache.NewExpiring(),
			}

			if tc.cachedValue != nil {
				handler.specCache.Set(
					"commonEdits",
					tc.cachedValue,
					5*time.Minute,
				)
			}

			for range tc.callCount {
				got, err := handler.GetCommonEditsCached()
				require.ErrorContains(t, err, tc.wantErr)
				assert.Nil(t, got)
			}

			assert.Equal(
				t,
				tc.wantUnderlyingCall,
				fakeNVCDIClaim.commonEditCalls,
			)
		})
	}
}

func TestGetDeviceSpecsByUUIDCachedBehavior(t *testing.T) {
	tests := map[string]struct {
		deviceSpecs        map[string][]cdispec.Device
		deviceSpecsErrs    map[string]error
		cachedValues       map[string]any
		requestUUIDs       []string
		wantUnderlyingCall map[string]int
		wantErr            string
		checkResults       func(*testing.T, [][]cdispec.Device)
	}{
		"entries are cached independently by UUID": {
			deviceSpecs: map[string][]cdispec.Device{
				"GPU-1": {
					{Name: "gpu-1", ContainerEdits: *containerEdits("/dev/nvidia1").ContainerEdits},
				},
				"GPU-2": {
					{Name: "gpu-2", ContainerEdits: *containerEdits("/dev/nvidia2").ContainerEdits},
				},
			},
			requestUUIDs: []string{
				"GPU-1",
				"GPU-2",
				"GPU-1",
				"GPU-2",
			},
			wantUnderlyingCall: map[string]int{
				"GPU-1": 1,
				"GPU-2": 1,
			},
			checkResults: func(t *testing.T, results [][]cdispec.Device) {
				require.Len(t, results, 4)
				assert.Equal(t, "gpu-1", results[0][0].Name)
				assert.Equal(t, "gpu-2", results[1][0].Name)
				assert.Equal(t, "gpu-1", results[2][0].Name)
				assert.Equal(t, "gpu-2", results[3][0].Name)
			},
		},
		"underlying errors are not cached": {
			deviceSpecs: map[string][]cdispec.Device{},
			deviceSpecsErrs: map[string]error{
				"GPU-123": errors.New("mock error"),
			},
			requestUUIDs: []string{"GPU-123", "GPU-123"},
			wantUnderlyingCall: map[string]int{
				"GPU-123": 2,
			},
			wantErr: "mock error",
		},
		"invalid cached value returns an error": {
			deviceSpecs: map[string][]cdispec.Device{},
			cachedValues: map[string]any{
				"GPU-123": "invalid value",
			},
			requestUUIDs:       []string{"GPU-123"},
			wantUnderlyingCall: map[string]int{},
			wantErr:            "expected []cdispec.Device",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			deviceSpecsCalls := map[string]int{}
			fakeNVCDIClaim := &fakeNVCDI{
				getDeviceSpecs: func(ids ...string) ([]cdispec.Device, error) {
					uuid := ids[0]
					deviceSpecsCalls[uuid]++
					return tc.deviceSpecs[uuid], tc.deviceSpecsErrs[uuid]
				},
			}
			handler := &CDIHandler{
				nvcdiClaim: fakeNVCDIClaim,
				specCache:  utilcache.NewExpiring(),
			}

			for uuid, value := range tc.cachedValues {
				handler.specCache.Set(uuid, value, 5*time.Minute)
			}

			var results [][]cdispec.Device
			for _, uuid := range tc.requestUUIDs {
				got, err := handler.GetDeviceSpecsByUUIDCached(uuid)

				if tc.wantErr != "" {
					require.Error(t, err)
					assert.Contains(t, err.Error(), tc.wantErr)
					continue
				}

				require.NoError(t, err)

				results = append(results, got)
			}

			assert.Equal(
				t,
				tc.wantUnderlyingCall,
				deviceSpecsCalls,
			)

			if tc.checkResults != nil {
				tc.checkResults(t, results)
			}
		})
	}
}

func TestWarmupDevSpecCache(t *testing.T) {
	deviceSpecsCalls := map[string]int{}
	specsByUUID := map[string][]cdispec.Device{
		"GPU-1": {{Name: "gpu-1", ContainerEdits: *containerEdits("/dev/nvidia1").ContainerEdits}},
		"GPU-2": {{Name: "gpu-2", ContainerEdits: *containerEdits("/dev/nvidia2").ContainerEdits}},
	}
	fakeNVCDIClaim := &fakeNVCDI{
		getDeviceSpecs: func(ids ...string) ([]cdispec.Device, error) {
			uuid := ids[0]
			deviceSpecsCalls[uuid]++
			if uuid == "GPU-error" {
				return nil, errors.New("mock error")
			}
			return specsByUUID[uuid], nil
		},
	}
	handler := &CDIHandler{
		nvcdiClaim: fakeNVCDIClaim,
		specCache:  utilcache.NewExpiring(),
	}

	handler.WarmupDevSpecCache([]string{
		"GPU-1",
		"GPU-error",
		"GPU-2",
		"GPU-1",
	})

	wantCalls := map[string]int{
		"GPU-1":     1,
		"GPU-2":     1,
		"GPU-error": 1,
	}
	require.Equal(t, wantCalls, deviceSpecsCalls)

	for uuid, wantName := range map[string]string{
		"GPU-1": "gpu-1",
		"GPU-2": "gpu-2",
	} {
		got, err := handler.GetDeviceSpecsByUUIDCached(uuid)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, wantName, got[0].Name)
	}

	assert.Equal(t, wantCalls, deviceSpecsCalls)
}

func TestCreateClaimSpecFile(t *testing.T) {
	const (
		claimUID = "claim-123"
		uuid     = "GPU-123"
	)

	tests := map[string]struct {
		commonEditsErr      error
		deviceSpecsErrs     map[string]error
		wantErr             string
		wantCommonEditCalls int
		wantDeviceSpecCalls map[string]int
	}{
		"regular GPU": {
			wantCommonEditCalls: 1,
			wantDeviceSpecCalls: map[string]int{uuid: 1},
		},
		"common edits error": {
			commonEditsErr:      errors.New("mock common edits error"),
			wantErr:             "failed to get common CDI spec edits: mock common edits error",
			wantCommonEditCalls: 1,
			wantDeviceSpecCalls: map[string]int{},
		},
		"device spec error": {
			deviceSpecsErrs: map[string]error{
				uuid: errors.New("mock device spec error"),
			},
			wantErr:             "unable to get device spec for claim-123-gpu-0: mock device spec error",
			wantCommonEditCalls: 1,
			wantDeviceSpecCalls: map[string]int{uuid: 1},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			deviceSpecsCalls := map[string]int{}
			fakeNVCDIClaim := &fakeNVCDI{
				getCommonEdits: func() (*cdiapi.ContainerEdits, error) {
					edits := containerEdits("/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools")
					edits.Env = []string{"COMMON=value"}
					return edits, tc.commonEditsErr
				},
				getDeviceSpecs: func(ids ...string) ([]cdispec.Device, error) {
					deviceSpecsCalls[ids[0]]++
					specs := deviceSpecs("/dev/nvidia0")
					specs[0].ContainerEdits.Env = []string{"DEVICE=value"}
					return specs, tc.deviceSpecsErrs[ids[0]]
				},
			}
			cdiRoot := t.TempDir()
			handler := &CDIHandler{
				nvcdiClaim: fakeNVCDIClaim,
				specCache:  utilcache.NewExpiring(),
				cdiRoot:    cdiRoot,
			}
			preparedDevices := PreparedDevices{
				{
					Devices: PreparedDeviceList{
						{
							Gpu: &PreparedGpu{
								Info: &GpuInfo{UUID: uuid, minor: 0},
								Device: &CheckpointedDevice{
									DeviceName: "gpu-0",
								},
							},
						},
					},
					ConfigState: DeviceConfigState{
						containerEdits: &cdiapi.ContainerEdits{
							ContainerEdits: &cdispec.ContainerEdits{
								Env: []string{"GROUP=value"},
							},
						},
					},
				},
			}
			specName := cdiapi.GenerateTransientSpecName(cdiVendor, cdiClaimClass, claimUID)
			specPath := filepath.Join(cdiRoot, specName+".yaml")

			err := handler.CreateClaimSpecFile(claimUID, preparedDevices, nil)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.NoFileExists(t, specPath)
			} else {
				require.NoError(t, err)

				generated, err := cdiapi.ReadSpec(specPath, 0)
				require.NoError(t, err)
				assert.Equal(t, cdiVendor+"/"+cdiClaimClass, generated.Kind)
				assert.Equal(t, []string{"COMMON=value"}, generated.ContainerEdits.Env)
				require.Len(t, generated.Devices, 1)
				assert.Equal(t, "claim-123-gpu-0", generated.Devices[0].Name)
				assert.Equal(t, []string{"DEVICE=value", "GROUP=value"}, generated.Devices[0].ContainerEdits.Env)
			}

			assert.Equal(t, tc.wantCommonEditCalls, fakeNVCDIClaim.commonEditCalls)
			assert.Equal(t, tc.wantDeviceSpecCalls, deviceSpecsCalls)
		})
	}
}
