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
	"sort"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
)

func deviceNames(devices []resourceapi.Device) []string {
	names := make([]string, 0, len(devices))
	for _, d := range devices {
		names = append(names, d.Name)
	}
	sort.Strings(names)
	return names
}

func driverManagedConfig() *Config {
	return &Config{
		flags:      &Flags{},
		imexConfig: imex.Config{Mode: imex.ModeDriverManaged, Isolation: imex.IsolationIMEXDomain},
	}
}

func TestComputeDomainPublishedDevices(t *testing.T) {
	allocatable := AllocatableDevices{
		"channel-0": &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: 0}},
		"channel-1": &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: 1}},
		"daemon-0":  &AllocatableDevice{Daemon: &ComputeDomainDaemonInfo{ID: 0}},
	}

	t.Run("driver-managed publishes channel 0 and the daemon device", func(t *testing.T) {
		// Non-zero channels are always excluded (advertised from the control plane).
		assert.Equal(t, []string{"channel-0", "daemon-0"}, deviceNames(computeDomainPublishedDevices(allocatable, driverManagedConfig())))
	})

	t.Run("host-managed omits the daemon device", func(t *testing.T) {
		assert.Equal(t, []string{"channel-0"}, deviceNames(computeDomainPublishedDevices(allocatable, hostManagedConfig())))
	})

	t.Run("consumable shares only makes the channel multi-allocatable", func(t *testing.T) {
		setConsumableSharesGate(t, true)
		config := driverManagedConfig()
		config.flags.consumableShares = "4"

		devices := computeDomainPublishedDevices(allocatable, config)

		require.Len(t, devices, 2)
		for _, d := range devices {
			switch d.Name {
			case "channel-0":
				assert.Equal(t, ptr.To(true), d.AllowMultipleAllocations)
				assert.Contains(t, d.Capacity, resourceapi.QualifiedName("shares"))
			case "daemon-0":
				assert.Nil(t, d.AllowMultipleAllocations, "daemon devices back one IMEX daemon per node and must stay exclusive")
				assert.Empty(t, d.Capacity)
			}
		}
	})
}
