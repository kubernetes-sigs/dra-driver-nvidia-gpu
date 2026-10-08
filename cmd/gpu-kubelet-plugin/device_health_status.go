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
func (d *driver) deviceHealthReportLocked() kubeletplugin.DeviceHealthReport {
	now := time.Now()
	devices := d.state.perGPUAllocatable.GetAllDevices()
	report := kubeletplugin.DeviceHealthReport{
		Devices: make([]kubeletplugin.DeviceHealth, 0, len(devices)),
	}
	// Sorted for a stable order in logs and tests.
	for _, name := range slices.Sorted(maps.Keys(devices)) {
		health, message := deviceHealthFromTaints(devices[name].Taints())
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

// deviceHealthReport waits for the DeviceState lock and returns the current
// health of every allocatable device.
func (d *driver) deviceHealthReport() kubeletplugin.DeviceHealthReport {
	d.state.Lock()
	defer d.state.Unlock()
	return d.deviceHealthReportLocked()
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

// nextDeviceHealthReport returns a fresh report when the DeviceState lock can
// be taken right away, and otherwise re-sends the previous one. Prepare and
// Unprepare hold that lock for their whole duration (MIG creation, CDI spec
// generation, checkpoint I/O), which can exceed the kubelet's health check
// timeout; a report must never wait behind them, or every allocated GPU on
// the node decays to Unknown while NVML is fine. A taint change that lands
// while the lock is busy is picked up by the next periodic resend.
func (d *driver) nextDeviceHealthReport(previous kubeletplugin.DeviceHealthReport) kubeletplugin.DeviceHealthReport {
	if !d.state.TryLock() {
		klog.V(6).Info("Device state busy; re-sending the previous device health report")
		return refreshDeviceHealthReport(previous)
	}
	defer d.state.Unlock()
	return d.deviceHealthReportLocked()
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
	resend := time.NewTicker(interval)
	defer resend.Stop()

	// The initial report waits for the lock: there is nothing to re-send
	// yet, and the kubelet expects a report covering all devices first.
	report := d.deviceHealthReport()
	for {
		select {
		case <-ctx.Done():
			return nil
		case reports <- report:
		}

		select {
		case <-ctx.Done():
			return nil
		case <-changed:
		case <-resend.C:
		}
		report = d.nextDeviceHealthReport(report)
	}
}
