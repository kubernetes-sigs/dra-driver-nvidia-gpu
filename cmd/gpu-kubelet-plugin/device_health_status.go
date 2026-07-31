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
	"time"

	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// This file implements device health reporting (KEP-4680) for the GPU kubelet
// plugin. It bridges the NVML device health monitor -- whose events also drive
// DRA device taints (KEP-5055) -- to the version-neutral
// [kubeletplugin.DRAPlugin] WatchHealthStatus API, so that the health of
// allocated GPUs surfaces in
// pod.status.containerStatuses[].allocatedResourcesStatus.

// newDeviceHealth returns the initial health entry for a device. Full GPUs
// and static MIG partitions are watched by the NVML monitor directly and
// start out healthy; devices the monitor cannot see (dynamic MIG placeholders,
// VFIO passthrough devices) report an unknown health status.
func (d *driver) newDeviceHealth(devname string, dev *AllocatableDevice) kubeletplugin.DeviceHealth {
	health := kubeletplugin.HealthStatusHealthy
	var message string
	switch dev.Type() {
	case GpuDeviceType, MigStaticDeviceType:
		// Covered by the NVML health monitor.
	default:
		health = kubeletplugin.HealthStatusUnknown
		message = fmt.Sprintf("%s devices are not covered by the NVML health monitor", dev.Type())
	}
	return kubeletplugin.DeviceHealth{
		PoolName:    d.nodeName,
		DeviceName:  devname,
		Health:      health,
		LastUpdated: time.Now(),
		Message:     message,
	}
}

// initDeviceHealth seeds the health map with all allocatable devices, so that
// the first report after the kubelet subscribes covers every device.
func (d *driver) initDeviceHealth() {
	d.healthMu.Lock()
	defer d.healthMu.Unlock()

	d.deviceHealth = make(map[string]kubeletplugin.DeviceHealth)
	for _, devices := range d.state.perGPUAllocatable.allocatablesMap {
		for devname, dev := range devices {
			d.deviceHealth[devname] = d.newDeviceHealth(devname, dev)
		}
	}
}

// updateDeviceHealth records the health consequence of an NVML health event
// and notifies all pending WatchHealthStatus subscribers. It shares the
// severity classification with the ResourceSlice taints (classifyHealthEvent):
// critical events mark the device unhealthy, unmonitored devices have an
// unknown health status, and non-fatal XIDs keep the device healthy while
// still surfacing the event message.
//
// Unhealthy is sticky: like the ResourceSlice taints from the same events, an
// unhealthy status is never cleared by a later event, because a later
// non-fatal event is no evidence that the earlier critical failure was fixed.
// Recovery requires fixing the device (GPU reset or reboot) and restarting
// the driver, which re-initializes the device as healthy.
func (d *driver) updateDeviceHealth(event *DeviceHealthEvent) {
	var health kubeletplugin.HealthStatus
	var message string
	switch classifyHealthEvent(d.deviceHealthMonitor, event) {
	case severityCritical:
		health = kubeletplugin.HealthStatusUnhealthy
		if event.EventType == HealthEventGPULost {
			message = "GPU is lost"
		} else {
			message = fmt.Sprintf("critical XID %d reported by NVML", event.EventData)
		}
	case severityNonFatal:
		health = kubeletplugin.HealthStatusHealthy
		message = fmt.Sprintf("non-fatal XID %d reported by NVML", event.EventData)
	default:
		// severityUnmonitored, including unknown event types.
		health = kubeletplugin.HealthStatusUnknown
		message = "device health is not monitored"
	}

	d.healthMu.Lock()
	for _, dev := range event.Devices {
		name := dev.CanonicalName()
		if current, ok := d.deviceHealth[name]; ok && current.Health == kubeletplugin.HealthStatusUnhealthy && health != kubeletplugin.HealthStatusUnhealthy {
			// Keep the device unhealthy and the message of the event which
			// made it so; only refresh the timestamp to show the status is
			// current.
			current.LastUpdated = time.Now()
			d.deviceHealth[name] = current
			continue
		}
		d.deviceHealth[name] = kubeletplugin.DeviceHealth{
			PoolName:    d.nodeName,
			DeviceName:  name,
			Health:      health,
			LastUpdated: time.Now(),
			Message:     message,
		}
	}
	d.healthMu.Unlock()

	d.notifyHealthSubscribers()
}

// buildHealthReport snapshots the current health of all devices, reconciling
// the health map with the current allocatable devices first: devices which
// appeared since the last report are seeded with their initial health, and
// entries for devices which no longer exist are pruned.
func (d *driver) buildHealthReport() kubeletplugin.DeviceHealthReport {
	// Snapshot the allocatable devices under the state lock: the maps are
	// mutated at claim prepare/unprepare time (for example VFIO sibling
	// removal and re-addition) and must not be iterated while that happens.
	d.state.Lock()
	allocatable := make(map[string]*AllocatableDevice)
	for _, devs := range d.state.perGPUAllocatable.allocatablesMap {
		for devname, dev := range devs {
			allocatable[devname] = dev
		}
	}
	d.state.Unlock()

	d.healthMu.Lock()
	defer d.healthMu.Unlock()

	if d.deviceHealth == nil {
		d.deviceHealth = make(map[string]kubeletplugin.DeviceHealth)
	}
	for devname, dev := range allocatable {
		if _, ok := d.deviceHealth[devname]; !ok {
			d.deviceHealth[devname] = d.newDeviceHealth(devname, dev)
		}
	}
	for devname := range d.deviceHealth {
		if _, ok := allocatable[devname]; !ok {
			delete(d.deviceHealth, devname)
		}
	}

	devices := make([]kubeletplugin.DeviceHealth, 0, len(d.deviceHealth))
	for _, health := range d.deviceHealth {
		devices = append(devices, health)
	}
	return kubeletplugin.DeviceHealthReport{Devices: devices}
}

// notifyHealthSubscribers wakes all pending WatchHealthStatus calls to send a
// fresh health report. The subscriber channels have capacity one and the send
// is non-blocking, so notifications coalesce: however many updates arrive
// while a subscriber is busy, it wakes once and builds the latest snapshot at
// send time. No health data is queued here; the current state lives in the
// deviceHealth map and this is only a notification.
func (d *driver) notifyHealthSubscribers() {
	d.healthSubMu.RLock()
	defer d.healthSubMu.RUnlock()

	for _, subscriber := range d.healthSubscribers {
		select {
		case subscriber <- struct{}{}:
		default:
		}
	}
}

// WatchHealthStatus implements [kubeletplugin.DRAPlugin]. The kubeletplugin
// helper calls it whenever the kubelet subscribes to device health updates and
// takes care of translating the reports into the DRAResourceHealth gRPC API
// version that the kubelet supports.
func (d *driver) WatchHealthStatus(ctx context.Context, reports chan<- kubeletplugin.DeviceHealthReport) error {
	if !featuregates.Enabled(featuregates.NVMLDeviceHealthCheck) {
		// The health service is not advertised in this case (see the
		// HealthService option in NewDriver), so the kubelet is not expected
		// to subscribe at all; answer any stray subscription accordingly.
		return kubeletplugin.ErrHealthNotSupported
	}

	klog.V(4).Info("Kubelet subscribed to device health updates")

	// A capacity-one notification channel: notifications coalesce, and the
	// report is built fresh at send time (see notifyHealthSubscribers).
	subscriber := make(chan struct{}, 1)
	d.healthSubMu.Lock()
	d.healthSubscribers = append(d.healthSubscribers, subscriber)
	d.healthSubMu.Unlock()

	defer func() {
		d.healthSubMu.Lock()
		for i, ch := range d.healthSubscribers {
			if ch == subscriber {
				d.healthSubscribers = append(d.healthSubscribers[:i], d.healthSubscribers[i+1:]...)
				break
			}
		}
		d.healthSubMu.Unlock()
		klog.V(4).Info("Kubelet unsubscribed from device health updates")
	}()

	select {
	case <-ctx.Done():
		return nil
	case reports <- d.buildHealthReport():
	}

	// Send a fresh snapshot on every notification. Periodic resends
	// (required because the kubelet treats health data older than the
	// health check timeout as stale) are not generated here: the
	// notifications are driven by the NVML monitor's heartbeat, so that a
	// resend is evidence the monitoring loop is actually alive.
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-subscriber:
			// Empty reports (a node with no allocatable devices) are sent
			// too: they are valid subset reports for the kubelet, and the
			// kubeletplugin helper resets its staleness tracking only when
			// it receives a report, so suppressing them would trigger
			// spurious staleness errors on device-less nodes.
			select {
			case <-ctx.Done():
				return nil
			case reports <- d.buildHealthReport():
			}
		}
	}
}
