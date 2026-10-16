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
	"testing"

	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/fabricmanager"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// This A100 topology has NVML module IDs 1-8 and FM physical IDs 9-16.
// The UUIDs are synthetic; PCI addresses and partition membership reproduce
// the hardware mapping. No constant offset maps all module IDs to physical IDs.
func a100FabricIdentityFixture() ([]fabricmanager.Partition, map[int]*GpuInfo) {
	modules := []int{5, 6, 7, 8, 1, 2, 3, 4}
	buses := []string{"91", "da", "8c", "d6", "15", "53", "0f", "50"}
	members := make([]fabricmanager.PartitionGPU, 8)
	gpus := make(map[int]*GpuInfo)
	for i, module := range modules {
		uuid := fmt.Sprintf("GPU-00000000-0000-0000-0000-%012d", module)
		members[i] = fabricmanager.PartitionGPU{PhysicalID: i + 9, UUID: uuid, PCIBusID: "00000000:" + buses[i] + ":00.0"}
		gpus[module] = &GpuInfo{UUID: uuid, pciBusID: "0000:" + buses[i] + ":00.0", gpuModuleID: module}
	}
	partitions := []fabricmanager.Partition{
		{ID: 2, GPUs: members},
		{ID: 9, GPUs: members[:4]},
		{ID: 10, GPUs: members[4:]},
	}
	for i := 0; i < 4; i++ {
		partitions = append(partitions, fabricmanager.Partition{ID: 15 + i, GPUs: members[2*i : 2*i+2]})
	}
	for i := 0; i < 8; i++ {
		partitions = append(partitions, fabricmanager.Partition{ID: 27 + i, GPUs: members[i : i+1]})
	}
	return partitions, gpus
}

func TestResolveFabricPartitionGPUIdentity(t *testing.T) {
	for name, tc := range map[string]struct {
		modules   []int
		want      int
		wantError bool
	}{
		"single module 3":  {[]int{3}, 33, false},
		"single module 5":  {[]int{5}, 27, false},
		"two GPUs":         {[]int{4, 3}, 18, false},
		"four GPUs":        {[]int{1, 2, 3, 4}, 10, false},
		"eight GPUs":       {[]int{8, 7, 6, 5, 4, 3, 2, 1}, 2, false},
		"unsupported pair": {[]int{1, 3}, 0, true},
		"empty":            {nil, 0, true},
	} {
		t.Run(name, func(t *testing.T) {
			partitions, inventory := a100FabricIdentityFixture()
			manager, err := fabricmanager.Open(&testFMClient{partitions: partitions})
			require.NoError(t, err)
			defer manager.Close()
			state := &DeviceState{fmManager: manager}
			var gpus []*GpuInfo
			for _, module := range tc.modules {
				gpus = append(gpus, inventory[module])
			}
			id, err := state.resolveFabricPartition(gpus)
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, id)
		})
	}
}

func TestFabricPartitionAttributesGPUIdentity(t *testing.T) {
	oldGate := featuregates.Enabled(featuregates.FabricManagerPartitioning)
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{string(featuregates.FabricManagerPartitioning): true}))
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{string(featuregates.FabricManagerPartitioning): oldGate}))
	})
	partitions, gpus := a100FabricIdentityFixture()
	manager, err := fabricmanager.Open(&testFMClient{partitions: partitions})
	require.NoError(t, err)
	defer manager.Close()
	state := &DeviceState{fmManager: manager}
	for _, module := range []int{3, 5} {
		gpu := gpus[module]
		require.NoError(t, state.attachFabricManagerPartitions(gpu))
		want := map[int]int{1: 33, 2: 18, 4: 10, 8: 2}
		if module == 5 {
			want = map[int]int{1: 27, 2: 15, 4: 9, 8: 2}
		}
		require.Equal(t, want, gpu.partitionsBySize)
		require.Equal(t, module, gpu.gpuModuleID)
		attrs := make(map[resourceapi.QualifiedName]resourceapi.DeviceAttribute)
		gpu.addFabricManagerAttributes(attrs)
		require.Equal(t, int64(module), *attrs["gpuModuleID"].IntValue)
		for size, id := range want {
			require.Equal(t, int64(id), *attrs[resourceapi.QualifiedName(fmt.Sprintf("partition%d", size))].IntValue)
		}
	}
}

func TestFabricPartitionLifecycleGPUIdentity(t *testing.T) {
	oldGate := featuregates.Enabled(featuregates.FabricManagerPartitioning)
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{string(featuregates.FabricManagerPartitioning): true}))
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{string(featuregates.FabricManagerPartitioning): oldGate}))
	})
	for _, vfio := range []bool{false, true} {
		t.Run(fmt.Sprintf("vfio=%t", vfio), func(t *testing.T) {
			partitions, gpus := a100FabricIdentityFixture()
			client := &testFMClient{partitions: partitions}
			manager, err := fabricmanager.Open(client)
			require.NoError(t, err)
			defer manager.Close()
			gpu := gpus[3]
			device := &AllocatableDevice{Gpu: gpu}
			if vfio {
				device = &AllocatableDevice{Vfio: &VfioDeviceInfo{UUID: "synthetic-vfio-uuid", parent: gpu}}
			}
			state := &DeviceState{fmManager: manager, config: &Config{flags: &Flags{}}, perGPUAllocatable: &PerGPUAllocatableDevices{allocatablesMap: map[PCIBusID]AllocatableDevices{PCIBusID(gpu.pciBusID): {"device": device}}}}
			claim := &resourceapi.ResourceClaim{Status: resourceapi.ResourceClaimStatus{Allocation: &resourceapi.AllocationResult{Devices: resourceapi.DeviceAllocationResult{Results: []resourceapi.DeviceRequestAllocationResult{{Driver: DriverName, Device: "device"}}}}}}
			require.NoError(t, state.activateFabricPartition(claim))
			require.Equal(t, []int{33}, client.activatedIDs)
			require.NoError(t, state.activateFabricPartition(claim))
			require.Equal(t, []int{33}, client.activatedIDs, "retry must not reactivate")
			require.NoError(t, state.deactivateFabricPartition("claim", &PreparedClaim{Status: claim.Status}, nil))
			require.Equal(t, []int{33}, client.deactivatedIDs)
		})
	}
}

func TestResolveFabricPartitionIdentityValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate    func(*GpuInfo)
		wantError bool
	}{
		"no NVML module ID":                    {mutate: func(g *GpuInfo) { g.gpuModuleID = 0 }},
		"module ID already equals physical ID": {mutate: func(g *GpuInfo) { g.gpuModuleID = 15 }},
		"UUID unavailable":                     {mutate: func(g *GpuInfo) { g.UUID = "" }},
		"PCI unavailable":                      {mutate: func(g *GpuInfo) { g.pciBusID = "" }},
		"conflicting UUID":                     {mutate: func(g *GpuInfo) { g.UUID = "GPU-other" }, wantError: true},
		"unknown GPU":                          {mutate: func(g *GpuInfo) { g.UUID = "GPU-other"; g.pciBusID = "0000:ff:00.0" }, wantError: true},
		"malformed PCI":                        {mutate: func(g *GpuInfo) { g.pciBusID = "0f:00.0" }, wantError: true},
		"missing identity":                     {mutate: func(g *GpuInfo) { g.UUID = ""; g.pciBusID = "" }, wantError: true},
	} {
		t.Run(name, func(t *testing.T) {
			partitions, gpus := a100FabricIdentityFixture()
			manager, err := fabricmanager.Open(&testFMClient{partitions: partitions})
			require.NoError(t, err)
			defer manager.Close()
			state := &DeviceState{fmManager: manager}
			gpu := gpus[3]
			tc.mutate(gpu)
			id, err := state.resolveFabricPartition([]*GpuInfo{gpu})
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 33, id)
		})
	}
}

// H100 and later FM topologies can omit UUID and PCI identity entirely. In
// that representation, FM physical IDs equal the NVML module IDs.
func TestFabricPartitionWithoutFMIdentity(t *testing.T) {
	oldGate := featuregates.Enabled(featuregates.FabricManagerPartitioning)
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{string(featuregates.FabricManagerPartitioning): true}))
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{string(featuregates.FabricManagerPartitioning): oldGate}))
	})
	for _, emptyPCI := range []string{"", "00000000:00:00.0"} {
		t.Run("PCI="+emptyPCI, func(t *testing.T) {
			partitions, gpus := a100FabricIdentityFixture()
			moduleByUUID := make(map[string]int)
			for module, gpu := range gpus {
				moduleByUUID[gpu.UUID] = module
			}
			for i := range partitions {
				members := make([]fabricmanager.PartitionGPU, len(partitions[i].GPUs))
				for j, gpu := range partitions[i].GPUs {
					members[j] = fabricmanager.PartitionGPU{PhysicalID: moduleByUUID[gpu.UUID], PCIBusID: emptyPCI}
				}
				partitions[i].GPUs = members
			}
			client := &testFMClient{partitions: partitions}
			manager, err := fabricmanager.Open(client)
			require.NoError(t, err)
			defer manager.Close()
			state := &DeviceState{fmManager: manager}
			for module, gpu := range gpus {
				require.NoError(t, state.attachFabricManagerPartitions(gpu))
				require.Len(t, gpu.partitionsBySize, 4)
				require.Equal(t, module, gpu.gpuModuleID)
			}
			for _, tc := range []struct {
				modules []int
				want    int
			}{
				{[]int{3}, 33},
				{[]int{4, 3}, 18},
				{[]int{1, 2, 3, 4}, 10},
				{[]int{1, 2, 3, 4, 5, 6, 7, 8}, 2},
			} {
				var selected []*GpuInfo
				for _, module := range tc.modules {
					selected = append(selected, gpus[module])
				}
				id, err := state.resolveFabricPartition(selected)
				require.NoError(t, err)
				require.Equal(t, tc.want, id)
			}
		})
	}
}
