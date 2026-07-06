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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

// newHealthTestDriver builds a driver with a single unhealthy device and a
// registered health subscriber, ready to exercise health reporting.
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

// TestInitDeviceHealth_Seeding verifies per-type seeding: devices covered by
// the NVML monitor start healthy, devices it cannot see (dynamic MIG
// placeholders, VFIO passthrough devices) report an unknown health status.
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
	assert.Equal(t, kubeletplugin.HealthStatusUnknown, d.deviceHealth["gpu-0-mig-1g.6"].Health)
	assert.Contains(t, d.deviceHealth["gpu-0-mig-1g.6"].Message, "not covered")
	assert.Equal(t, kubeletplugin.HealthStatusUnknown, d.deviceHealth["gpu-0-vfio"].Health)
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
