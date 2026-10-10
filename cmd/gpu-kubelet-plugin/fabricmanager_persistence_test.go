/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/fabricmanager"
)

type fakePersistenceSMI struct {
	lib     *deviceLib
	logPath string
}

// Substitute chroot so tests exercise the real persistence command builder
// without requiring a driver installation, root privileges, or GPU hardware.
func newFakePersistenceSMI(t *testing.T, exitCode int) fakePersistenceSMI {
	t.Helper()
	dir := t.TempDir()
	fake := fakePersistenceSMI{lib: &deviceLib{devRoot: filepath.Join(dir, "driver")}, logPath: filepath.Join(dir, "commands")}
	require.NoError(t, os.MkdirAll(fake.lib.devRoot, 0o755))
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nexit %d\n", fake.logPath, exitCode)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chroot"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return fake
}

func (s fakePersistenceSMI) commands(t *testing.T) []string {
	t.Helper()
	content, err := os.ReadFile(s.logPath)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	return strings.Split(strings.TrimSpace(string(content)), "\n")
}

type persistenceFMClient struct {
	*testFMClient
	beforeActivate func()
}

func (c *persistenceFMClient) ActivateFabricPartition(id int) error {
	c.beforeActivate()
	return c.testFMClient.ActivateFabricPartition(id)
}

func persistenceClaimState(t *testing.T, client fabricmanager.Client, lib *deviceLib, inventory map[int]*GpuInfo, modules []int, vfio bool) (*DeviceState, *resourceapi.ResourceClaim) {
	t.Helper()
	manager, err := fabricmanager.Open(client)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Close()) })
	allocatables := make(map[PCIBusID]AllocatableDevices)
	// Keep all GPUs in inventory, including those outside the requested claim.
	for module, gpu := range inventory {
		name := fmt.Sprintf("device-%d", module)
		device := &AllocatableDevice{Gpu: gpu}
		if vfio {
			device = &AllocatableDevice{Vfio: &VfioDeviceInfo{UUID: "vfio-" + name, PciBusID: gpu.pciBusID, parent: gpu}}
		}
		allocatables[PCIBusID(gpu.pciBusID)] = AllocatableDevices{name: device}
	}
	var results []resourceapi.DeviceRequestAllocationResult
	for _, module := range modules {
		results = append(results, resourceapi.DeviceRequestAllocationResult{Driver: DriverName, Device: fmt.Sprintf("device-%d", module)})
	}
	claim := &resourceapi.ResourceClaim{Status: resourceapi.ResourceClaimStatus{Allocation: &resourceapi.AllocationResult{Devices: resourceapi.DeviceAllocationResult{Results: results}}}}
	return &DeviceState{fmManager: manager, nvdevlib: lib, perGPUAllocatable: &PerGPUAllocatableDevices{allocatablesMap: allocatables}}, claim
}

func TestFabricPartitionPersistenceOrder(t *testing.T) {
	for _, vfio := range []bool{false, true} {
		for _, modules := range [][]int{{1}, {1, 2}, {1, 2, 3, 4}, {1, 2, 3, 4, 5, 6, 7, 8}} {
			for _, daemon := range []bool{false, true} {
				t.Run(fmt.Sprintf("vfio=%t/GPUs=%d/daemon=%t", vfio, len(modules), daemon), func(t *testing.T) {
					smi := newFakePersistenceSMI(t, 0)
					if daemon {
						socket := filepath.Join(smi.lib.devRoot, nvidiaPersistencedSocketPath)
						require.NoError(t, os.MkdirAll(filepath.Dir(socket), 0o755))
						require.NoError(t, os.WriteFile(socket, nil, 0o600))
					}
					partitions, inventory := a100FabricIdentityFixture()
					var want []string
					for _, module := range modules {
						want = append(want, smi.lib.devRoot+" nvidia-smi -i "+inventory[module].pciBusID+" -pm 0")
					}
					client := &persistenceFMClient{testFMClient: &testFMClient{partitions: partitions}, beforeActivate: func() {
						require.Equal(t, want, smi.commands(t), "all selected GPUs must have persistence disabled before FM activation")
					}}
					state, claim := persistenceClaimState(t, client, smi.lib, inventory, modules, vfio)
					require.NoError(t, state.activateFabricPartition(claim))
					require.Len(t, client.activatedIDs, 1)
					require.NoError(t, state.activateFabricPartition(claim))
					require.Len(t, client.activatedIDs, 1)
					require.Equal(t, want, smi.commands(t), "active retry must not change persistence again")

					// A stop/start must perform preparation again, even if cleanup
					// or another process has re-enabled persistence in between.
					require.NoError(t, client.DeactivateFabricPartition(client.activatedIDs[0]))
					client.beforeActivate = func() { require.Equal(t, append(want, want...), smi.commands(t)) }
					require.NoError(t, state.activateFabricPartition(claim))
					require.Len(t, client.activatedIDs, 2)
				})
			}
		}
	}
}

func TestFabricPartitionPersistenceFailure(t *testing.T) {
	smi := newFakePersistenceSMI(t, 1)
	partitions, inventory := a100FabricIdentityFixture()
	client := &testFMClient{partitions: partitions}
	state, claim := persistenceClaimState(t, client, smi.lib, inventory, []int{1, 2}, false)
	err := state.activateFabricPartition(claim)
	require.ErrorContains(t, err, "disabling persistence for GPU "+inventory[1].UUID)
	require.ErrorContains(t, err, "0000:15:00.0")
	require.Empty(t, client.activatedIDs)
	require.Equal(t, []string{smi.lib.devRoot + " nvidia-smi -i 0000:15:00.0 -pm 0"}, smi.commands(t))
}

func TestFabricPartitionPersistenceScope(t *testing.T) {
	for _, tc := range []struct {
		name       string
		modules    []int
		activeID   int
		admin      bool
		wantError  string
		wantChange bool
	}{
		{name: "other VM active", modules: []int{1}, activeID: 33, wantChange: true},
		{name: "already active after restart", modules: []int{1}, activeID: 31},
		{name: "overlapping active partition", modules: []int{1}, activeID: 10, wantError: "overlaps active partition 10"},
		{name: "invalid GPU combination", modules: []int{1, 3}, wantError: "does not match any FM partition"},
		{name: "admin access", modules: []int{1}, admin: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			smi := newFakePersistenceSMI(t, 0)
			partitions, inventory := a100FabricIdentityFixture()
			for i := range partitions {
				partitions[i].IsActive = partitions[i].ID == tc.activeID
			}
			client := &testFMClient{partitions: partitions}
			state, claim := persistenceClaimState(t, client, smi.lib, inventory, tc.modules, true)
			if tc.admin {
				claim.Status.Allocation.Devices.Results[0].AdminAccess = ptr.To(true)
			}
			err := state.activateFabricPartition(claim)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
			} else {
				require.NoError(t, err)
			}
			if tc.wantChange {
				require.Equal(t, []string{smi.lib.devRoot + " nvidia-smi -i 0000:15:00.0 -pm 0"}, smi.commands(t))
				require.Equal(t, []int{31}, client.activatedIDs)
			} else {
				require.Empty(t, smi.commands(t))
				require.Empty(t, client.activatedIDs)
			}
		})
	}
}
