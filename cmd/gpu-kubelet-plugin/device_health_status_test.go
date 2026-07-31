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

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// newHealthTestDriver builds a driver with a single device seeded with the
// given health, ready to exercise health reporting.
func newHealthTestDriver(monitor deviceHealthMonitor, health kubeletplugin.DeviceHealth) *driver {
	// A GpuInfo with minor 0 yields the canonical name "gpu-0".
	dev := &AllocatableDevice{Gpu: &GpuInfo{}}
	return &driver{
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
			"gpu-0": health,
		},
	}
}

func healthyGPU0() kubeletplugin.DeviceHealth {
	return kubeletplugin.DeviceHealth{
		PoolName:    "node1",
		DeviceName:  "gpu-0",
		Health:      kubeletplugin.HealthStatusHealthy,
		LastUpdated: time.Now(),
	}
}

// enableHealthGate turns the NVMLDeviceHealthCheck feature gate on for the
// duration of the test; WatchHealthStatus declines subscriptions without it.
func enableHealthGate(t *testing.T) {
	t.Helper()
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.NVMLDeviceHealthCheck): true,
	}))
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
			string(featuregates.NVMLDeviceHealthCheck): false,
		}))
	})
}

// startWatch runs WatchHealthStatus in a goroutine, consumes and returns the
// initial snapshot, and hands back the reports channel plus a stop function
// which cancels the watch and asserts it returns cleanly.
func startWatch(t *testing.T, d *driver) (kubeletplugin.DeviceHealthReport, <-chan kubeletplugin.DeviceHealthReport, func()) {
	t.Helper()
	enableHealthGate(t)

	ctx, cancel := context.WithCancel(t.Context())
	reports := make(chan kubeletplugin.DeviceHealthReport)
	done := make(chan error, 1)
	go func() {
		done <- d.WatchHealthStatus(ctx, reports)
	}()

	var initial kubeletplugin.DeviceHealthReport
	select {
	case initial = <-reports:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for initial health report")
	}

	stop := func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("WatchHealthStatus returned error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for WatchHealthStatus to return")
		}
	}
	return initial, reports, stop
}

// receiveReport reads one health report or fails the test.
func receiveReport(t *testing.T, reports <-chan kubeletplugin.DeviceHealthReport) kubeletplugin.DeviceHealthReport {
	t.Helper()
	select {
	case report := <-reports:
		return report
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for health report")
		return kubeletplugin.DeviceHealthReport{}
	}
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

// TestDeviceHealthEvents_HeartbeatResends verifies that a heartbeat from the
// NVML monitor's event loop wakes the watcher and re-sends a fresh report,
// keeping the kubelet's health data fresh without a detached timer.
func TestDeviceHealthEvents_HeartbeatResends(t *testing.T) {
	monitor := &mockHealthMonitor{heartbeatCh: make(chan struct{}, 1)}
	d := newHealthTestDriver(monitor, healthyGPU0())

	initial, reports, stopWatch := startWatch(t, d)
	defer stopWatch()
	require.Len(t, initial.Devices, 1)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	eventsDone := make(chan struct{})
	go func() {
		defer close(eventsDone)
		d.deviceHealthEvents(ctx)
	}()

	monitor.heartbeatCh <- struct{}{}

	report := receiveReport(t, reports)
	require.Len(t, report.Devices, 1)
	assert.Equal(t, "gpu-0", report.Devices[0].DeviceName)
	assert.Equal(t, kubeletplugin.HealthStatusHealthy, report.Devices[0].Health)

	cancel()
	<-eventsDone
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
			// Start from a healthy device: this test checks the pure
			// event-to-status mapping, not the sticky-unhealthy rule
			// (see TestUpdateDeviceHealth_UnhealthyIsSticky).
			d := newHealthTestDriver(monitor, healthyGPU0())
			dev := d.state.perGPUAllocatable.allocatablesMap["0000:01:00.0"]["gpu-0"]
			tc.event.Devices = []*AllocatableDevice{dev}

			_, reports, stopWatch := startWatch(t, d)
			defer stopWatch()

			d.updateDeviceHealth(tc.event)

			report := receiveReport(t, reports)
			require.Len(t, report.Devices, 1)
			assert.Equal(t, tc.expectedHealth, report.Devices[0].Health)
		})
	}
}

// TestUpdateDeviceHealth_UnhealthyIsSticky verifies that once a device is
// unhealthy, later events cannot flip it back: a non-fatal XID after a
// critical one must not mask the unrecovered failure, mirroring the
// ResourceSlice taint lifecycle (which has no removal either). Recovery
// requires a driver restart after the device is fixed.
func TestUpdateDeviceHealth_UnhealthyIsSticky(t *testing.T) {
	monitor := &mockHealthMonitor{nonFatalXids: map[uint64]bool{31: true}}
	d := newHealthTestDriver(monitor, healthyGPU0())
	dev := d.state.perGPUAllocatable.allocatablesMap["0000:01:00.0"]["gpu-0"]

	_, reports, stopWatch := startWatch(t, d)
	defer stopWatch()

	// Critical XID marks the device unhealthy.
	d.updateDeviceHealth(&DeviceHealthEvent{
		EventType: HealthEventXID, EventData: 79, Devices: []*AllocatableDevice{dev},
	})
	report := receiveReport(t, reports)
	require.Equal(t, kubeletplugin.HealthStatusUnhealthy, report.Devices[0].Health)
	criticalMessage := report.Devices[0].Message

	// A later non-fatal XID must not flip the device back to healthy, and
	// the message must keep naming the critical failure.
	d.updateDeviceHealth(&DeviceHealthEvent{
		EventType: HealthEventXID, EventData: 31, Devices: []*AllocatableDevice{dev},
	})
	report = receiveReport(t, reports)
	assert.Equal(t, kubeletplugin.HealthStatusUnhealthy, report.Devices[0].Health)
	assert.Equal(t, criticalMessage, report.Devices[0].Message)

	// An unmonitored event must not upgrade it to unknown either.
	d.updateDeviceHealth(&DeviceHealthEvent{
		EventType: HealthEventUnmonitored, Devices: []*AllocatableDevice{dev},
	})
	report = receiveReport(t, reports)
	assert.Equal(t, kubeletplugin.HealthStatusUnhealthy, report.Devices[0].Health)
}

// TestNotifyHealthSubscribers_Coalesces verifies the notification semantics:
// updates arriving while a watcher is busy collapse into a single pending
// wake-up, and the report is built at send time, so the watcher sends one
// report reflecting the latest state instead of a queue of stale snapshots.
func TestNotifyHealthSubscribers_Coalesces(t *testing.T) {
	monitor := &mockHealthMonitor{nonFatalXids: map[uint64]bool{31: true}}
	d := newHealthTestDriver(monitor, healthyGPU0())
	dev := d.state.perGPUAllocatable.allocatablesMap["0000:01:00.0"]["gpu-0"]

	// Register a subscriber directly, without a running watcher, to model a
	// watcher which is busy while updates arrive.
	subscriber := make(chan struct{}, 1)
	d.healthSubMu.Lock()
	d.healthSubscribers = append(d.healthSubscribers, subscriber)
	d.healthSubMu.Unlock()

	// A burst of updates and a heartbeat while the watcher is busy.
	d.updateDeviceHealth(&DeviceHealthEvent{
		EventType: HealthEventXID, EventData: 31, Devices: []*AllocatableDevice{dev},
	})
	d.updateDeviceHealth(&DeviceHealthEvent{
		EventType: HealthEventXID, EventData: 79, Devices: []*AllocatableDevice{dev},
	})
	d.notifyHealthSubscribers()

	// Exactly one pending wake-up, no queued snapshots.
	require.Len(t, subscriber, 1)

	// What the watcher does on wake-up: build the report at send time. It
	// must reflect the latest state (the critical XID), not the first event
	// of the burst.
	<-subscriber
	report := d.buildHealthReport()
	require.Len(t, report.Devices, 1)
	assert.Equal(t, kubeletplugin.HealthStatusUnhealthy, report.Devices[0].Health)
	assert.Contains(t, report.Devices[0].Message, "critical XID 79")

	// Nothing left pending: the burst coalesced into that single wake-up.
	assert.Empty(t, subscriber)
}

// TestBuildHealthReport_Reconciles verifies that each report reflects the
// current allocatable devices: devices which appear after seeding (for
// example re-added after a VFIO unprepare) are picked up with their initial
// health, and entries for devices which no longer exist are pruned.
func TestBuildHealthReport_Reconciles(t *testing.T) {
	monitor := &mockHealthMonitor{}
	d := newHealthTestDriver(monitor, healthyGPU0())

	// A device which appears after seeding shows up in the next report,
	// seeded with its initial health.
	d.state.Lock()
	d.state.perGPUAllocatable.allocatablesMap["0000:02:00.0"] = AllocatableDevices{
		"gpu-1": {Gpu: &GpuInfo{}},
	}
	d.state.Unlock()

	report := d.buildHealthReport()
	require.Len(t, report.Devices, 2)
	byName := map[string]kubeletplugin.DeviceHealth{}
	for _, dev := range report.Devices {
		byName[dev.DeviceName] = dev
	}
	assert.Equal(t, kubeletplugin.HealthStatusHealthy, byName["gpu-1"].Health)

	// A device which disappears is pruned from the report and the map.
	d.state.Lock()
	delete(d.state.perGPUAllocatable.allocatablesMap, "0000:01:00.0")
	d.state.Unlock()

	report = d.buildHealthReport()
	require.Len(t, report.Devices, 1)
	assert.Equal(t, "gpu-1", report.Devices[0].DeviceName)
	_, orphaned := d.deviceHealth["gpu-0"]
	assert.False(t, orphaned, "expected pruned entry for removed device")
}
