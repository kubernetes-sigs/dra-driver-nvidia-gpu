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

	"github.com/NVIDIA/go-nvlib/pkg/nvpci"
	"github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi"
	nvcdspec "github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi/spec"
	"github.com/stretchr/testify/require"
	utilcache "k8s.io/apimachinery/pkg/util/cache"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

// newGPUDirectNVCDI fakes a GDRCopy or GDS library: nvcdi returns one "all"
// device, with empty edits when the node has no such device.
func newGPUDirectNVCDI(edits cdispec.ContainerEdits, err error) *fakeNVCDI {
	return &fakeNVCDI{
		getDeviceSpecs: func(...string) ([]cdispec.Device, error) {
			if err != nil {
				return nil, err
			}
			return []cdispec.Device{{Name: "all", ContainerEdits: edits}}, nil
		},
	}
}

var (
	gdrcopyEdits = cdispec.ContainerEdits{DeviceNodes: []*cdispec.DeviceNode{{Path: "/dev/gdrdrv"}}}
	gdsEdits     = cdispec.ContainerEdits{
		DeviceNodes: []*cdispec.DeviceNode{{Path: "/dev/nvidia-fs0"}},
		Mounts: []*cdispec.Mount{
			{HostPath: "/run/udev", ContainerPath: "/run/udev"},
			{HostPath: "/etc/cufile.json", ContainerPath: "/etc/cufile.json"},
		},
	}
)

func TestGPUDirectCDIHandlerGetCDIContainerEdits(t *testing.T) {
	testCases := map[string]struct {
		edits       cdispec.ContainerEdits
		err         error
		wantNodes   []*cdispec.DeviceNode
		wantMounts  []*cdispec.Mount
		wantErrText []string
	}{
		"device present": {
			edits:      gdsEdits,
			wantNodes:  gdsEdits.DeviceNodes,
			wantMounts: gdsEdits.Mounts,
		},
		"device absent": {},
		"discovery error": {
			err:         errors.New("permission denied"),
			wantErrText: []string{"gds", "permission denied"},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			fake := newGPUDirectNVCDI(tc.edits, tc.err)
			h := &gpuDirectCDIHandler{mode: nvcdi.ModeGds, lib: fake}

			edits, err := h.GetCDIContainerEdits()
			if tc.wantErrText != nil {
				for _, text := range tc.wantErrText {
					require.ErrorContains(t, err, text)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantNodes, edits.DeviceNodes)
			require.Equal(t, tc.wantMounts, edits.Mounts)
		})
	}
}

func TestGPUDirectCDIHandlerIsNotCached(t *testing.T) {
	fake := newGPUDirectNVCDI(gdrcopyEdits, nil)
	h := &gpuDirectCDIHandler{mode: nvcdi.ModeGdrcopy, lib: fake}

	for range 2 {
		_, err := h.GetCDIContainerEdits()
		require.NoError(t, err)
	}
	require.Equal(t, 2, fake.deviceSpecCalls)
}

func gpuDirectHandler(mode nvcdi.Mode, edits cdispec.ContainerEdits, err error) *gpuDirectCDIHandler {
	return &gpuDirectCDIHandler{mode: mode, lib: newGPUDirectNVCDI(edits, err)}
}

// newGPUDirectTestCDIHandler returns a CDIHandler for a node with one GPU
// (GPU-0, minor 0) and one VFIO-bound GPU in IOMMU group 7.
func newGPUDirectTestCDIHandler(t *testing.T, gdrcopy, gds *gpuDirectCDIHandler) *CDIHandler {
	return &CDIHandler{
		nvcdiClaim: &fakeNVCDI{
			getCommonEdits: func() (*cdiapi.ContainerEdits, error) {
				return containerEdits("/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools"), nil
			},
			getDeviceSpecs: func(...string) ([]cdispec.Device, error) {
				return deviceSpecs("/dev/nvidia0"), nil
			},
		},
		vfiocdi: &vfioCDIHandler{deviceLib: &deviceLib{nvpci: &nvpci.InterfaceMock{
			GetGPUByPciBusIDFunc: func(string) (*nvpci.NvidiaPCIDevice, error) {
				return &nvpci.NvidiaPCIDevice{IommuGroup: 7}, nil
			},
		}}},
		gdrcopycdi: gdrcopy,
		gdscdi:     gds,
		specCache:  utilcache.NewExpiring(),
		cdiRoot:    t.TempDir(),
	}
}

func gpuDirectTestGPUClaim() PreparedDevices {
	return PreparedDevices{{Devices: PreparedDeviceList{{Gpu: &PreparedGpu{
		Info:   &GpuInfo{UUID: "GPU-0", minor: 0},
		Device: &CheckpointedDevice{DeviceName: "gpu-0"},
	}}}}}
}

func gpuDirectTestVFIOClaim() PreparedDevices {
	vfioCommonEdits, _ := (&vfioCDIHandler{}).GetCommonEdits(false, false)
	return PreparedDevices{{
		Devices: PreparedDeviceList{{Vfio: &PreparedVfioDevice{
			Info:   &VfioDeviceInfo{PciBusID: "0000:01:00.0"},
			Device: &CheckpointedDevice{DeviceName: "vfio-0"},
		}}},
		ConfigState: DeviceConfigState{
			Config:         &configapi.VfioDeviceConfig{Iommu: &configapi.IOMMUConfig{}},
			containerEdits: vfioCommonEdits,
		},
	}}
}

func claimSpecPath(h *CDIHandler, claimUID string) string {
	return filepath.Join(h.cdiRoot, cdiapi.GenerateTransientSpecName(cdiVendor, cdiClaimClass, claimUID)+".yaml")
}

// On nodes without GDRCopy or GDS the spec must stay byte-identical to the
// one written before GPUDirect support: the common nvml edits and the GPU
// device, nothing else. The expected spec is built here the same way
// CreateClaimSpecFile builds it, so a stray edit (e.g. an empty "all" device
// or NVIDIA_VISIBLE_DEVICES=void) shows up as a byte difference.
func TestCreateClaimSpecFileWithoutGPUDirectDevicesIsUnchanged(t *testing.T) {
	h := newGPUDirectTestCDIHandler(t,
		gpuDirectHandler(nvcdi.ModeGdrcopy, cdispec.ContainerEdits{}, nil),
		gpuDirectHandler(nvcdi.ModeGds, cdispec.ContainerEdits{}, nil),
	)
	require.NoError(t, h.CreateClaimSpecFile("claim", gpuDirectTestGPUClaim(), nil))

	gpu := deviceSpecs("/dev/nvidia0")
	gpu[0].Name = "claim-gpu-0"
	expected, err := nvcdspec.New(
		nvcdspec.WithVendor(cdiVendor),
		nvcdspec.WithClass(cdiClaimClass),
		nvcdspec.WithDeviceSpecs(gpu),
		nvcdspec.WithEdits(*containerEdits("/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools").ContainerEdits),
	)
	require.NoError(t, err)
	require.NoError(t, h.writeSpec(expected, cdiapi.GenerateTransientSpecName(cdiVendor, cdiClaimClass, "expected")))

	got, err := os.ReadFile(claimSpecPath(h, "claim"))
	require.NoError(t, err)
	want, err := os.ReadFile(claimSpecPath(h, "expected"))
	require.NoError(t, err)
	require.Equal(t, string(want), string(got))
}

func TestCreateClaimSpecFileGPUDirectEdits(t *testing.T) {
	commonNodes := []string{"/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools"}
	testCases := map[string]struct {
		devices      PreparedDevices
		gdrcopyEdits cdispec.ContainerEdits
		gdsEdits     cdispec.ContainerEdits
		localIPC     bool
		wantNodes    []string
		wantMounts   []string
		wantCalls    int
	}{
		"GDRCopy present on GPU claim": {
			devices:      gpuDirectTestGPUClaim(),
			gdrcopyEdits: gdrcopyEdits,
			wantNodes:    append(commonNodes, "/dev/gdrdrv"),
			wantCalls:    1,
		},
		"GDS present on GPU claim": {
			devices:    gpuDirectTestGPUClaim(),
			gdsEdits:   gdsEdits,
			wantNodes:  append(commonNodes, "/dev/nvidia-fs0"),
			wantMounts: []string{"/run/udev", "/etc/cufile.json"},
			wantCalls:  1,
		},
		"present on VFIO-only claim": {
			devices:      gpuDirectTestVFIOClaim(),
			gdrcopyEdits: gdrcopyEdits,
			gdsEdits:     gdsEdits,
		},
		"present on GPU claim with local IPC edits": {
			devices:      gpuDirectTestGPUClaim(),
			gdrcopyEdits: gdrcopyEdits,
			gdsEdits:     gdsEdits,
			localIPC:     true,
			wantNodes:    append(commonNodes, "/dev/gdrdrv", "/dev/nvidia-fs0"),
			wantMounts:   []string{localIPCContainerPath, "/run/udev", "/etc/cufile.json"},
			wantCalls:    1,
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			gdrcopy := newGPUDirectNVCDI(tc.gdrcopyEdits, nil)
			gds := newGPUDirectNVCDI(tc.gdsEdits, nil)
			h := newGPUDirectTestCDIHandler(t,
				&gpuDirectCDIHandler{mode: nvcdi.ModeGdrcopy, lib: gdrcopy},
				&gpuDirectCDIHandler{mode: nvcdi.ModeGds, lib: gds},
			)
			var claimEdits *cdiapi.ContainerEdits
			if tc.localIPC {
				claimEdits = (&LocalIPCInfo{HostPath: t.TempDir(), ContainerPath: localIPCContainerPath}).GetCDIContainerEdits()
			}

			require.NoError(t, h.CreateClaimSpecFile("claim", tc.devices, claimEdits))

			spec, err := cdiapi.ReadSpec(claimSpecPath(h, "claim"), 0)
			require.NoError(t, err)
			var nodes, mounts []string
			for _, node := range spec.ContainerEdits.DeviceNodes {
				nodes = append(nodes, node.Path)
			}
			for _, mount := range spec.ContainerEdits.Mounts {
				mounts = append(mounts, mount.ContainerPath)
			}
			for _, dev := range spec.Devices {
				for _, node := range dev.ContainerEdits.DeviceNodes {
					require.NotContains(t, []string{"/dev/gdrdrv", "/dev/nvidia-fs0"}, node.Path, "GPUDirect edits belong at spec level")
				}
			}
			require.ElementsMatch(t, tc.wantNodes, nodes)
			require.ElementsMatch(t, tc.wantMounts, mounts)
			if tc.localIPC {
				require.Contains(t, spec.ContainerEdits.Env, "NVIDIA_DRA_LOCAL_IPC_DIR="+localIPCContainerPath)
			}
			require.Equal(t, tc.wantCalls, gdrcopy.deviceSpecCalls)
			require.Equal(t, tc.wantCalls, gds.deviceSpecCalls)
		})
	}
}

func TestCreateClaimSpecFileDoesNotGrowCachedCommonEdits(t *testing.T) {
	h := newGPUDirectTestCDIHandler(t,
		gpuDirectHandler(nvcdi.ModeGdrcopy, gdrcopyEdits, nil),
		gpuDirectHandler(nvcdi.ModeGds, gdsEdits, nil),
	)
	for _, claimUID := range []string{"claim-a", "claim-b"} {
		require.NoError(t, h.CreateClaimSpecFile(claimUID, gpuDirectTestGPUClaim(), nil))
	}

	common, err := h.GetCommonEditsCached()
	require.NoError(t, err)
	require.Len(t, common.DeviceNodes, 3)
	require.Empty(t, common.Mounts)
	spec, err := cdiapi.ReadSpec(claimSpecPath(h, "claim-b"), 0)
	require.NoError(t, err)
	require.Len(t, spec.ContainerEdits.DeviceNodes, 5)
}

func TestCreateClaimSpecFileFailsOnGPUDirectDiscoveryError(t *testing.T) {
	h := newGPUDirectTestCDIHandler(t,
		gpuDirectHandler(nvcdi.ModeGdrcopy, gdrcopyEdits, nil),
		gpuDirectHandler(nvcdi.ModeGds, cdispec.ContainerEdits{}, errors.New("permission denied")),
	)

	err := h.CreateClaimSpecFile("claim", gpuDirectTestGPUClaim(), nil)
	require.ErrorContains(t, err, "gds")
	require.ErrorContains(t, err, "permission denied")
	require.NoFileExists(t, claimSpecPath(h, "claim"))
}
