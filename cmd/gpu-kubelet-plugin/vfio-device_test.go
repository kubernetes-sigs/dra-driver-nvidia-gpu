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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
)

type fakeVfioConfigureOperations struct {
	calls                   []string
	persistenceModeDisabled bool
	disableErr              error
	waitErr                 error
	verifyErr               error
	changeErrs              map[string]error
	enableErr               error
}

func (f *fakeVfioConfigureOperations) disableGPUPersistenceMode(string) (bool, error) {
	f.calls = append(f.calls, "disable-persistence")
	return f.persistenceModeDisabled, f.disableErr
}

func (f *fakeVfioConfigureOperations) enableGPUPersistenceMode(string) error {
	f.calls = append(f.calls, "enable-persistence")
	return f.enableErr
}

func (f *fakeVfioConfigureOperations) WaitForGPUFree(context.Context, *VfioDeviceInfo) error {
	f.calls = append(f.calls, "wait-for-free")
	return f.waitErr
}

func (f *fakeVfioConfigureOperations) verifyDisabledVFs(string) error {
	f.calls = append(f.calls, "verify-vfs")
	return f.verifyErr
}

func (f *fakeVfioConfigureOperations) changeDriver(_ string, driver string) error {
	f.calls = append(f.calls, "change-driver:"+driver)
	return f.changeErrs[driver]
}

func TestConfigureVfioDeviceRollsBackHostMutations(t *testing.T) {
	testCases := map[string]struct {
		ops           *fakeVfioConfigureOperations
		expectedCalls []string
		errorContains []string
	}{
		"wait failure restores persistence mode": {
			ops: &fakeVfioConfigureOperations{
				persistenceModeDisabled: true,
				waitErr:                 errors.New("GPU is busy"),
			},
			expectedCalls: []string{
				"disable-persistence",
				"wait-for-free",
				"change-driver:nvidia",
				"enable-persistence",
			},
			errorContains: []string{"GPU is busy"},
		},
		"driver change failure reports rollback failures": {
			ops: &fakeVfioConfigureOperations{
				persistenceModeDisabled: true,
				changeErrs: map[string]error{
					"vfio-pci": errors.New("bind failed"),
					"nvidia":   errors.New("driver restore failed"),
				},
				enableErr: errors.New("persistence restore failed"),
			},
			expectedCalls: []string{
				"disable-persistence",
				"wait-for-free",
				"verify-vfs",
				"change-driver:vfio-pci",
				"change-driver:nvidia",
				"enable-persistence",
			},
			errorContains: []string{"bind failed", "driver restore failed", "persistence restore failed"},
		},
		"success keeps the prepared state": {
			ops: &fakeVfioConfigureOperations{persistenceModeDisabled: true},
			expectedCalls: []string{
				"disable-persistence",
				"wait-for-free",
				"verify-vfs",
				"change-driver:vfio-pci",
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			err := configureVfioDevice(
				context.Background(),
				&VfioDeviceInfo{PciBusID: "0000:01:00.0"},
				"nvidia",
				"vfio-pci",
				tc.ops,
			)

			if len(tc.errorContains) == 0 {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				for _, message := range tc.errorContains {
					require.ErrorContains(t, err, message)
				}
			}
			require.Equal(t, tc.expectedCalls, tc.ops.calls)
		})
	}
}

type fakeNvPassthrough struct {
	devicePath string
	bindErrs   map[string]error
}

func (f *fakeNvPassthrough) FindBestVFIOVariant(string) (string, error) { return "vfio_pci", nil }
func (f *fakeNvPassthrough) BindToVFIODriver(string) error              { return nil }

func (f *fakeNvPassthrough) BindToDriver(_ string, driver string) error {
	if err := f.bindErrs[driver]; err != nil {
		return err
	}
	return os.Symlink(filepath.Join("/sys/bus/pci/drivers", driver), filepath.Join(f.devicePath, "driver"))
}

func (f *fakeNvPassthrough) Unbind(string) error {
	if err := os.Remove(filepath.Join(f.devicePath, "driver")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func TestChangeDriverRestoresOriginalDriverAfterBindFailure(t *testing.T) {
	const pciAddress = "0000:01:00.0"
	pciDevicesRoot := t.TempDir()
	devicePath := filepath.Join(pciDevicesRoot, pciAddress)
	require.NoError(t, os.MkdirAll(devicePath, 0o755))
	require.NoError(t, os.Symlink("/sys/bus/pci/drivers/nvidia", filepath.Join(devicePath, "driver")))

	nvpasst := &fakeNvPassthrough{
		devicePath: devicePath,
		bindErrs: map[string]error{
			"vfio-pci": errors.New("injected vfio bind failure"),
		},
	}
	manager := &VfioPciManager{
		pciDevicesPath: pciDevicesRoot,
		nvlib:          &deviceLib{nvpasst: nvpasst},
	}

	err := manager.changeDriver(pciAddress, "vfio-pci")
	require.ErrorContains(t, err, "injected vfio bind failure")

	driver, driverErr := getDriver(pciDevicesRoot, pciAddress)
	require.NoError(t, driverErr)
	require.Equal(t, "nvidia", driver)
}

type fakeVfioDeviceManager struct {
	configureCalls   []string
	unconfigureCalls []string
	configureErrs    map[string]error
	unconfigureErrs  map[string]error
}

func (f *fakeVfioDeviceManager) Configure(_ context.Context, info *VfioDeviceInfo) error {
	f.configureCalls = append(f.configureCalls, info.PciBusID)
	return f.configureErrs[info.PciBusID]
}

func (f *fakeVfioDeviceManager) Unconfigure(_ context.Context, info *VfioDeviceInfo) error {
	f.unconfigureCalls = append(f.unconfigureCalls, info.PciBusID)
	return f.unconfigureErrs[info.PciBusID]
}

func TestConfigureVfioDevicesRollsBackPreviouslyConfiguredDevices(t *testing.T) {
	manager := &fakeVfioDeviceManager{
		configureErrs: map[string]error{
			"0000:03:00.0": errors.New("injected third device failure"),
		},
		unconfigureErrs: map[string]error{},
	}
	state := &DeviceState{vfioPciManager: manager}

	devices := make([]*AllocatableDevice, 3)
	for index := range devices {
		devices[index] = &AllocatableDevice{Vfio: &VfioDeviceInfo{
			PciBusID: fmt.Sprintf("0000:0%d:00.0", index+1),
			index:    index,
		}}
	}

	err := state.configureVfioDevices(context.Background(), devices)
	require.ErrorContains(t, err, "injected third device failure")
	require.Equal(t, []string{"0000:01:00.0", "0000:02:00.0", "0000:03:00.0"}, manager.configureCalls)
	require.Equal(t, []string{"0000:02:00.0", "0000:01:00.0"}, manager.unconfigureCalls)
}

func TestRollbackVfioDevicesAttemptsEveryDevice(t *testing.T) {
	manager := &fakeVfioDeviceManager{
		configureErrs: map[string]error{},
		unconfigureErrs: map[string]error{
			"0000:02:00.0": errors.New("injected rollback failure"),
		},
	}
	state := &DeviceState{vfioPciManager: manager}
	devices := []*AllocatableDevice{
		{Vfio: &VfioDeviceInfo{PciBusID: "0000:01:00.0", index: 0}},
		{Vfio: &VfioDeviceInfo{PciBusID: "0000:02:00.0", index: 1}},
		{Vfio: &VfioDeviceInfo{PciBusID: "0000:03:00.0", index: 2}},
	}

	err := state.rollbackVfioDevices(context.Background(), devices, false)
	require.ErrorContains(t, err, "injected rollback failure")
	require.Equal(t, []string{"0000:03:00.0", "0000:02:00.0", "0000:01:00.0"}, manager.unconfigureCalls)
}

func TestRollbackCheckpointedVfioDevicesIgnoresMissingNonVfioAllocatables(t *testing.T) {
	state := &DeviceState{
		perGPUAllocatable: &PerGPUAllocatableDevices{allocatablesMap: map[PCIBusID]AllocatableDevices{}},
		vfioPciManager:    &fakeVfioDeviceManager{},
	}

	claimWithResult := func(deviceName string) PreparedClaim {
		return PreparedClaim{Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{Devices: resourceapi.DeviceAllocationResult{
				Results: []resourceapi.DeviceRequestAllocationResult{{
					Driver: DriverName,
					Device: deviceName,
				}},
			}},
		}}
	}

	require.NoError(t, state.rollbackCheckpointedVfioDevices(context.Background(), claimWithResult("gpu-0")))
	require.ErrorContains(t, state.rollbackCheckpointedVfioDevices(context.Background(), claimWithResult("gpu-vfio-0")), "allocatable not found for VFIO device")
}
