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
	"sort"
	"testing"

	resourceapi "k8s.io/api/resource/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func deviceNames(devices []resourceapi.Device) []string {
	names := make([]string, 0, len(devices))
	for _, d := range devices {
		names = append(names, d.Name)
	}
	sort.Strings(names)
	return names
}

func TestComputeDomainPublishedSlices(t *testing.T) {
	allocatable := AllocatableDevices{}
	for id := range 2048 {
		allocatable[fmt.Sprintf("channel-%d", id)] = &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: id}}
	}
	publication := computeDomainPublishedSlices(allocatable, true)
	require.Len(t, publication, 16)
	seen := make(map[string]bool)
	for _, slice := range publication {
		require.LessOrEqual(t, len(slice.Devices), resourceapi.ResourceSliceMaxDevices)
		for _, device := range slice.Devices {
			require.False(t, seen[device.Name])
			seen[device.Name] = true
		}
	}
	require.Len(t, seen, 2048)
	assert.Equal(t, publication, computeDomainPublishedSlices(allocatable, true))
}

func TestComputeDomainPublishedDevices(t *testing.T) {
	allocatable := AllocatableDevices{
		"channel-0": &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: 0}},
		"channel-1": &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: 1}},
		"daemon-0":  &AllocatableDevice{Daemon: &ComputeDomainDaemonInfo{ID: 0}},
	}

	t.Run("driver-managed publishes channel 0 and the daemon device", func(t *testing.T) {
		// Non-zero channels are always excluded (advertised from the control plane).
		assert.Equal(t, []string{"channel-0", "daemon-0"}, deviceNames(computeDomainPublishedDevices(allocatable, false)))
	})

	t.Run("host-managed omits the daemon device", func(t *testing.T) {
		assert.Equal(t, []string{"channel-0", "channel-1"}, deviceNames(computeDomainPublishedDevices(allocatable, true)))
	})
}
