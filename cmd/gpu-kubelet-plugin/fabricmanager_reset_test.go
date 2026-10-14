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

func resetCommands(smi fakePersistenceSMI, inventory map[int]*GpuInfo, modules []int) []string {
	var cmds []string
	for _, module := range modules {
		cmds = append(cmds, smi.lib.devRoot+" nvidia-smi -i "+inventory[module].pciBusID+" -r")
	}
	return cmds
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
		for _, vfio := range []bool{false, true} {
			t.Run(fmt.Sprintf("arch=%q/vfio=%t", tc.architecture, vfio), func(t *testing.T) {
				smi := newFakePersistenceSMI(t, 0)
				partitions, inventory := a100FabricIdentityFixture()
				setFabricTestArchitecture(inventory, tc.architecture)
				modules := []int{1, 2}
				want := persistenceCommands(smi, inventory, modules)
				if tc.wantReset {
					want = append(want, resetCommands(smi, inventory, modules)...)
				}
				client := &persistenceFMClient{testFMClient: &testFMClient{partitions: partitions}, beforeActivate: func() {
					require.Equal(t, want, smi.commands(t), "GPUs must be reset after disabling persistence and before FM activation")
				}}
				state, claim := persistenceClaimState(t, client, smi.lib, inventory, modules, vfio)
				require.NoError(t, state.activateFabricPartition(claim))
				require.Equal(t, []int{17}, client.activatedIDs)

				require.NoError(t, state.activateFabricPartition(claim))
				require.Equal(t, []int{17}, client.activatedIDs)
				require.Equal(t, want, smi.commands(t), "retry of an active partition must not reset its GPUs")
			})
		}
	}
}

// A GPU moved into a different partition must be reset again, because its
// NVLinks remain trained for the previous partition.
func TestFabricPartitionResetOnPartitionChange(t *testing.T) {
	smi := newFakePersistenceSMI(t, 0)
	partitions, inventory := a100FabricIdentityFixture()
	setFabricTestArchitecture(inventory, "Ampere")
	client := &persistenceFMClient{testFMClient: &testFMClient{partitions: partitions}, beforeActivate: func() {}}

	single, singleClaim := persistenceClaimState(t, client, smi.lib, inventory, []int{1}, true)
	require.NoError(t, single.activateFabricPartition(singleClaim))
	require.Equal(t, []int{31}, client.activatedIDs)
	require.NoError(t, client.DeactivateFabricPartition(31))

	want := persistenceCommands(smi, inventory, []int{1})
	want = append(want, resetCommands(smi, inventory, []int{1})...)
	want = append(want, persistenceCommands(smi, inventory, []int{1, 2})...)
	want = append(want, resetCommands(smi, inventory, []int{1, 2})...)
	client.beforeActivate = func() {
		require.Equal(t, want, smi.commands(t), "every GPU of the new partition must be reset before activation")
	}
	pair, pairClaim := persistenceClaimState(t, client, smi.lib, inventory, []int{1, 2}, true)
	require.NoError(t, pair.activateFabricPartition(pairClaim))
	require.Equal(t, []int{31, 17}, client.activatedIDs)
}

func TestFabricPartitionResetFailure(t *testing.T) {
	smi := newFakePersistenceSMI(t, 0)
	// Let persistence changes succeed and fail every GPU reset.
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\ncase \"$*\" in *' -r') echo 'GPU is in use'; exit 1;; esac\n", smi.logPath)
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(smi.logPath), "chroot"), []byte(script), 0o755))
	partitions, inventory := a100FabricIdentityFixture()
	setFabricTestArchitecture(inventory, "Ampere")
	client := &testFMClient{partitions: partitions}
	state, claim := persistenceClaimState(t, client, smi.lib, inventory, []int{1, 2}, false)

	err := state.activateFabricPartition(claim)
	require.ErrorContains(t, err, "resetting GPU "+inventory[1].UUID)
	require.ErrorContains(t, err, inventory[1].pciBusID)
	require.ErrorContains(t, err, "GPU is in use")
	require.Empty(t, client.activatedIDs)
	want := persistenceCommands(smi, inventory, []int{1, 2})
	want = append(want, resetCommands(smi, inventory, []int{1})...)
	require.Equal(t, want, smi.commands(t))
}
