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
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/NVIDIA/go-nvlib/pkg/nvpci"
	"github.com/stretchr/testify/require"
)

func TestVfioDriverSwitchInflight(t *testing.T) {
	vm := NewVfioPciManager("", "", nil, true)
	const first = "0000:01:00.0"
	const second = "0000:02:00.0"

	require.False(t, vm.isDriverSwitchInflight(first))
	require.NoError(t, vm.markDriverSwitchInflight(first))
	require.True(t, vm.isDriverSwitchInflight(first))
	require.ErrorContains(t, vm.markDriverSwitchInflight(first), first)
	require.True(t, vm.isDriverSwitchInflight(first))
	require.False(t, vm.isDriverSwitchInflight(second))
	require.NoError(t, vm.markDriverSwitchInflight(second))
	require.True(t, vm.isDriverSwitchInflight(second))

	vm.unmarkDriverSwitchInflight("0000:03:00.0")
	require.True(t, vm.isDriverSwitchInflight(first))
	require.True(t, vm.isDriverSwitchInflight(second))

	vm.unmarkDriverSwitchInflight(first)
	require.False(t, vm.isDriverSwitchInflight(first))
	require.True(t, vm.isDriverSwitchInflight(second))
	require.NoError(t, vm.markDriverSwitchInflight(first))
	require.True(t, vm.isDriverSwitchInflight(first))
}

func TestVfioDriverSwitchInflightConcurrent(t *testing.T) {
	vm := NewVfioPciManager("", "", nil, true)
	const address = "0000:01:00.0"
	const workers = 16
	start := make(chan struct{})
	results := make(chan error, workers)
	for range workers {
		go func() {
			<-start
			results <- vm.markDriverSwitchInflight(address)
		}()
	}
	close(start)

	successes := 0
	for range workers {
		if err := <-results; err == nil {
			successes++
		} else {
			require.ErrorContains(t, err, address)
		}
	}
	require.Equal(t, 1, successes)
	require.True(t, vm.isDriverSwitchInflight(address))
	vm.unmarkDriverSwitchInflight(address)
	require.False(t, vm.isDriverSwitchInflight(address))
}

func TestVerifyDisabledVFs(t *testing.T) {
	lookupErr := errors.New("PCI lookup failed")
	tests := map[string]struct {
		gpu     *nvpci.NvidiaPCIDevice
		err     error
		wantErr string
	}{
		"PCI lookup error": {err: lookupErr},
		"missing GPU":      {wantErr: "no GPU found at PCI bus ID"},
		"no SR-IOV support": {
			gpu: &nvpci.NvidiaPCIDevice{},
		},
		"disabled VFs": {
			gpu: &nvpci.NvidiaPCIDevice{SriovInfo: nvpci.SriovInfo{
				PhysicalFunction: &nvpci.SriovPhysicalFunction{NumVFs: 0},
			}},
		},
		"enabled VFs": {
			gpu: &nvpci.NvidiaPCIDevice{SriovInfo: nvpci.SriovInfo{
				PhysicalFunction: &nvpci.SriovPhysicalFunction{NumVFs: 2},
			}},
			wantErr: "gpu has 2 VFs, cannot unbind",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			const address = "0000:01:00.0"
			pci := &nvpci.InterfaceMock{
				GetGPUByPciBusIDFunc: func(string) (*nvpci.NvidiaPCIDevice, error) {
					return tc.gpu, tc.err
				},
			}
			vm := NewVfioPciManager("", "", &deviceLib{nvpci: pci}, true)

			err := vm.verifyDisabledVFs(address)
			switch {
			case tc.err != nil:
				require.ErrorIs(t, err, tc.err)
			case tc.wantErr != "":
				require.ErrorContains(t, err, tc.wantErr)
			default:
				require.NoError(t, err)
			}
			calls := pci.GetGPUByPciBusIDCalls()
			require.Len(t, calls, 1)
			require.Equal(t, address, calls[0].S)
		})
	}
}

func TestCheckKernelModuleLoaded(t *testing.T) {
	tests := map[string]struct {
		setup      func(t *testing.T, path string)
		wantLoaded bool
		wantErr    bool
	}{
		"missing module": {},
		"regular file": {
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.WriteFile(path, nil, 0o644))
			},
		},
		"module directory": {
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.Mkdir(path, 0o755))
			},
			wantLoaded: true,
		},
		"stat error": {
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.Symlink(filepath.Base(path), path))
			},
			wantErr: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			const module = "vfio_pci"
			hostRoot := t.TempDir()
			path := filepath.Join(hostRoot, sysModulePath, module)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			if tc.setup != nil {
				tc.setup(t, path)
			}
			vm := NewVfioPciManager("", "", &deviceLib{hostRoot: hostRoot}, true)

			loaded, err := vm.checkKernelModuleLoaded(module)
			if tc.wantErr {
				require.ErrorIs(t, err, syscall.ELOOP)
				require.ErrorContains(t, err, "failed to check if module")
				var pathErr *os.PathError
				require.ErrorAs(t, err, &pathErr)
				require.Equal(t, path, pathErr.Path)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantLoaded, loaded)
		})
	}
}

func TestCheckIommuFDEnabled(t *testing.T) {
	tests := map[string]struct {
		setup       func(t *testing.T, path string)
		wantEnabled bool
		wantErr     bool
	}{
		"missing iommu device": {},
		"present iommu device": {
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.WriteFile(path, nil, 0o644))
			},
			wantEnabled: true,
		},
		"stat error": {
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.Symlink(filepath.Base(path), path))
			},
			wantErr: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			hostRoot := t.TempDir()
			path := filepath.Join(hostRoot, iommuDevicePath)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			if tc.setup != nil {
				tc.setup(t, path)
			}

			enabled, err := checkIommuFDEnabled(hostRoot)
			if tc.wantErr {
				require.ErrorIs(t, err, syscall.ELOOP)
				require.ErrorContains(t, err, "error checking if iommu device node exists")
				var pathErr *os.PathError
				require.ErrorAs(t, err, &pathErr)
				require.Equal(t, path, pathErr.Path)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantEnabled, enabled)
		})
	}
}

func TestUnconfigureNvidiaDisabled(t *testing.T) {
	vm := NewVfioPciManager("", "", nil, false)
	err := vm.Unconfigure(context.Background(), &VfioDeviceInfo{PciBusID: "0000:01:00.0"})
	require.NoError(t, err)
}

func TestUnconfigureRejectsInflightSwitch(t *testing.T) {
	vm := NewVfioPciManager("", "", nil, true)
	const address = "0000:01:00.0"
	require.NoError(t, vm.markDriverSwitchInflight(address))

	err := vm.Unconfigure(context.Background(), &VfioDeviceInfo{PciBusID: address})
	require.ErrorContains(t, err, "inflight")
	// A rejected switch must not clear the mark owned by the in-progress one.
	require.True(t, vm.isDriverSwitchInflight(address))
}

func TestWaitForGPUFreeWithoutParent(t *testing.T) {
	vm := NewVfioPciManager("", "", nil, true)
	err := vm.WaitForGPUFree(context.Background(), &VfioDeviceInfo{PciBusID: "0000:01:00.0"})
	require.NoError(t, err)
}

func TestDisableGPUPersistenceModeWithoutDaemon(t *testing.T) {
	for name, statError := range map[string]bool{"missing socket": false, "stat error": true} {
		t.Run(name, func(t *testing.T) {
			containerRoot := t.TempDir()
			path := filepath.Join(containerRoot, nvidiaPersistencedSocketPath)
			if statError {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.Symlink(filepath.Base(path), path))
			}
			vm := NewVfioPciManager(containerRoot, "", nil, true)

			err := vm.disableGPUPersistenceMode("0000:01:00.0")
			if statError {
				require.ErrorIs(t, err, syscall.ELOOP)
				require.ErrorContains(t, err, "error checking if nvidia-persistenced is running")
				var pathErr *os.PathError
				require.ErrorAs(t, err, &pathErr)
				require.Equal(t, path, pathErr.Path)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCheckIommuEnabled(t *testing.T) {
	tests := map[string]struct {
		setup       func(t *testing.T, path string)
		wantEnabled bool
		wantErr     bool
	}{
		"missing iommu groups": {},
		"empty iommu groups": {
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.MkdirAll(path, 0o755))
			},
		},
		"populated iommu groups": {
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.MkdirAll(filepath.Join(path, "0"), 0o755))
			},
			wantEnabled: true,
		},
		"unexpected read error": {
			setup: func(t *testing.T, path string) {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, nil, 0o644))
			},
			wantErr: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			hostRoot := t.TempDir()
			iommuGroupsPath := filepath.Join(hostRoot, kernelIommuGroupPath)
			if tc.setup != nil {
				tc.setup(t, iommuGroupsPath)
			}

			enabled, err := checkIommuEnabled(hostRoot)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantEnabled, enabled)
		})
	}
}

func TestGetDriver(t *testing.T) {
	t.Run("returns empty driver", func(t *testing.T) {
		pciDevicesPath := t.TempDir()
		pciAddress := "0000:00:01.0"
		devicePath := filepath.Join(pciDevicesPath, pciAddress)
		require.NoError(t, os.MkdirAll(devicePath, 0o755))

		driver, err := getDriver(pciDevicesPath, pciAddress)
		require.NoError(t, err)
		require.Empty(t, driver)
	})

	t.Run("returns valid driver", func(t *testing.T) {
		pciDevicesPath := t.TempDir()
		pciDriversPath := t.TempDir()
		pciAddress := "0000:00:01.0"
		devicePath := filepath.Join(pciDevicesPath, pciAddress)
		require.NoError(t, os.MkdirAll(devicePath, 0o755))

		require.NoError(t, os.Symlink(filepath.Join(pciDriversPath, "nvidia"), filepath.Join(devicePath, "driver")))
		driver, err := getDriver(pciDevicesPath, pciAddress)
		require.NoError(t, err)
		require.Equal(t, "nvidia", driver)
	})

	t.Run("returns readlink error", func(t *testing.T) {
		pciDevicesPath := t.TempDir()
		pciAddress := "0000:00:01.0"
		devicePath := filepath.Join(pciDevicesPath, pciAddress)
		require.NoError(t, os.MkdirAll(devicePath, 0o755))
		path := filepath.Join(devicePath, "driver")
		require.NoError(t, os.WriteFile(path, nil, 0o644))

		driver, err := getDriver(pciDevicesPath, pciAddress)
		require.Error(t, err)
		require.Empty(t, driver)
		var pathErr *os.PathError
		require.ErrorAs(t, err, &pathErr)
		require.Equal(t, path, pathErr.Path)
	})
}

func TestTryChangeDriverWithTimeout(t *testing.T) {
	vm := &VfioPciManager{
		nvidiaEnabled: true,
	}

	t.Run("returns success", func(t *testing.T) {
		err := vm.tryChangeDriverWithTimeout(context.Background(), time.Second, func() error {
			return nil
		})
		require.NoError(t, err)
	})

	t.Run("returns error", func(t *testing.T) {
		expected := errors.New("work function failed")
		err := vm.tryChangeDriverWithTimeout(context.Background(), time.Second, func() error {
			return expected
		})
		require.ErrorIs(t, err, expected)
	})

	t.Run("returns on timeout", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		finished := make(chan struct{})
		t.Cleanup(func() {
			close(release)
			requireEventuallyClosed(t, finished)
		})

		err := vm.tryChangeDriverWithTimeout(context.Background(), 10*time.Millisecond, func() error {
			close(started)
			defer close(finished)
			<-release
			return nil
		})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		requireClosed(t, started)
	})

	t.Run("returns on caller cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := make(chan struct{})
		release := make(chan struct{})
		finished := make(chan struct{})
		t.Cleanup(func() {
			close(release)
			requireEventuallyClosed(t, finished)
		})

		go func() {
			<-started
			cancel()
		}()

		err := vm.tryChangeDriverWithTimeout(ctx, time.Second, func() error {
			close(started)
			defer close(finished)
			<-release
			return nil
		})
		require.ErrorIs(t, err, context.Canceled)
	})
}

func requireClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	default:
		t.Fatal("expected channel to be closed")
	}
}

func requireEventuallyClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel to close")
	}
}
