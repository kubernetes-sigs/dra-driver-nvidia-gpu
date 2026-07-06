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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

// newHealthTestDriver builds a driver with a single unhealthy device and a
// registered health subscriber, ready to exercise the recovery loop.
func newHealthTestDriver(monitor deviceHealthMonitor, lastUpdated time.Time) (*driver, chan kubeletplugin.DeviceHealthReport) {
	// A GpuInfo with minor 0 yields the canonical name "gpu-0".
	dev := &AllocatableDevice{Gpu: &GpuInfo{}}
	d := &driver{
		nodeName:            "node1",
		deviceHealthMonitor: monitor,
		state: &DeviceState{
			perGPUAllocatable: &PerGPUAllocatableDevices{
				allocatablesMap: map[PCIBusID]AllocatableDevices{
					"0000:01:00.0": {"gpu-0": dev},
				},
			},
		},
		deviceHealth: map[string]kubeletplugin.DeviceHealth{
			"gpu-0": {
				PoolName:    "node1",
				DeviceName:  "gpu-0",
				Health:      kubeletplugin.HealthStatusUnhealthy,
				LastUpdated: lastUpdated,
				Message:     "critical XID 79 reported by NVML",
			},
		},
	}
	subscriber := make(chan kubeletplugin.DeviceHealthReport, 1)
	d.healthSubscribers = append(d.healthSubscribers, subscriber)
	return d, subscriber
}

// TestRecoverHealthyDevices_Recovers verifies that an unhealthy device which
// responds to probing and has been quiet for a full interval is reported
// healthy again without a driver restart.
func TestRecoverHealthyDevices_Recovers(t *testing.T) {
	d, subscriber := newHealthTestDriver(&mockHealthMonitor{}, time.Now().Add(-2*healthRecoveryInterval))

	d.recoverHealthyDevices()

	health := d.deviceHealth["gpu-0"]
	assert.Equal(t, kubeletplugin.HealthStatusHealthy, health.Health)
	assert.Contains(t, health.Message, "recovered")

	// The subscriber is notified with the full, updated report.
	require.Len(t, subscriber, 1)
	report := <-subscriber
	require.Len(t, report.Devices, 1)
	assert.Equal(t, kubeletplugin.HealthStatusHealthy, report.Devices[0].Health)
	assert.Equal(t, "gpu-0", report.Devices[0].DeviceName)
}

// TestRecoverHealthyDevices_ProbeFails verifies that a device which does not
// respond to probing stays unhealthy.
func TestRecoverHealthyDevices_ProbeFails(t *testing.T) {
	monitor := &mockHealthMonitor{probeErr: errors.New("GPU is lost")}
	d, subscriber := newHealthTestDriver(monitor, time.Now().Add(-2*healthRecoveryInterval))

	d.recoverHealthyDevices()

	assert.Equal(t, kubeletplugin.HealthStatusUnhealthy, d.deviceHealth["gpu-0"].Health)
	assert.Empty(t, subscriber)
}

// TestRecoverHealthyDevices_QuietPeriod verifies that a device with a recent
// health event is not recovered yet, even if it responds to probing.
func TestRecoverHealthyDevices_QuietPeriod(t *testing.T) {
	d, subscriber := newHealthTestDriver(&mockHealthMonitor{}, time.Now())

	d.recoverHealthyDevices()

	assert.Equal(t, kubeletplugin.HealthStatusUnhealthy, d.deviceHealth["gpu-0"].Health)
	assert.Empty(t, subscriber)
}

// TestInitDeviceHealth_Seeding verifies per-type seeding: monitored devices
// start healthy, dynamic MIG placeholders follow their parent, VFIO devices
// await the PCI probe.
func TestInitDeviceHealth_Seeding(t *testing.T) {
	parent := &GpuInfo{UUID: "GPU-parent"}
	d := &driver{
		nodeName: "node1",
		state: &DeviceState{
			perGPUAllocatable: &PerGPUAllocatableDevices{
				allocatablesMap: map[PCIBusID]AllocatableDevices{
					"0000:01:00.0": {
						"gpu-0":          &AllocatableDevice{Gpu: parent},
						"gpu-0-mig-1g.6": &AllocatableDevice{MigDynamic: &MigSpec{Parent: parent}},
						"gpu-0-vfio":     &AllocatableDevice{Vfio: &VfioDeviceInfo{UUID: "GPU-parent", PciBusID: "0000:01:00.0"}},
					},
				},
			},
		},
	}

	d.initDeviceHealth()

	assert.Equal(t, kubeletplugin.HealthStatusHealthy, d.deviceHealth["gpu-0"].Health)
	assert.Equal(t, kubeletplugin.HealthStatusHealthy, d.deviceHealth["gpu-0-mig-1g.6"].Health)
	assert.Contains(t, d.deviceHealth["gpu-0-mig-1g.6"].Message, "parent GPU")
	assert.Equal(t, kubeletplugin.HealthStatusUnknown, d.deviceHealth["gpu-0-vfio"].Health)
	assert.Equal(t, []string{"gpu-0-mig-1g.6"}, d.healthChildren["GPU-parent"])
}

// TestUpdateDeviceHealth_ParentPropagation verifies that a GPU-scoped health
// event also marks the GPU's dynamic MIG placeholders.
func TestUpdateDeviceHealth_ParentPropagation(t *testing.T) {
	parent := &GpuInfo{UUID: "GPU-parent"}
	gpuDev := &AllocatableDevice{Gpu: parent}
	d := &driver{
		nodeName:            "node1",
		deviceHealthMonitor: &mockHealthMonitor{},
		state: &DeviceState{
			perGPUAllocatable: &PerGPUAllocatableDevices{
				allocatablesMap: map[PCIBusID]AllocatableDevices{
					"0000:01:00.0": {
						"gpu-0":          gpuDev,
						"gpu-0-mig-1g.6": &AllocatableDevice{MigDynamic: &MigSpec{Parent: parent}},
					},
				},
			},
		},
	}
	d.initDeviceHealth()

	d.updateDeviceHealth(&DeviceHealthEvent{
		Devices:   []*AllocatableDevice{gpuDev},
		EventType: HealthEventGPULost,
	})

	assert.Equal(t, kubeletplugin.HealthStatusUnhealthy, d.deviceHealth["gpu-0"].Health)
	assert.Equal(t, kubeletplugin.HealthStatusUnhealthy, d.deviceHealth["gpu-0-mig-1g.6"].Health)
	assert.Contains(t, d.deviceHealth["gpu-0-mig-1g.6"].Message, "parent GPU")
}

// TestProbePCIDevice exercises the sysfs bus-level probe used for VFIO
// passthrough devices.
func TestProbePCIDevice(t *testing.T) {
	root := t.TempDir()

	present := filepath.Join(root, "0000:01:00.0")
	require.NoError(t, os.MkdirAll(present, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(present, "config"), []byte{0xde, 0x10, 0xb9, 0x26}, 0o644))

	dead := filepath.Join(root, "0000:02:00.0")
	require.NoError(t, os.MkdirAll(dead, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dead, "config"), []byte{0xff, 0xff, 0xff, 0xff}, 0o644))

	assert.NoError(t, probePCIDevice(root, "0000:01:00.0"))
	// NVML-style 8-digit domain is normalized.
	assert.NoError(t, probePCIDevice(root, "00000000:01:00.0"))
	assert.ErrorContains(t, probePCIDevice(root, "0000:02:00.0"), "all-ones")
	assert.ErrorContains(t, probePCIDevice(root, "0000:03:00.0"), "not present")
	assert.Error(t, probePCIDevice(root, ""))
}

// TestRefreshVfioHealth verifies that VFIO devices transition from unknown to
// healthy or unhealthy based on the PCI probe.
func TestRefreshVfioHealth(t *testing.T) {
	root := t.TempDir()
	dev := filepath.Join(root, "0000:01:00.0")
	require.NoError(t, os.MkdirAll(dev, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dev, "config"), []byte{0xde, 0x10, 0xb9, 0x26}, 0o644))

	origRoot := sysfsPCIDevicesRoot
	sysfsPCIDevicesRoot = root
	defer func() { sysfsPCIDevicesRoot = origRoot }()

	d := &driver{
		nodeName: "node1",
		state: &DeviceState{
			perGPUAllocatable: &PerGPUAllocatableDevices{
				allocatablesMap: map[PCIBusID]AllocatableDevices{
					"0000:01:00.0": {
						"gpu-0-vfio": &AllocatableDevice{Vfio: &VfioDeviceInfo{UUID: "GPU-x", PciBusID: "0000:01:00.0"}},
					},
				},
			},
		},
	}
	d.initDeviceHealth()
	subscriber := make(chan kubeletplugin.DeviceHealthReport, 3)
	d.healthSubscribers = append(d.healthSubscribers, subscriber)

	require.Equal(t, kubeletplugin.HealthStatusUnknown, d.deviceHealth["gpu-0-vfio"].Health)

	// Probe succeeds: unknown -> healthy (bus-level).
	d.refreshVfioHealth()
	assert.Equal(t, kubeletplugin.HealthStatusHealthy, d.deviceHealth["gpu-0-vfio"].Health)
	require.Len(t, subscriber, 1)
	report := <-subscriber
	require.Len(t, report.Devices, 1)
	assert.Equal(t, vfioHealthCheckTimeout, report.Devices[0].HealthCheckTimeout)

	// A pass without any change still sends: it is the VFIO heartbeat which
	// keeps the kubelet's lease for these devices fresh.
	d.refreshVfioHealth()
	require.Len(t, subscriber, 1)
	<-subscriber

	// Device falls off the bus: healthy -> unhealthy.
	require.NoError(t, os.WriteFile(filepath.Join(dev, "config"), []byte{0xff, 0xff, 0xff, 0xff}, 0o644))
	d.refreshVfioHealth()
	assert.Equal(t, kubeletplugin.HealthStatusUnhealthy, d.deviceHealth["gpu-0-vfio"].Health)
	require.Len(t, subscriber, 1)
}

// TestDeviceHealthEvents_HeartbeatResends verifies that the driver re-sends
// its current health report when the NVML monitor's event loop signals a
// heartbeat, keeping the kubelet's health data fresh without a detached timer.
func TestDeviceHealthEvents_HeartbeatResends(t *testing.T) {
	monitor := &mockHealthMonitor{heartbeatCh: make(chan struct{}, 1)}
	d, subscriber := newHealthTestDriver(monitor, time.Now())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.deviceHealthEvents(ctx, "node1")
	}()

	monitor.heartbeatCh <- struct{}{}

	select {
	case report := <-subscriber:
		require.Len(t, report.Devices, 1)
		assert.Equal(t, "gpu-0", report.Devices[0].DeviceName)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for heartbeat-driven resend")
	}

	cancel()
	<-done
}

// TestUpdateDeviceHealth_EventMapping verifies the event type to health status
// mapping, including that non-fatal XIDs keep the device healthy.
func TestUpdateDeviceHealth_EventMapping(t *testing.T) {
	testCases := []struct {
		name           string
		event          *DeviceHealthEvent
		expectedHealth kubeletplugin.HealthStatus
	}{
		{
			name:           "critical XID",
			event:          &DeviceHealthEvent{EventType: HealthEventXID, EventData: 79},
			expectedHealth: kubeletplugin.HealthStatusUnhealthy,
		},
		{
			name:           "non-fatal XID",
			event:          &DeviceHealthEvent{EventType: HealthEventXID, EventData: 31},
			expectedHealth: kubeletplugin.HealthStatusHealthy,
		},
		{
			name:           "GPU lost",
			event:          &DeviceHealthEvent{EventType: HealthEventGPULost},
			expectedHealth: kubeletplugin.HealthStatusUnhealthy,
		},
		{
			name:           "unmonitored",
			event:          &DeviceHealthEvent{EventType: HealthEventUnmonitored},
			expectedHealth: kubeletplugin.HealthStatusUnknown,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			monitor := &mockHealthMonitor{nonFatalXids: map[uint64]bool{31: true}}
			d, subscriber := newHealthTestDriver(monitor, time.Now())
			dev := d.state.perGPUAllocatable.allocatablesMap["0000:01:00.0"]["gpu-0"]
			tc.event.Devices = []*AllocatableDevice{dev}

			d.updateDeviceHealth(tc.event)

			require.Len(t, subscriber, 1)
			report := <-subscriber
			require.Len(t, report.Devices, 1)
			assert.Equal(t, tc.expectedHealth, report.Devices[0].Health)
		})
	}
}
