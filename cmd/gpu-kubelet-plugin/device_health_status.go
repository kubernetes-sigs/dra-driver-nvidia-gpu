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
	"fmt"
	"maps"
	"slices"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
)

// This file implements KEP-4680 device health reporting for the GPU kubelet
// plugin: the kubelet subscribes through [kubeletplugin.DRAPlugin]'s
// WatchHealthStatus and surfaces the health of the devices allocated to a pod
// in pod.status.containerStatuses[].allocatedResourcesStatus.
//
// There is no separate health state. The NVML health monitor puts DRA device
// taints (KEP-5055) on each AllocatableDevice and those taints are published
// in the ResourceSlice; the health reported to the kubelet is derived from the
// very same in-memory taints at report time. Whatever adds, keeps or removes
// a taint (a fatal XID, a sticky NoSchedule taint, a destroyed Dynamic MIG
// device) therefore shows up identically in the scheduler-facing and the
// pod-facing view. DeviceState.onTaintsChanged wakes the watchers after every
// taint modification.

// defaultDeviceHealthReportInterval is how often the current health of all
// devices is re-sent to the kubelet while nothing changes. The kubelet treats
// health data that is not refreshed within its health check timeout (30
// seconds by default) as unknown, so this must stay well below that.
const defaultDeviceHealthReportInterval = 10 * time.Second

// defaultDeviceHealthMaxBusy is how long the previous health is re-sent while
// the DeviceState lock stays busy. Prepare and Unprepare legitimately hold it
// for a while, but one that holds it longer than this is most likely stuck,
// for example in an uncancelable VFIO driver switch. The NVML event handler
// waits for the same lock, so no fault can be recorded meanwhile, and the
// devices are reported Unknown rather than vouched for with stale health.
const defaultDeviceHealthMaxBusy = 2 * time.Minute

// deviceHealthMonitorStaleAfter is how long the NVML health monitor may go
// without hearing from NVML before it can no longer vouch that a device is
// healthy. Its event wait returns at least every 5 seconds, so this allows for
// several missed rounds.
const deviceHealthMonitorStaleAfter = 30 * time.Second

// defaultDeviceHealthBusyRetryInterval is how soon a report is retried when a
// taint change wakes a watcher but the DeviceState lock is busy, so that the
// change does not wait for the next periodic resend.
const defaultDeviceHealthBusyRetryInterval = time.Second

// hasSchedulingEffect reports whether a taint keeps the scheduler from
// allocating the device, which is what makes the device Unhealthy. The zero
// value and None are informational only.
func hasSchedulingEffect(taint *resourceapi.DeviceTaint) bool {
	return taint.Effect == resourceapi.DeviceTaintEffectNoSchedule ||
		taint.Effect == resourceapi.DeviceTaintEffectNoExecute
}

// deviceHealthFromTaints derives the kubelet-facing health of a device from
// its taints:
//
//   - a taint with a scheduling effect makes it Unhealthy, and the message
//     names the most severe one: GPU lost, then a fatal XID, then any other;
//   - an unmonitored taint makes it Unknown;
//   - a non-fatal XID taint keeps it Healthy but surfaces the event;
//   - without taints it is Healthy.
func deviceHealthFromTaints(taints []resourceapi.DeviceTaint) (kubeletplugin.HealthStatus, string) {
	var gpuLost, fatalXID, other, unmonitored, nonFatalXID *resourceapi.DeviceTaint
	for i := range taints {
		taint := &taints[i]
		switch {
		case hasSchedulingEffect(taint) && taint.Key == TaintKeyGPULost:
			gpuLost = taint
		case hasSchedulingEffect(taint) && taint.Key == TaintKeyXID:
			fatalXID = taint
		case hasSchedulingEffect(taint):
			other = taint
		case taint.Key == TaintKeyUnmonitored:
			unmonitored = taint
		case taint.Key == TaintKeyXID:
			nonFatalXID = taint
		}
	}
	switch {
	case gpuLost != nil:
		return kubeletplugin.HealthStatusUnhealthy, "GPU is lost"
	case fatalXID != nil:
		return kubeletplugin.HealthStatusUnhealthy, fmt.Sprintf("fatal XID %s reported by NVML", fatalXID.Value)
	case other != nil:
		return kubeletplugin.HealthStatusUnhealthy, fmt.Sprintf("device is tainted with %s", other.Key)
	case unmonitored != nil:
		return kubeletplugin.HealthStatusUnknown, "device health is not monitored by NVML"
	case nonFatalXID != nil:
		return kubeletplugin.HealthStatusHealthy, fmt.Sprintf("non-fatal XID %s reported by NVML", nonFatalXID.Value)
	default:
		return kubeletplugin.HealthStatusHealthy, ""
	}
}

// deviceHealthReportLocked returns the current health of every allocatable
// device, derived from its taints. The DeviceState lock must be held.
//
// If the NVML health monitor has stopped hearing from NVML, a device without
// a fault is reported Unknown instead of Healthy: the monitor could not have
// recorded a new fault. Faults it did record stay Unhealthy.
func (d *driver) deviceHealthReportLocked() kubeletplugin.DeviceHealthReport {
	now := time.Now()
	var monitorStaleMessage string
	if d.deviceHealthMonitor != nil {
		if heartbeat := d.deviceHealthMonitor.LastHeartbeat(); now.Sub(heartbeat) > deviceHealthMonitorStaleAfter {
			monitorStaleMessage = fmt.Sprintf("NVML health monitor has not heard from NVML since %s", heartbeat.UTC().Format(time.RFC3339))
		}
	}
	devices := d.state.perGPUAllocatable.GetAllDevices()
	report := kubeletplugin.DeviceHealthReport{
		Devices: make([]kubeletplugin.DeviceHealth, 0, len(devices)),
	}
	// Sorted for a stable order in logs and tests.
	for _, name := range slices.Sorted(maps.Keys(devices)) {
		health, message := deviceHealthFromTaints(devices[name].Taints())
		if monitorStaleMessage != "" && health == kubeletplugin.HealthStatusHealthy {
			health, message = kubeletplugin.HealthStatusUnknown, monitorStaleMessage
		}
		report.Devices = append(report.Devices, kubeletplugin.DeviceHealth{
			PoolName:    d.state.config.flags.nodeName,
			DeviceName:  name,
			Health:      health,
			LastUpdated: now,
			Message:     message,
		})
	}
	return report
}

// tryDeviceHealthReport returns the current health of every allocatable device
// if the DeviceState lock can be taken right away. Prepare and Unprepare hold
// that lock for their whole duration (MIG creation, CDI spec generation,
// checkpoint I/O), which can exceed the kubelet's health check timeout; a
// report must never wait behind them, or every allocated GPU on the node
// decays to Unknown while NVML is fine.
func (d *driver) tryDeviceHealthReport() (kubeletplugin.DeviceHealthReport, bool) {
	if !d.state.TryLock() {
		return kubeletplugin.DeviceHealthReport{}, false
	}
	defer d.state.Unlock()
	return d.deviceHealthReportLocked(), true
}

// initialDeviceHealthReport waits until the DeviceState lock is free and
// returns the current health of every allocatable device. It polls instead of
// blocking on the lock so that it gives up when the kubelet's stream ends; a
// Prepare stuck holding the lock must not leak a WatchHealthStatus call per
// kubelet reconnect. Until the first report is sent the kubeletplugin helper
// reports the stream as stale and the kubelet shows the devices as Unknown.
func (d *driver) initialDeviceHealthReport(ctx context.Context, poll time.Duration) (kubeletplugin.DeviceHealthReport, bool) {
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		if report, ok := d.tryDeviceHealthReport(); ok {
			return report, true
		}
		select {
		case <-ctx.Done():
			return kubeletplugin.DeviceHealthReport{}, false
		case <-ticker.C:
		}
	}
}

// refreshDeviceHealthReport returns the previous report with its timestamps
// set to now. It is what gets re-sent while the DeviceState lock is busy.
func refreshDeviceHealthReport(previous kubeletplugin.DeviceHealthReport) kubeletplugin.DeviceHealthReport {
	now := time.Now()
	report := kubeletplugin.DeviceHealthReport{
		Devices: slices.Clone(previous.Devices),
	}
	for i := range report.Devices {
		report.Devices[i].LastUpdated = now
	}
	return report
}

// unverifiedDeviceHealthReport returns the devices of the previous report as
// Unknown. It is what gets sent once the DeviceState lock has been busy for
// longer than the driver is willing to vouch for the previous health.
func unverifiedDeviceHealthReport(previous kubeletplugin.DeviceHealthReport, verified time.Time) kubeletplugin.DeviceHealthReport {
	now := time.Now()
	message := fmt.Sprintf("device health not verified since %s: the driver is blocked by a long-running device operation",
		verified.UTC().Format(time.RFC3339))
	report := kubeletplugin.DeviceHealthReport{
		Devices: slices.Clone(previous.Devices),
	}
	for i := range report.Devices {
		report.Devices[i].Health = kubeletplugin.HealthStatusUnknown
		report.Devices[i].Message = message
		report.Devices[i].LastUpdated = now
	}
	return report
}

// addHealthWatcher registers and returns a wake-up channel for one
// WatchHealthStatus call.
func (d *driver) addHealthWatcher() chan struct{} {
	// Capacity one so that notifications coalesce: however many taint
	// changes happen while the watcher is busy sending, it wakes up once
	// and builds a report reflecting all of them.
	watcher := make(chan struct{}, 1)
	d.healthWatchersMu.Lock()
	defer d.healthWatchersMu.Unlock()
	if d.healthWatchers == nil {
		d.healthWatchers = make(map[chan struct{}]struct{})
	}
	d.healthWatchers[watcher] = struct{}{}
	return watcher
}

func (d *driver) removeHealthWatcher(watcher chan struct{}) {
	d.healthWatchersMu.Lock()
	defer d.healthWatchersMu.Unlock()
	delete(d.healthWatchers, watcher)
}

// notifyHealthWatchers wakes every active WatchHealthStatus call so that it
// sends a fresh report. It is safe to call under any lock: the watcher
// channels have capacity one, the send never blocks, and a pending wake-up
// is not duplicated. For the woken watcher to actually see the change it
// should run after the DeviceState lock is released, which is what
// DeviceState.notifyTaintsChanged guarantees.
func (d *driver) notifyHealthWatchers() {
	d.healthWatchersMu.Lock()
	defer d.healthWatchersMu.Unlock()
	for watcher := range d.healthWatchers {
		select {
		case watcher <- struct{}{}:
		default:
		}
	}
}

// WatchHealthStatus implements [kubeletplugin.DRAPlugin]. The kubeletplugin
// helper calls it when the kubelet subscribes to device health and takes care
// of the DRAResourceHealth gRPC API version the kubelet speaks. It sends the
// health of all devices immediately, after every taint change, and
// periodically so that the kubelet's health data never goes stale.
func (d *driver) WatchHealthStatus(ctx context.Context, reports chan<- kubeletplugin.DeviceHealthReport) error {
	if d.deviceHealthMonitor == nil {
		// Without the NVML health monitor (NVMLDeviceHealthCheck disabled)
		// the health service is not advertised, so the kubelet does not
		// normally subscribe; answer a stray subscription accordingly.
		return kubeletplugin.ErrHealthNotSupported
	}

	klog.V(4).Info("Kubelet subscribed to device health updates")
	defer klog.V(4).Info("Kubelet unsubscribed from device health updates")

	changed := d.addHealthWatcher()
	defer d.removeHealthWatcher(changed)

	interval := d.healthReportInterval
	if interval <= 0 {
		interval = defaultDeviceHealthReportInterval
	}
	maxBusy := d.healthReportMaxBusy
	if maxBusy <= 0 {
		maxBusy = defaultDeviceHealthMaxBusy
	}
	retryInterval := d.healthBusyRetryInterval
	if retryInterval <= 0 {
		retryInterval = defaultDeviceHealthBusyRetryInterval
	}
	resend := time.NewTicker(interval)
	defer resend.Stop()

	// The initial report waits for the lock: there is nothing to re-send
	// yet, and the kubelet expects a report covering all devices first.
	report, ok := d.initialDeviceHealthReport(ctx, retryInterval)
	if !ok {
		return nil
	}
	verified := time.Now()
	unverified := false
	// pending is set while a taint change has not been reported because the
	// lock was busy. The lock holder may be a short one, such as a duplicate
	// XID event, so retry polls for the lock until the change is reported
	// rather than leaving it for the next periodic resend.
	pending := false
	var retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case reports <- report:
		}

	wait:
		for {
			retried := false
			select {
			case <-ctx.Done():
				return nil
			case <-changed:
				pending = true
			case <-resend.C:
			case <-retry:
				retried = true
			}
			retry = nil

			if next, ok := d.tryDeviceHealthReport(); ok {
				if unverified {
					klog.Info("Device state no longer busy; reporting verified device health again")
				}
				report, verified, unverified, pending = next, time.Now(), false, false
				break wait
			}
			if pending {
				retry = time.After(retryInterval)
			}
			if retried {
				// Nothing new to send until the lock is free; the periodic
				// resend keeps the kubelet's data fresh meanwhile.
				continue
			}
			if time.Since(verified) <= maxBusy {
				klog.V(6).Info("Device state busy; re-sending the previous device health report")
				report = refreshDeviceHealthReport(report)
				break wait
			}
			if !unverified {
				klog.Warningf("Device state busy since %s; reporting device health as Unknown until it is released", verified.UTC().Format(time.RFC3339))
				unverified = true
			}
			report = unverifiedDeviceHealthReport(report, verified)
			break wait
		}
	}
}
