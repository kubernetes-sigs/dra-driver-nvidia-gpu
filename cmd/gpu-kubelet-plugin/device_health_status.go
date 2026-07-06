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

// initDeviceHealth seeds the health map with all allocatable devices. Full
// GPUs and static MIG partitions are watched by the NVML monitor directly and
// start out healthy; devices the monitor cannot see (dynamic MIG placeholders,
// VFIO passthrough devices) report an unknown health status.
func (d *driver) initDeviceHealth() {
	d.healthMu.Lock()
	defer d.healthMu.Unlock()

	d.deviceHealth = make(map[string]kubeletplugin.DeviceHealth)
	for _, devices := range d.state.perGPUAllocatable.allocatablesMap {
		for devname, dev := range devices {
			health := kubeletplugin.HealthStatusHealthy
			var message string
			switch dev.Type() {
			case GpuDeviceType, MigStaticDeviceType:
				// Covered by the NVML health monitor.
			default:
				health = kubeletplugin.HealthStatusUnknown
				message = fmt.Sprintf("%s devices are not covered by the NVML health monitor", dev.Type())
			}
			d.deviceHealth[devname] = kubeletplugin.DeviceHealth{
				PoolName:    d.nodeName,
				DeviceName:  devname,
				Health:      health,
				LastUpdated: time.Now(),
				Message:     message,
			}
		}
	}
}

// updateDeviceHealth records the health consequence of an NVML health event
// and notifies all pending WatchHealthStatus subscribers. The mapping mirrors
// healthEventToTaint: events which produce a NoSchedule taint mark the device
// unhealthy, unmonitored devices have an unknown health status, and non-fatal
// XIDs keep the device healthy while still surfacing the event message.
func (d *driver) updateDeviceHealth(event *DeviceHealthEvent) {
	health := kubeletplugin.HealthStatusUnhealthy
	var message string
	switch event.EventType {
	case HealthEventXID:
		if d.deviceHealthMonitor != nil && d.deviceHealthMonitor.IsEventNonFatal(event) {
			health = kubeletplugin.HealthStatusHealthy
			message = fmt.Sprintf("non-fatal XID %d reported by NVML", event.EventData)
		} else {
			message = fmt.Sprintf("critical XID %d reported by NVML", event.EventData)
		}
	case HealthEventGPULost:
		message = "GPU is lost"
	case HealthEventUnmonitored:
		health = kubeletplugin.HealthStatusUnknown
		message = "device health is not monitored"
	default:
		health = kubeletplugin.HealthStatusUnknown
		message = fmt.Sprintf("unknown health event type %q", event.EventType)
	}

	d.healthMu.Lock()
	for _, dev := range event.Devices {
		name := dev.CanonicalName()
		d.deviceHealth[name] = kubeletplugin.DeviceHealth{
			PoolName:    d.nodeName,
			DeviceName:  name,
			Health:      health,
			LastUpdated: time.Now(),
			Message:     message,
		}
	}
	d.healthMu.Unlock()

	d.notifyHealthSubscribers(d.buildHealthReport())
}

// buildHealthReport snapshots the current health of all devices.
func (d *driver) buildHealthReport() kubeletplugin.DeviceHealthReport {
	d.healthMu.RLock()
	defer d.healthMu.RUnlock()

	var devices []kubeletplugin.DeviceHealth
	for _, devs := range d.state.perGPUAllocatable.allocatablesMap {
		for devname := range devs {
			if health, ok := d.deviceHealth[devname]; ok {
				devices = append(devices, health)
			}
		}
	}
	return kubeletplugin.DeviceHealthReport{Devices: devices}
}

// notifyHealthSubscribers fans a health report out to all pending
// WatchHealthStatus calls. Sends are non-blocking: a subscriber which is not
// keeping up misses intermediate snapshots, not information, because every
// report is a complete snapshot.
func (d *driver) notifyHealthSubscribers(report kubeletplugin.DeviceHealthReport) {
	if len(report.Devices) == 0 {
		return
	}

	d.healthSubMu.RLock()
	defer d.healthSubMu.RUnlock()

	for _, subscriber := range d.healthSubscribers {
		select {
		case subscriber <- report:
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

	subscriber := make(chan kubeletplugin.DeviceHealthReport, 10)
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

	// Forward updates. Periodic resends (required because the kubelet
	// treats health data older than the health check timeout as stale) are
	// not generated here: they arrive through the subscriber channel, driven
	// by the NVML monitor's heartbeat, so that a resend is evidence the
	// monitoring loop is actually alive.
	for {
		select {
		case <-ctx.Done():
			return nil
		case report := <-subscriber:
			select {
			case <-ctx.Done():
				return nil
			case reports <- report:
			}
		}
	}
}
