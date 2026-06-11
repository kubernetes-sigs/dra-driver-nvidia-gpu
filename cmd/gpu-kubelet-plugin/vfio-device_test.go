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
)

// setupMockIommuGroup creates a fake sysfs layout for IOMMU group testing.
// Returns the temp dir and a cleanup function.
//
// Layout:
//
//	<tmpdir>/sys/bus/pci/devices/<gpu>/iommu_group -> ../../../../kernel/iommu_groups/<groupID>
//	<tmpdir>/kernel/iommu_groups/<groupID>/devices/<device>/  (for each device)
//	<tmpdir>/sys/bus/pci/devices/<device>/class  (PCI class code)
func setupMockIommuGroup(t *testing.T, groupID string, devices map[string]string, gpuPciBusID string) string {
	t.Helper()

	tmpDir := t.TempDir()

	pciDevDir := filepath.Join(tmpDir, "sys", "bus", "pci", "devices")
	iommuGroupDevicesDir := filepath.Join(tmpDir, "kernel", "iommu_groups", groupID, "devices")
	require.NoError(t, os.MkdirAll(iommuGroupDevicesDir, 0755))

	for devAddr, classCode := range devices {
		// Create the device directory under iommu_groups/<N>/devices/
		devInGroup := filepath.Join(iommuGroupDevicesDir, devAddr)
		require.NoError(t, os.MkdirAll(devInGroup, 0755))

		// Create /sys/bus/pci/devices/<dev>/class with the PCI class code.
		devPciDir := filepath.Join(pciDevDir, devAddr)
		require.NoError(t, os.MkdirAll(devPciDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(devPciDir, "class"), []byte(classCode+"\n"), 0644))
	}

	// Create the iommu_group symlink for the GPU.
	gpuPciDir := filepath.Join(pciDevDir, gpuPciBusID)
	require.NoError(t, os.MkdirAll(gpuPciDir, 0755))

	// Symlink: <pciDevDir>/<gpu>/iommu_group -> relative path to iommu group
	relTarget, err := filepath.Rel(gpuPciDir, filepath.Join(tmpDir, "kernel", "iommu_groups", groupID))
	require.NoError(t, err)
	require.NoError(t, os.Symlink(relTarget, filepath.Join(gpuPciDir, "iommu_group")))

	return tmpDir
}

func TestGetIommuGroupCompanions(t *testing.T) {
	testCases := map[string]struct {
		gpuPciBusID        string
		devices            map[string]string // PCI address -> class code
		expectedCompanions []string
	}{
		"GPU with HDA audio companion": {
			gpuPciBusID: "0000:86:00.0",
			devices: map[string]string{
				"0000:86:00.0": "0x030000", // VGA controller
				"0000:86:00.1": "0x040300", // Audio device
			},
			expectedCompanions: []string{"0000:86:00.1"},
		},
		"GPU alone in group": {
			gpuPciBusID: "0000:41:00.0",
			devices: map[string]string{
				"0000:41:00.0": "0x030000",
			},
			expectedCompanions: nil,
		},
		"GPU with bridge device (should be skipped)": {
			gpuPciBusID: "0000:86:00.0",
			devices: map[string]string{
				"0000:86:00.0": "0x030000", // VGA controller
				"0000:85:00.0": "0x060400", // PCI bridge
			},
			expectedCompanions: nil,
		},
		"GPU with mixed devices": {
			gpuPciBusID: "0000:86:00.0",
			devices: map[string]string{
				"0000:86:00.0": "0x030000", // VGA controller
				"0000:86:00.1": "0x040300", // Audio device
				"0000:85:00.0": "0x060400", // PCI bridge (skipped)
				"0000:86:00.2": "0x0c0330", // USB controller
			},
			expectedCompanions: []string{"0000:86:00.1", "0000:86:00.2"},
		},
	}

	// Save and restore original pciDevicesPath.
	origPciDevicesPath := pciDevicesPath
	t.Cleanup(func() { pciDevicesPath = origPciDevicesPath })

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			tmpDir := setupMockIommuGroup(t, "6", tc.devices, tc.gpuPciBusID)
			pciDevicesPath = filepath.Join(tmpDir, "sys", "bus", "pci", "devices")

			companions, err := getIommuGroupCompanions(tc.gpuPciBusID)
			require.NoError(t, err)

			if tc.expectedCompanions == nil {
				require.Empty(t, companions)
			} else {
				require.ElementsMatch(t, tc.expectedCompanions, companions)
			}
		})
	}
}

func TestGetIommuGroupCompanions_NoIommuGroup(t *testing.T) {
	origPciDevicesPath := pciDevicesPath
	t.Cleanup(func() { pciDevicesPath = origPciDevicesPath })

	tmpDir := t.TempDir()
	pciDevicesPath = filepath.Join(tmpDir, "sys", "bus", "pci", "devices")

	// Create the GPU PCI directory without an iommu_group symlink.
	gpuDir := filepath.Join(pciDevicesPath, "0000:86:00.0")
	require.NoError(t, os.MkdirAll(gpuDir, 0755))

	_, err := getIommuGroupCompanions("0000:86:00.0")
	require.Error(t, err)
	require.Contains(t, err.Error(), "iommu_group")
}

func TestIsPCIBridgeDevice(t *testing.T) {
	origPciDevicesPath := pciDevicesPath
	t.Cleanup(func() { pciDevicesPath = origPciDevicesPath })

	tmpDir := t.TempDir()
	pciDevicesPath = filepath.Join(tmpDir, "sys", "bus", "pci", "devices")

	testCases := map[string]struct {
		classCode string
		isBridge  bool
	}{
		"VGA controller":  {"0x030000", false},
		"Audio device":    {"0x040300", false},
		"PCI bridge":      {"0x060400", true},
		"Root port":       {"0x060000", true},
		"USB controller":  {"0x0c0330", false},
		"NVMe controller": {"0x010802", false},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			devAddr := "0000:00:01.0"
			devDir := filepath.Join(pciDevicesPath, devAddr)
			require.NoError(t, os.MkdirAll(devDir, 0755))
			require.NoError(t, os.WriteFile(filepath.Join(devDir, "class"), []byte(tc.classCode+"\n"), 0644))

			result, err := isPCIBridgeDevice(devAddr)
			require.NoError(t, err)
			require.Equal(t, tc.isBridge, result)

			// Clean up for next iteration.
			require.NoError(t, os.RemoveAll(devDir))
		})
	}
}

func TestIsCompanionStillNeeded(t *testing.T) {
	vm := &VfioPciManager{
		companionDrivers: map[string]map[string]string{
			"0000:86:00.0": {
				"0000:86:00.1": "snd_hda_intel",
			},
			"0000:87:00.0": {
				"0000:86:00.1": "snd_hda_intel", // shared companion
				"0000:87:00.1": "snd_hda_intel",
			},
		},
	}

	// Companion 86:00.1 is referenced by both GPUs — still needed when excluding 86:00.0.
	require.True(t, vm.isCompanionStillNeeded("0000:86:00.1", "0000:86:00.0"))

	// Companion 87:00.1 is only referenced by 87:00.0 — not needed when excluding 87:00.0.
	require.False(t, vm.isCompanionStillNeeded("0000:87:00.1", "0000:87:00.0"))

	// Companion 86:00.1 still needed when excluding 87:00.0 (referenced by 86:00.0).
	require.True(t, vm.isCompanionStillNeeded("0000:86:00.1", "0000:87:00.0"))

	// Unknown companion is never needed.
	require.False(t, vm.isCompanionStillNeeded("0000:99:00.1", "0000:86:00.0"))
}

func TestRestoreIommuGroupCompanions_PerGPUCleanup(t *testing.T) {
	vm := &VfioPciManager{
		companionDrivers: map[string]map[string]string{
			"0000:86:00.0": {
				"0000:86:00.1": "snd_hda_intel",
			},
			"0000:87:00.0": {
				"0000:87:00.1": "snd_hda_intel",
			},
		},
	}

	// Restoring GPU 86:00.0 should only remove its entry, not affect 87:00.0.
	// Note: actual restore calls execCommand/WriteFile which will fail in tests,
	// but we test the map cleanup logic by checking that 87:00.0 companions survive.
	// Since restore uses klog.Warning for exec failures, this won't panic.
	vm.restoreIommuGroupCompanions("0000:86:00.0")

	require.NotContains(t, vm.companionDrivers, "0000:86:00.0")
	require.Contains(t, vm.companionDrivers, "0000:87:00.0")
	require.Equal(t, "snd_hda_intel", vm.companionDrivers["0000:87:00.0"]["0000:87:00.1"])
}
