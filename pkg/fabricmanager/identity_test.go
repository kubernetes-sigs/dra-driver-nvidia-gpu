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

package fabricmanager

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPhysicalIDForGPU(t *testing.T) {
	for name, tc := range map[string]struct {
		moduleID  int
		uuid      string
		pci       string
		members   []PartitionGPU
		wantID    int
		wantFound bool
		wantError string
	}{
		"UUID and normalized PCI":         {uuid: "GPU-a", pci: "0000:0f:00.0", members: []PartitionGPU{{PhysicalID: 15, UUID: "GPU-a", PCIBusID: "00000000:0F:00.0"}}, wantID: 15, wantFound: true},
		"UUID only":                       {uuid: "GPU-a", members: []PartitionGPU{{PhysicalID: 15, UUID: "GPU-a"}}, wantID: 15, wantFound: true},
		"PCI only":                        {pci: "0000:0f:00.0", members: []PartitionGPU{{PhysicalID: 15, UUID: "GPU-a", PCIBusID: "00000000:0F:00.0"}}, wantID: 15, wantFound: true},
		"FM UUID unavailable":             {uuid: "GPU-a", pci: "0000:0f:00.0", members: []PartitionGPU{{PhysicalID: 15, PCIBusID: "00000000:0F:00.0"}}, wantID: 15, wantFound: true},
		"same GPU in multiple partitions": {uuid: "GPU-a", members: []PartitionGPU{{PhysicalID: 15, UUID: "GPU-a"}, {PhysicalID: 15, UUID: "GPU-a"}}, wantID: 15, wantFound: true},
		"physical ID zero is valid":       {uuid: "GPU-a", members: []PartitionGPU{{PhysicalID: 0, UUID: "GPU-a"}}, wantFound: true},
		"unknown GPU":                     {uuid: "GPU-b", members: []PartitionGPU{{PhysicalID: 15, UUID: "GPU-a"}}},
		"missing identity":                {members: []PartitionGPU{{PhysicalID: 15}}},
		"significant domain bits":         {pci: "00010000:0f:00.0", members: []PartitionGPU{{PhysicalID: 15, PCIBusID: "00000000:0F:00.0"}}},
		"UUID conflicts":                  {uuid: "GPU-a", pci: "0000:0f:00.0", members: []PartitionGPU{{PhysicalID: 15, UUID: "GPU-b", PCIBusID: "0000:0f:00.0"}}, wantError: "conflicts"},
		"PCI conflicts":                   {uuid: "GPU-a", pci: "0000:0f:00.0", members: []PartitionGPU{{PhysicalID: 15, UUID: "GPU-a", PCIBusID: "0000:50:00.0"}}, wantError: "conflicts"},
		"ambiguous UUID":                  {uuid: "GPU-a", members: []PartitionGPU{{PhysicalID: 15, UUID: "GPU-a"}, {PhysicalID: 16, UUID: "GPU-a"}}, wantError: "multiple physical IDs"},
		"ambiguous PCI":                   {pci: "0000:0f:00.0", members: []PartitionGPU{{PhysicalID: 15, PCIBusID: "0000:0f:00.0"}, {PhysicalID: 16, PCIBusID: "00000000:0F:00.0"}}, wantError: "multiple physical IDs"},
		"FM omits identities":             {uuid: "GPU-a", pci: "0000:0f:00.0", moduleID: 3, members: []PartitionGPU{{PhysicalID: 3}}, wantID: 3, wantFound: true},
		"FM uses zero BDF":                {uuid: "GPU-a", pci: "0000:0f:00.0", moduleID: 3, members: []PartitionGPU{{PhysicalID: 3, PCIBusID: "00000000:00:00.0"}}, wantID: 3, wantFound: true},
		"missing module ID":               {uuid: "GPU-a", pci: "0000:0f:00.0", members: []PartitionGPU{{PhysicalID: 3}}},
		"unknown module ID":               {moduleID: 4, members: []PartitionGPU{{PhysicalID: 3}}},
		"module ID must not override unknown identity":     {uuid: "GPU-other", pci: "0000:50:00.0", moduleID: 15, members: []PartitionGPU{{PhysicalID: 15, UUID: "GPU-a", PCIBusID: "0000:0f:00.0"}}},
		"module ID must not override conflicting identity": {uuid: "GPU-other", pci: "0000:0f:00.0", moduleID: 15, members: []PartitionGPU{{PhysicalID: 15, UUID: "GPU-a", PCIBusID: "0000:0f:00.0"}}, wantError: "conflicts"},
		"mixed topology must not use module fallback":      {uuid: "GPU-a", moduleID: 3, members: []PartitionGPU{{PhysicalID: 3}, {PhysicalID: 4, UUID: "GPU-b"}}},
		"malformed query":      {pci: "0f:00.0", wantError: "invalid PCI bus ID"},
		"malformed FM address": {uuid: "GPU-a", members: []PartitionGPU{{PhysicalID: 15, UUID: "GPU-a", PCIBusID: "bad"}}, wantError: "invalid PCI bus ID"},
	} {
		t.Run(name, func(t *testing.T) {
			var partitions []Partition
			for i, gpu := range tc.members {
				partitions = append(partitions, Partition{ID: i, GPUs: []PartitionGPU{gpu}})
			}
			manager, err := Open(&fakeClient{partitions: partitions})
			require.NoError(t, err)
			defer manager.Close()
			id, found, err := manager.PhysicalIDForGPU(tc.uuid, tc.pci, tc.moduleID)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				require.False(t, found)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantFound, found)
			require.Equal(t, tc.wantID, id)
		})
	}
}

func TestNormalizePCIBusID(t *testing.T) {
	for input, want := range map[string]string{
		"":                 "",
		"00000000:0F:00.0": "0000:0f:00.0",
		"0000:0f:00.0":     "0000:0f:00.0",
		"00000001:FF:1F.7": "0001:ff:1f.7",
		"00010000:0F:00.0": "10000:0f:00.0",
	} {
		t.Run(input, func(t *testing.T) {
			got, err := normalizePCIBusID(input)
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
	for _, input := range []string{"0f:00.0", "garbage0000:0f:00.0", "0000:0f:20.0", "0000:0f:00.8", "0000:0f:00.0suffix"} {
		t.Run(input, func(t *testing.T) {
			_, err := normalizePCIBusID(input)
			require.Error(t, err)
		})
	}
}
