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
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

func TestDeviceHealthFromTaints(t *testing.T) {
	fatalXID := resourceapi.DeviceTaint{Key: TaintKeyXID, Value: "79", Effect: resourceapi.DeviceTaintEffectNoSchedule}
	nonFatalXID := resourceapi.DeviceTaint{Key: TaintKeyXID, Value: "31", Effect: resourceapi.DeviceTaintEffectNone}
	gpuLost := resourceapi.DeviceTaint{Key: TaintKeyGPULost, Effect: resourceapi.DeviceTaintEffectNoSchedule}
	unmonitored := resourceapi.DeviceTaint{Key: TaintKeyUnmonitored, Effect: resourceapi.DeviceTaintEffectNone}

	tests := []struct {
		name    string
		taints  []resourceapi.DeviceTaint
		health  kubeletplugin.HealthStatus
		message string
	}{
		{
			name:   "no taints",
			health: kubeletplugin.HealthStatusHealthy,
		},
		{
			name:    "non-fatal XID",
			taints:  []resourceapi.DeviceTaint{nonFatalXID},
			health:  kubeletplugin.HealthStatusHealthy,
			message: "non-fatal XID 31 reported by NVML",
		},
		{
			name:    "fatal XID",
			taints:  []resourceapi.DeviceTaint{fatalXID},
			health:  kubeletplugin.HealthStatusUnhealthy,
			message: "fatal XID 79 reported by NVML",
		},
		{
			name:    "GPU lost",
			taints:  []resourceapi.DeviceTaint{gpuLost},
			health:  kubeletplugin.HealthStatusUnhealthy,
			message: "GPU is lost",
		},
		{
			name:    "unmonitored",
			taints:  []resourceapi.DeviceTaint{unmonitored},
			health:  kubeletplugin.HealthStatusUnknown,
			message: "device health is not monitored by NVML",
		},
		{
			name:    "unmonitored wins over non-fatal XID",
			taints:  []resourceapi.DeviceTaint{nonFatalXID, unmonitored},
			health:  kubeletplugin.HealthStatusUnknown,
			message: "device health is not monitored by NVML",
		},
		{
			name:    "GPU lost wins over unmonitored regardless of order",
			taints:  []resourceapi.DeviceTaint{unmonitored, gpuLost},
			health:  kubeletplugin.HealthStatusUnhealthy,
			message: "GPU is lost",
		},
		{
			name:    "GPU lost wins over a fatal XID regardless of order",
			taints:  []resourceapi.DeviceTaint{fatalXID, gpuLost},
			health:  kubeletplugin.HealthStatusUnhealthy,
			message: "GPU is lost",
		},
		{
			name:    "unknown key with a scheduling effect is unhealthy",
			taints:  []resourceapi.DeviceTaint{{Key: "example.com/other", Effect: resourceapi.DeviceTaintEffectNoExecute}},
			health:  kubeletplugin.HealthStatusUnhealthy,
			message: "device is tainted with example.com/other",
		},
		{
			name:    "fatal XID wins over an unknown key with a scheduling effect",
			taints:  []resourceapi.DeviceTaint{{Key: "example.com/other", Effect: resourceapi.DeviceTaintEffectNoSchedule}, fatalXID},
			health:  kubeletplugin.HealthStatusUnhealthy,
			message: "fatal XID 79 reported by NVML",
		},
		{
			name:   "zero-value effect is informational, not a scheduling effect",
			taints: []resourceapi.DeviceTaint{{Key: "example.com/other"}},
			health: kubeletplugin.HealthStatusHealthy,
		},
		{
			name:    "zero-value effect on the XID key reads as non-fatal",
			taints:  []resourceapi.DeviceTaint{{Key: TaintKeyXID, Value: "31"}},
			health:  kubeletplugin.HealthStatusHealthy,
			message: "non-fatal XID 31 reported by NVML",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			health, message := deviceHealthFromTaints(tc.taints)
			assert.Equal(t, tc.health, health)
			assert.Equal(t, tc.message, message)
		})
	}
}

// TestDeviceHealthFromTaints_FatalIsSticky checks that the sticky NoSchedule
// taint (AddOrUpdateTaint) carries over into the reported health: a later
// non-fatal XID does not turn an Unhealthy device Healthy again.
func TestDeviceHealthFromTaints_FatalIsSticky(t *testing.T) {
	dev := &AllocatableDevice{Gpu: &GpuInfo{}}
	require.True(t, dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key: TaintKeyXID, Value: "79", Effect: resourceapi.DeviceTaintEffectNoSchedule,
	}))
	require.False(t, dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key: TaintKeyXID, Value: "31", Effect: resourceapi.DeviceTaintEffectNone,
	}))

	health, message := deviceHealthFromTaints(dev.Taints())
	assert.Equal(t, kubeletplugin.HealthStatusUnhealthy, health)
	assert.Equal(t, "fatal XID 79 reported by NVML", message)
}

// newTestHealthDriver returns a driver with the health monitor "enabled" and
// two GPUs, gpu-0 and gpu-1, in the pool named after the node, wired the same
// way NewDriver does: taint changes wake the health watchers, and unchanged
// health is re-sent at a short interval.
func newTestHealthDriver(t *testing.T) (*driver, map[DeviceName]*AllocatableDevice) {
	t.Helper()
	devices := map[DeviceName]*AllocatableDevice{
		"gpu-0": {Gpu: &GpuInfo{}},
		"gpu-1": {Gpu: &GpuInfo{}},
	}
	d := &driver{
		deviceHealthMonitor: &mockHealthMonitor{},
		state: &DeviceState{
			config: &Config{flags: &Flags{nodeName: "node-a"}},
			perGPUAllocatable: &PerGPUAllocatableDevices{
				allocatablesMap: map[PCIBusID]AllocatableDevices{
					"0000:01:00.0": {"gpu-0": devices["gpu-0"]},
					"0000:02:00.0": {"gpu-1": devices["gpu-1"]},
				},
			},
		},
		healthReportInterval: 20 * time.Millisecond,
	}
	d.state.onTaintsChanged = d.notifyHealthWatchers
	return d, devices
}

func TestDeviceHealthReport(t *testing.T) {
	d, devices := newTestHealthDriver(t)
	d.state.AddDeviceTaint(devices["gpu-1"], &resourceapi.DeviceTaint{
		Key: TaintKeyGPULost, Effect: resourceapi.DeviceTaintEffectNoSchedule,
	})

	before := time.Now()
	report := d.deviceHealthReport()
	require.Len(t, report.Devices, 2)

	assert.Equal(t, "node-a", report.Devices[0].PoolName)
	assert.Equal(t, "gpu-0", report.Devices[0].DeviceName)
	assert.Equal(t, kubeletplugin.HealthStatusHealthy, report.Devices[0].Health)
	assert.Empty(t, report.Devices[0].Message)
	assert.False(t, report.Devices[0].LastUpdated.Before(before))

	assert.Equal(t, "node-a", report.Devices[1].PoolName)
	assert.Equal(t, "gpu-1", report.Devices[1].DeviceName)
	assert.Equal(t, kubeletplugin.HealthStatusUnhealthy, report.Devices[1].Health)
	assert.Equal(t, "GPU is lost", report.Devices[1].Message)
}

func TestDeviceHealthReport_NoDevices(t *testing.T) {
	d, _ := newTestHealthDriver(t)
	d.state.perGPUAllocatable.allocatablesMap = nil
	report := d.deviceHealthReport()
	assert.Empty(t, report.Devices)
}

// receiveReport waits for the next report on the channel.
func receiveReport(t *testing.T, reports <-chan kubeletplugin.DeviceHealthReport) kubeletplugin.DeviceHealthReport {
	t.Helper()
	select {
	case report := <-reports:
		return report
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a device health report")
		return kubeletplugin.DeviceHealthReport{}
	}
}

func healthByDevice(report kubeletplugin.DeviceHealthReport) map[string]kubeletplugin.HealthStatus {
	out := make(map[string]kubeletplugin.HealthStatus, len(report.Devices))
	for _, dev := range report.Devices {
		out[dev.DeviceName] = dev.Health
	}
	return out
}

// startWatch runs WatchHealthStatus in the background and returns the report
// channel, a cancel function and a channel that carries the return value.
func startWatch(t *testing.T, d *driver) (<-chan kubeletplugin.DeviceHealthReport, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	reports := make(chan kubeletplugin.DeviceHealthReport)
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		done <- d.WatchHealthStatus(ctx, reports)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Error("WatchHealthStatus did not return after cancellation")
		}
	})
	return reports, cancel, done
}

func TestWatchHealthStatus_InitialReportAndTaintChange(t *testing.T) {
	d, devices := newTestHealthDriver(t)
	reports, cancel, done := startWatch(t, d)

	// The initial report covers all devices, all healthy.
	initial := receiveReport(t, reports)
	assert.Equal(t, map[string]kubeletplugin.HealthStatus{
		"gpu-0": kubeletplugin.HealthStatusHealthy,
		"gpu-1": kubeletplugin.HealthStatusHealthy,
	}, healthByDevice(initial))

	// A taint change wakes the watcher through DeviceState.onTaintsChanged
	// and produces a fresh report.
	d.state.AddDeviceTaint(devices["gpu-0"], &resourceapi.DeviceTaint{
		Key: TaintKeyXID, Value: "79", Effect: resourceapi.DeviceTaintEffectNoSchedule,
	})
	updated := receiveReport(t, reports)
	assert.Equal(t, map[string]kubeletplugin.HealthStatus{
		"gpu-0": kubeletplugin.HealthStatusUnhealthy,
		"gpu-1": kubeletplugin.HealthStatusHealthy,
	}, healthByDevice(updated))
	assert.Equal(t, "fatal XID 79 reported by NVML", updated.Devices[0].Message)

	// Cancellation ends the watch cleanly and unregisters the watcher.
	cancel()
	assert.NoError(t, <-done)
	d.healthWatchersMu.Lock()
	assert.Empty(t, d.healthWatchers)
	d.healthWatchersMu.Unlock()
}

func TestWatchHealthStatus_PeriodicResend(t *testing.T) {
	d, _ := newTestHealthDriver(t)
	reports, _, _ := startWatch(t, d)

	first := receiveReport(t, reports)
	// No notification: the next report is driven by the resend ticker and
	// carries the same health with a newer timestamp.
	second := receiveReport(t, reports)
	assert.Equal(t, healthByDevice(first), healthByDevice(second))
	assert.False(t, second.Devices[0].LastUpdated.Before(first.Devices[0].LastUpdated))
}

// TestWatchHealthStatus_StateLockBusy checks that a report is still sent
// while Prepare/Unprepare hold the DeviceState lock (the previous health with
// a fresh timestamp), and that the real health follows once the lock is free.
func TestWatchHealthStatus_StateLockBusy(t *testing.T) {
	d, devices := newTestHealthDriver(t)
	reports, _, _ := startWatch(t, d)
	initial := receiveReport(t, reports)

	// Hold the lock as a long-running Prepare would and taint a device
	// underneath (AddOrUpdateTaint is what AddDeviceTaint calls under the
	// lock). The watcher must not block on the lock.
	d.state.Lock()
	devices["gpu-1"].AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key: TaintKeyGPULost, Effect: resourceapi.DeviceTaintEffectNoSchedule,
	})
	d.notifyHealthWatchers()
	resent := receiveReport(t, reports)
	assert.Equal(t, healthByDevice(initial), healthByDevice(resent), "previous health is re-sent while the lock is busy")
	assert.False(t, resent.Devices[0].LastUpdated.Before(initial.Devices[0].LastUpdated))
	d.state.Unlock()

	// The next periodic resend takes the lock and reports the new health.
	deadline := time.Now().Add(5 * time.Second)
	for {
		report := receiveReport(t, reports)
		if report.Devices[1].Health == kubeletplugin.HealthStatusUnhealthy {
			assert.Equal(t, "GPU is lost", report.Devices[1].Message)
			break
		}
		require.True(t, time.Now().Before(deadline), "health never caught up after the lock was released")
	}
}

func TestAddDeviceTaint_NotifiesOnlyOnChange(t *testing.T) {
	d, devices := newTestHealthDriver(t)
	notified := 0
	d.state.onTaintsChanged = func() { notified++ }

	taint := &resourceapi.DeviceTaint{Key: TaintKeyXID, Value: "31", Effect: resourceapi.DeviceTaintEffectNone}
	assert.True(t, d.state.AddDeviceTaint(devices["gpu-0"], taint))
	assert.Equal(t, 1, notified)

	// Same taint again: nothing changed, nobody is woken.
	assert.False(t, d.state.AddDeviceTaint(devices["gpu-0"], taint))
	assert.Equal(t, 1, notified)

	// A nil hook (no driver wired) must be tolerated.
	d.state.onTaintsChanged = nil
	assert.True(t, d.state.AddDeviceTaint(devices["gpu-1"], taint))
}

// TestClearDynamicMIGXIDTaint_NotifiesAfterUnlock checks the Unprepare path:
// clearing a taint under the lock records the change, and notifyTaintsChanged
// (deferred by Unprepare to run after Unlock, on success and error paths)
// reports it exactly once.
func TestClearDynamicMIGXIDTaint_NotifiesAfterUnlock(t *testing.T) {
	dynamic := &AllocatableDevice{MigDynamic: &MigSpec{}}
	dynamic.AddOrUpdateTaint(&resourceapi.DeviceTaint{Key: TaintKeyXID, Value: "43"})
	state := &DeviceState{
		perGPUAllocatable: &PerGPUAllocatableDevices{
			allocatablesMap: map[PCIBusID]AllocatableDevices{"0000:01:00.0": {"dynamic": dynamic}},
		},
	}
	notified := 0
	state.onTaintsChanged = func() { notified++ }

	state.Lock()
	assert.True(t, state.clearDynamicMIGXIDTaint("dynamic"))
	assert.Equal(t, 0, notified, "not notified while the lock is held")
	state.Unlock()

	state.notifyTaintsChanged()
	assert.Equal(t, 1, notified)
	state.notifyTaintsChanged()
	assert.Equal(t, 1, notified, "a change is reported once")

	// Nothing to clear: nothing to report.
	state.Lock()
	assert.False(t, state.clearDynamicMIGXIDTaint("dynamic"))
	state.Unlock()
	state.notifyTaintsChanged()
	assert.Equal(t, 1, notified)
}

func TestWatchHealthStatus_CancelWhileSending(t *testing.T) {
	d, _ := newTestHealthDriver(t)
	// Nobody ever receives from reports, so the watch blocks on its first
	// send until the context is canceled.
	_, cancel, done := startWatch(t, d)
	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("WatchHealthStatus did not return after cancellation")
	}
}

func TestWatchHealthStatus_WithoutMonitorIsUnsupported(t *testing.T) {
	d, _ := newTestHealthDriver(t)
	d.deviceHealthMonitor = nil
	err := d.WatchHealthStatus(context.Background(), make(chan kubeletplugin.DeviceHealthReport))
	assert.ErrorIs(t, err, kubeletplugin.ErrHealthNotSupported)
}

func TestNotifyHealthWatchers_CoalescesAndNeverBlocks(t *testing.T) {
	d, _ := newTestHealthDriver(t)

	// No watchers: must not block or panic.
	d.notifyHealthWatchers()

	watcher := d.addHealthWatcher()
	d.notifyHealthWatchers()
	d.notifyHealthWatchers()
	d.notifyHealthWatchers()

	// Exactly one wake-up is pending.
	select {
	case <-watcher:
	default:
		t.Fatal("expected a pending wake-up")
	}
	select {
	case <-watcher:
		t.Fatal("expected wake-ups to coalesce into one")
	default:
	}

	d.removeHealthWatcher(watcher)
	d.notifyHealthWatchers()
	select {
	case <-watcher:
		t.Fatal("removed watcher must not be woken")
	default:
	}
}
