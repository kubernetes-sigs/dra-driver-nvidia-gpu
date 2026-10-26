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
	"testing"

	"github.com/stretchr/testify/require"
)

func setFabricTestArchitecture(inventory map[int]*GpuInfo, architecture string) {
	for _, gpu := range inventory {
		gpu.architecture = architecture
	}
}

func persistenceCommands(smi fakePersistenceSMI, inventory map[int]*GpuInfo, modules []int) []string {
	var cmds []string
	for _, module := range modules {
		cmds = append(cmds, smi.lib.devRoot+" nvidia-smi -i "+inventory[module].pciBusID+" -pm 0")
	}
	return cmds
}

func createPCIResetFiles(t *testing.T, smi fakePersistenceSMI, inventory map[int]*GpuInfo, modules []int) {
	t.Helper()
	for _, module := range modules {
		devicePath := filepath.Join(smi.lib.sysfsRoot, "bus", "pci", "devices", inventory[module].pciBusID)
		require.NoError(t, os.MkdirAll(devicePath, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(devicePath, "reset_method"), []byte("flr bus\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(devicePath, "reset"), nil, 0o644))
	}
}

func requirePCIResetValue(t *testing.T, smi fakePersistenceSMI, gpu *GpuInfo, want string) {
	t.Helper()
	devicePath := filepath.Join(smi.lib.sysfsRoot, "bus", "pci", "devices", gpu.pciBusID)
	reset, err := os.ReadFile(filepath.Join(devicePath, "reset"))
	require.NoError(t, err)
	require.Equal(t, want, string(reset))
	resetMethods, err := os.ReadFile(filepath.Join(devicePath, "reset_method"))
	require.NoError(t, err)
	require.Equal(t, "flr bus\n", string(resetMethods), "reset method ordering must be restored after SBR")
}

func markPCIResetFiles(t *testing.T, smi fakePersistenceSMI, inventory map[int]*GpuInfo, modules []int) {
	t.Helper()
	for _, module := range modules {
		resetPath := filepath.Join(smi.lib.sysfsRoot, "bus", "pci", "devices", inventory[module].pciBusID, "reset")
		require.NoError(t, os.WriteFile(resetPath, []byte("not-reset\n"), 0o644))
	}
}

func TestFabricPartitionResetBeforeActivation(t *testing.T) {
	for _, tc := range []struct {
		architecture string
		wantReset    bool
	}{
		{architecture: "Volta", wantReset: true},
		{architecture: "Ampere", wantReset: true},
		{architecture: "Hopper"},
		{architecture: "Blackwell"},
		{architecture: ""},
	} {
		for _, modules := range [][]int{{1}, {1, 2}} {
			for _, vfio := range []bool{false, true} {
				t.Run(fmt.Sprintf("arch=%q/GPUs=%d/vfio=%t", tc.architecture, len(modules), vfio), func(t *testing.T) {
					smi := newFakePersistenceSMI(t, 0)
					partitions, inventory := a100FabricIdentityFixture()
					setFabricTestArchitecture(inventory, tc.architecture)
					createPCIResetFiles(t, smi, inventory, modules)
					partitionID := map[int]int{1: 31, 2: 17}[len(modules)]
					client := &persistenceFMClient{testFMClient: &testFMClient{partitions: partitions}}
					state, claim := persistenceClaimState(t, client, smi.lib, inventory, modules, vfio)

					if tc.wantReset && vfio && len(modules) > 1 {
						client.beforeActivate = func() { t.Fatal("rejected VFIO handoff must not activate the partition") }
						err := state.activateFabricPartition(claim)
						require.ErrorContains(t, err, fmt.Sprintf("VFIO passthrough of 2-GPU fabric partition %d is not supported on %s GPUs", partitionID, tc.architecture))
						require.Empty(t, smi.commands(t), "rejection must happen before persistence or reset changes")
						return
					}

					want := persistenceCommands(smi, inventory, modules)
					// A single-GPU partition has no NVLinks to train, and
					// vfio-pci resets the GPU after activation regardless.
					wantReset := tc.wantReset && (!vfio || len(modules) > 1)
					client.beforeActivate = func() {
						require.Equal(t, want, smi.commands(t), "GPUs must be reset after disabling persistence and before FM activation")
						for _, module := range modules {
							wantValue := ""
							if wantReset {
								wantValue = "1\n"
							}
							requirePCIResetValue(t, smi, inventory[module], wantValue)
						}
					}
					require.NoError(t, state.activateFabricPartition(claim))
					require.Equal(t, []int{partitionID}, client.activatedIDs)

					markPCIResetFiles(t, smi, inventory, modules)
					require.NoError(t, state.activateFabricPartition(claim))
					require.Equal(t, []int{partitionID}, client.activatedIDs)
					require.Equal(t, want, smi.commands(t), "retry of an active partition must not reset its GPUs")
					for _, module := range modules {
						requirePCIResetValue(t, smi, inventory[module], "not-reset\n")
					}
				})
			}
		}
	}
}

// A GPU moved into a different partition must be reset again, because its
// NVLinks remain trained for the previous partition.
func TestFabricPartitionResetOnPartitionChange(t *testing.T) {
	smi := newFakePersistenceSMI(t, 0)
	partitions, inventory := a100FabricIdentityFixture()
	setFabricTestArchitecture(inventory, "Ampere")
	createPCIResetFiles(t, smi, inventory, []int{1, 2})
	client := &persistenceFMClient{testFMClient: &testFMClient{partitions: partitions}, beforeActivate: func() {}}

	single, singleClaim := persistenceClaimState(t, client, smi.lib, inventory, []int{1}, false)
	require.NoError(t, single.activateFabricPartition(singleClaim))
	require.Equal(t, []int{31}, client.activatedIDs)
	require.NoError(t, client.DeactivateFabricPartition(31))

	markPCIResetFiles(t, smi, inventory, []int{1, 2})
	want := persistenceCommands(smi, inventory, []int{1})
	want = append(want, persistenceCommands(smi, inventory, []int{1, 2})...)
	client.beforeActivate = func() {
		require.Equal(t, want, smi.commands(t), "every GPU of the new partition must be reset before activation")
		for _, module := range []int{1, 2} {
			requirePCIResetValue(t, smi, inventory[module], "1\n")
		}
	}
	pair, pairClaim := persistenceClaimState(t, client, smi.lib, inventory, []int{1, 2}, false)
	require.NoError(t, pair.activateFabricPartition(pairClaim))
	require.Equal(t, []int{31, 17}, client.activatedIDs)
}

func TestFabricPartitionResetFailure(t *testing.T) {
	smi := newFakePersistenceSMI(t, 0)
	partitions, inventory := a100FabricIdentityFixture()
	setFabricTestArchitecture(inventory, "Ampere")
	createPCIResetFiles(t, smi, inventory, []int{1, 2})
	resetPath := filepath.Join(smi.lib.sysfsRoot, "bus", "pci", "devices", inventory[1].pciBusID, "reset")
	require.NoError(t, os.Remove(resetPath))
	require.NoError(t, os.Mkdir(resetPath, 0o755))
	client := &testFMClient{partitions: partitions}
	state, claim := persistenceClaimState(t, client, smi.lib, inventory, []int{1, 2}, false)

	err := state.activateFabricPartition(claim)
	require.ErrorContains(t, err, fmt.Sprintf("resetting GPUs [%s %s]", inventory[1].pciBusID, inventory[2].pciBusID))
	require.ErrorContains(t, err, "triggering reset through")
	require.Empty(t, client.activatedIDs)
	want := persistenceCommands(smi, inventory, []int{1, 2})
	require.Equal(t, want, smi.commands(t))
	resetMethods, readErr := os.ReadFile(filepath.Join(filepath.Dir(resetPath), "reset_method"))
	require.NoError(t, readErr)
	require.Equal(t, "flr bus\n", string(resetMethods), "reset methods must also be restored after reset failure")
}
