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

// fakePciDevicesRoot builds a stand-in for /sys/bus/pci/devices in which
// pciAddress is bound to driver. An empty driver creates the device directory
// without a driver symlink, which is what sysfs looks like for a device no
// driver has claimed.
func fakePciDevicesRoot(t *testing.T, pciAddress, driver string) string {
	t.Helper()

	root := t.TempDir()
	devicePath := filepath.Join(root, pciAddress)
	require.NoError(t, os.MkdirAll(devicePath, 0o755))

	if driver != "" {
		// Mirrors sysfs, where the driver entry is a relative symlink into
		// /sys/bus/pci/drivers. getDriver() only reads the link, so the target
		// does not have to exist.
		target := filepath.Join("..", "..", "drivers", driver)
		require.NoError(t, os.Symlink(target, filepath.Join(devicePath, "driver")))
	}

	return root
}

func TestDeviceBoundToVfio(t *testing.T) {
	const pciAddress = "0000:41:00.0"

	tests := map[string]struct {
		// driver is the driver the fake sysfs binds pciAddress to ("" for none).
		driver string
		// queryAddress is the address deviceBoundToVfio is asked about.
		queryAddress string
		expected     bool
	}{
		"bound to vfio-pci": {
			driver:       "vfio-pci",
			queryAddress: pciAddress,
			expected:     true,
		},
		"bound to the Grace-Hopper vfio variant driver": {
			driver:       "nvgrace_gpu_vfio_pci",
			queryAddress: pciAddress,
			expected:     true,
		},
		"bound to nvidia": {
			driver:       nvidiaDriver,
			queryAddress: pciAddress,
			expected:     false,
		},
		"no driver bound": {
			driver:       "",
			queryAddress: pciAddress,
			expected:     false,
		},
		"device not present": {
			driver:       "vfio-pci",
			queryAddress: "0000:99:00.0",
			expected:     false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			root := fakePciDevicesRoot(t, pciAddress, tc.driver)
			require.Equal(t, tc.expected, deviceBoundToVfio(root, tc.queryAddress))
		})
	}
}

func nvidiaCommonEdits() *cdiapi.ContainerEdits {
	return containerEdits("/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools")
}

func newVfioTestCDIHandler(t *testing.T, commonEdits *cdiapi.ContainerEdits, commonErr error, specs []cdispec.Device, specsErr error, pciDevicesRoot string) *CDIHandler {
	t.Helper()

	return &CDIHandler{
		nvcdiClaim: &fakeNVCDI{
			getCommonEdits: func() (*cdiapi.ContainerEdits, error) {
				return commonEdits, commonErr
			},
			getDeviceSpecs: func(...string) ([]cdispec.Device, error) {
				return specs, specsErr
			},
		},
		specCache:      utilcache.NewExpiring(),
		cdiRoot:        t.TempDir(),
		pciDevicesRoot: pciDevicesRoot,
	}
}

func gpuPreparedDevices(uuid, deviceName, pciBusID string) PreparedDevices {
	return PreparedDevices{
		{
			Devices: PreparedDeviceList{
				{
					Gpu: &PreparedGpu{
						Info:   &GpuInfo{UUID: uuid, minor: 0, pciBusID: pciBusID},
						Device: &CheckpointedDevice{DeviceName: deviceName},
					},
				},
			},
		},
	}
}

func readClaimSpec(t *testing.T, cdi *CDIHandler, claimUID string) *cdispec.Spec {
	t.Helper()

	specName := cdiapi.GenerateTransientSpecName(cdiVendor, cdiClaimClass, claimUID)
	raw, err := cdiapi.ReadSpec(filepath.Join(cdi.cdiRoot, specName+".yaml"), 0)
	require.NoError(t, err)
	return raw.Spec
}

// A GPU that a passthrough consumer already rebound to a vfio variant driver is
// invisible to NVML, so spec generation fails for it. Prepare must tolerate
// that instead of wedging the claim; see the comment in CreateClaimSpecFile().
func TestCreateClaimSpecFileToleratesVfioBoundGPU(t *testing.T) {
	const (
		claimUID   = "6b9b1e7a-1b3c-4d1e-9f0a-6d2b6a0f1c11"
		pciAddress = "0000:41:00.0"
		deviceName = "gpu-0"
	)

	for _, driver := range []string{"vfio-pci", "nvgrace_gpu_vfio_pci"} {
		t.Run(driver, func(t *testing.T) {
			cdi := newVfioTestCDIHandler(t,
				nvidiaCommonEdits(), nil,
				nil, errors.New("device handle from UUID: Not Found"),
				fakePciDevicesRoot(t, pciAddress, driver))

			err := cdi.CreateClaimSpecFile(claimUID, gpuPreparedDevices("GPU-fake", deviceName, pciAddress), nil)
			require.NoError(t, err)

			// The claim must still get a spec, and the device in it must carry
			// container edits: CDI rejects a device with empty edits.
			s := readClaimSpec(t, cdi, claimUID)
			require.Len(t, s.Devices, 1)
			require.Equal(t, claimUID+"-"+deviceName, s.Devices[0].Name)
			require.Equal(t, []string{"NVIDIA_DRA_GPU_VFIO_PASSTHROUGH=" + pciAddress}, s.Devices[0].ContainerEdits.Env)
			// No /dev/nvidia* nodes: the consumer attaches the GPU by PCI address.
			require.Empty(t, s.Devices[0].ContainerEdits.DeviceNodes)
		})
	}
}

// Common NVIDIA edits are validated before per-device specs. When the GPU is
// already vfio-bound those edits are not required, so a failure there must not
// wedge prepare either.
func TestCreateClaimSpecFileToleratesMissingCommonEditsForVfioBoundGPU(t *testing.T) {
	const (
		claimUID   = "6b9b1e7a-1b3c-4d1e-9f0a-6d2b6a0f1c11"
		pciAddress = "0000:41:00.0"
		deviceName = "gpu-0"
	)

	cdi := newVfioTestCDIHandler(t,
		containerEdits(), nil,
		nil, errors.New("device handle from UUID: Not Found"),
		fakePciDevicesRoot(t, pciAddress, "vfio-pci"))

	err := cdi.CreateClaimSpecFile(claimUID, gpuPreparedDevices("GPU-fake", deviceName, pciAddress), nil)
	require.NoError(t, err)

	s := readClaimSpec(t, cdi, claimUID)
	require.Len(t, s.Devices, 1)
	require.Equal(t, []string{"NVIDIA_DRA_GPU_VFIO_PASSTHROUGH=" + pciAddress}, s.Devices[0].ContainerEdits.Env)
	require.Empty(t, s.ContainerEdits.DeviceNodes)
}

// The toleration is narrow: a spec generation failure for a GPU that is not
// vfio-bound is a genuine error and must still be surfaced.
func TestCreateClaimSpecFileSurfacesErrorForNonVfioGPU(t *testing.T) {
	const (
		claimUID   = "6b9b1e7a-1b3c-4d1e-9f0a-6d2b6a0f1c11"
		pciAddress = "0000:41:00.0"
		deviceName = "gpu-0"
	)

	tests := map[string]struct {
		// driver is the driver the GPU is bound to in the fake sysfs.
		driver string
		// pciBusID is what the prepared device reports; empty means the bus ID
		// was never discovered, so no binding check is possible.
		pciBusID string
	}{
		"bound to nvidia":      {driver: nvidiaDriver, pciBusID: pciAddress},
		"no driver bound":      {driver: "", pciBusID: pciAddress},
		"pci bus ID not known": {driver: "vfio-pci", pciBusID: ""},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cdi := newVfioTestCDIHandler(t,
				nvidiaCommonEdits(), nil,
				nil, errors.New("device handle from UUID: Not Found"),
				fakePciDevicesRoot(t, pciAddress, tc.driver))

			err := cdi.CreateClaimSpecFile(claimUID, gpuPreparedDevices("GPU-fake", deviceName, tc.pciBusID), nil)

			require.ErrorContains(t, err, "unable to get device spec for "+claimUID+"-"+deviceName)
			require.ErrorContains(t, err, "device handle from UUID: Not Found")
		})
	}
}

func TestCreateClaimSpecFileSurfacesCommonEditsErrorForNonVfioGPU(t *testing.T) {
	const (
		claimUID   = "6b9b1e7a-1b3c-4d1e-9f0a-6d2b6a0f1c11"
		pciAddress = "0000:41:00.0"
	)

	cdi := newVfioTestCDIHandler(t,
		containerEdits(), nil,
		nil, nil,
		fakePciDevicesRoot(t, pciAddress, nvidiaDriver))

	err := cdi.CreateClaimSpecFile(claimUID, gpuPreparedDevices("GPU-fake", "gpu-0", pciAddress), nil)
	require.ErrorContains(t, err, "failed to get common CDI spec edits")
	require.ErrorContains(t, err, "failed to validate NVIDIA CDI common edits")
}

// A GPU NVML can see must be unaffected by the vfio toleration.
func TestCreateClaimSpecFileUsesNvcdiSpecForNvmlVisibleGPU(t *testing.T) {
	const (
		claimUID   = "6b9b1e7a-1b3c-4d1e-9f0a-6d2b6a0f1c11"
		pciAddress = "0000:41:00.0"
		deviceName = "gpu-0"
	)

	cdi := newVfioTestCDIHandler(t,
		nvidiaCommonEdits(), nil,
		deviceSpecs("/dev/nvidia0"), nil,
		fakePciDevicesRoot(t, pciAddress, nvidiaDriver))

	err := cdi.CreateClaimSpecFile(claimUID, gpuPreparedDevices("GPU-fake", deviceName, pciAddress), nil)
	require.NoError(t, err)

	s := readClaimSpec(t, cdi, claimUID)
	require.Len(t, s.Devices, 1)
	require.Equal(t, claimUID+"-"+deviceName, s.Devices[0].Name)
	require.Len(t, s.Devices[0].ContainerEdits.DeviceNodes, 1)
	require.Equal(t, "/dev/nvidia0", s.Devices[0].ContainerEdits.DeviceNodes[0].Path)
	require.Empty(t, s.Devices[0].ContainerEdits.Env)
}
