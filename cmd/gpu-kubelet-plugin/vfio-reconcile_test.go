/*
 * Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/fabricmanager"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func TestIsVfioDriver(t *testing.T) {
	for _, tc := range []struct {
		driver string
		want   bool
	}{
		{"vfio-pci", true},
		// Variant drivers used on specific GPU generations must count too,
		// otherwise a Grace-based GPU is never recognized as stranded.
		{"nvgrace_gpu_vfio_pci", true},
		{"vfio_pci", true},
		{"nvidia", false},
		{"", false},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			require.Equal(t, tc.want, isVfioDriver(tc.driver))
		})
	}
}

// fmPartition builds a partition whose members carry the PCI bus ids FM
// reports (8-digit upper-case domain), so the tests exercise the same
// normalization the production path relies on.
func fmPartition(id int, active bool, moduleIDs ...int) fabricmanager.Partition {
	gpus := make([]fabricmanager.PartitionGPU, len(moduleIDs))
	for i, moduleID := range moduleIDs {
		gpus[i] = fabricmanager.PartitionGPU{
			PhysicalID: moduleID,
			PCIBusID:   fmPCIBusID(moduleID),
		}
	}
	return fabricmanager.Partition{ID: id, IsActive: active, GPUs: gpus}
}

func fmPCIBusID(moduleID int) string {
	const hex = "0123456789abcdef"
	return "00000000:0" + string(hex[moduleID]) + ":00.0"
}

func reclaimedSet(moduleIDs ...int) map[string]struct{} {
	out := make(map[string]struct{}, len(moduleIDs))
	for _, moduleID := range moduleIDs {
		out[fabricmanager.NormalizePCIBusID(fmPCIBusID(moduleID))] = struct{}{}
	}
	return out
}

func newFMState(t *testing.T, partitions ...fabricmanager.Partition) (*DeviceState, *testFMClient) {
	t.Helper()
	client := &testFMClient{partitions: partitions}
	manager, err := fabricmanager.Open(client)
	require.NoError(t, err)
	return &DeviceState{fmManager: manager}, client
}

func TestDeactivateFabricPartitionsForReclaimedGPUs(t *testing.T) {
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.FabricManagerPartitioning): true,
	}))

	t.Run("all GPUs reclaimed", func(t *testing.T) {
		state, client := newFMState(t, fmPartition(2, true, 1, 2))
		state.deactivateFabricPartitionsForReclaimedGPUs(reclaimedSet(1, 2))
		require.Equal(t, []int{2}, client.deactivatedIDs)
	})

	t.Run("partially reclaimed partition is left active", func(t *testing.T) {
		// The unreclaimed GPU may belong to a consumer this driver cannot see;
		// tearing the partition down would disrupt it.
		state, client := newFMState(t, fmPartition(1, true, 1, 2, 3, 4))
		state.deactivateFabricPartitionsForReclaimedGPUs(reclaimedSet(1, 2))
		require.Empty(t, client.deactivatedIDs)
	})

	t.Run("unrelated active partition is untouched", func(t *testing.T) {
		state, client := newFMState(t, fmPartition(3, true, 3, 4))
		state.deactivateFabricPartitionsForReclaimedGPUs(reclaimedSet(1, 2))
		require.Empty(t, client.deactivatedIDs)
	})

	t.Run("inactive partition is not deactivated", func(t *testing.T) {
		state, client := newFMState(t, fmPartition(2, false, 1, 2))
		state.deactivateFabricPartitionsForReclaimedGPUs(reclaimedSet(1, 2))
		require.Empty(t, client.deactivatedIDs)
	})

	t.Run("only fully covered partitions among several", func(t *testing.T) {
		state, client := newFMState(t,
			fmPartition(1, true, 1, 2, 3, 4), // partially covered -> kept
			fmPartition(2, true, 1, 2),       // fully covered -> released
			fmPartition(3, true, 3, 4),       // uncovered -> kept
		)
		state.deactivateFabricPartitionsForReclaimedGPUs(reclaimedSet(1, 2))
		require.Equal(t, []int{2}, client.deactivatedIDs)
	})

	t.Run("nothing reclaimed", func(t *testing.T) {
		state, client := newFMState(t, fmPartition(2, true, 1, 2))
		state.deactivateFabricPartitionsForReclaimedGPUs(nil)
		require.Empty(t, client.deactivatedIDs)
	})
}

func TestDeactivateFabricPartitionsForReclaimedGPUsFeatureGateOff(t *testing.T) {
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.FabricManagerPartitioning): false,
	}))
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
			string(featuregates.FabricManagerPartitioning): true,
		}))
	})

	state, client := newFMState(t, fmPartition(2, true, 1, 2))
	state.deactivateFabricPartitionsForReclaimedGPUs(reclaimedSet(1, 2))
	require.Empty(t, client.deactivatedIDs, "FM must not be touched while the feature gate is off")
}

func TestDeactivateFabricPartitionsForReclaimedGPUsNoFabricManager(t *testing.T) {
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.FabricManagerPartitioning): true,
	}))

	// The gate can be on while this node has no NVSwitch fabric, in which case
	// newFabricManager returns a nil Manager and reconciliation must still be
	// safe to run.
	state := &DeviceState{}
	require.NotPanics(t, func() {
		state.deactivateFabricPartitionsForReclaimedGPUs(reclaimedSet(1, 2))
	})
}
