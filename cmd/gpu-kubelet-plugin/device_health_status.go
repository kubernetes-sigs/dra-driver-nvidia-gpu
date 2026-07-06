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
	"os"
	"path/filepath"
	"strings"
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

// initDeviceHealth seeds the health map with all allocatable devices.
//
//   - Full GPUs and static MIG partitions are watched by the NVML monitor
//     directly and start out healthy.
//   - Dynamic MIG placeholders are abstract partitions of a parent GPU; their
//     health follows the parent (see the healthChildren index and
//     updateDeviceHealth).
//   - VFIO passthrough devices are invisible to NVML (and must not be touched
//     through it, since open NVML handles interfere with VFIO binding); their
//     health comes from periodic PCI bus-level probing via sysfs (see
//     refreshVfioHealth) and starts out unknown until the first probe.
func (d *driver) initDeviceHealth() {
	d.healthMu.Lock()
	defer d.healthMu.Unlock()

	d.deviceHealth = make(map[string]kubeletplugin.DeviceHealth)
	d.healthChildren = make(map[string][]string)
	for _, devices := range d.state.perGPUAllocatable.allocatablesMap {
		for devname, dev := range devices {
			health := kubeletplugin.HealthStatusHealthy
			var message string
			switch dev.Type() {
			case GpuDeviceType, MigStaticDeviceType:
				// Covered by the NVML health monitor.
			case MigDynamicDeviceType:
				if parent := dev.MigDynamic.Parent; parent != nil && parent.UUID != "" {
					d.healthChildren[parent.UUID] = append(d.healthChildren[parent.UUID], devname)
					message = "reflects the health of the parent GPU"
				} else {
					health = kubeletplugin.HealthStatusUnknown
					message = "dynamic MIG device has no parent GPU to derive health from"
				}
			case VfioDeviceType:
				health = kubeletplugin.HealthStatusUnknown
				message = "awaiting PCI bus-level health probe"
				// The lease matching the VFIO prober cadence is declared
				// from the first report on.
				d.deviceHealth[devname] = kubeletplugin.DeviceHealth{
					PoolName:           d.nodeName,
					DeviceName:         devname,
					Health:             health,
					LastUpdated:        time.Now(),
					HealthCheckTimeout: vfioHealthCheckTimeout,
					Message:            message,
				}
				continue
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

		// GPU-scoped events propagate to devices whose health follows this
		// GPU (dynamic MIG placeholders); a partition carved from an
		// unhealthy GPU is just as unhealthy.
		if dev.Type() != GpuDeviceType {
			continue
		}
		for _, child := range d.healthChildren[dev.UUID()] {
			d.deviceHealth[child] = kubeletplugin.DeviceHealth{
				PoolName:    d.nodeName,
				DeviceName:  child,
				Health:      health,
				LastUpdated: time.Now(),
				Message:     "parent GPU: " + message,
			}
		}
	}
	d.healthMu.Unlock()

	d.notifyHealthSubscribers(d.buildNvmlHealthReport())
}

// buildHealthReport snapshots the current health of all devices. Used for the
// initial report when the kubelet subscribes; subsequent reports are
// per-source (see buildNvmlHealthReport and buildVfioHealthReport).
func (d *driver) buildHealthReport() kubeletplugin.DeviceHealthReport {
	return d.buildHealthReportWhere(func(*AllocatableDevice) bool { return true })
}

// buildNvmlHealthReport snapshots the devices whose health is established via
// NVML: full GPUs, static MIG partitions, and dynamic MIG placeholders (which
// follow their parent GPU). It is sent on NVML monitor events and heartbeats,
// so that each send vouches only for devices NVML actually verified.
func (d *driver) buildNvmlHealthReport() kubeletplugin.DeviceHealthReport {
	return d.buildHealthReportWhere(func(dev *AllocatableDevice) bool {
		switch dev.Type() {
		case GpuDeviceType, MigStaticDeviceType, MigDynamicDeviceType:
			return true
		}
		return false
	})
}

// buildVfioHealthReport snapshots the VFIO passthrough devices, whose health
// is established by the PCI bus-level prober.
func (d *driver) buildVfioHealthReport() kubeletplugin.DeviceHealthReport {
	return d.buildHealthReportWhere(func(dev *AllocatableDevice) bool {
		return dev.Type() == VfioDeviceType
	})
}

func (d *driver) buildHealthReportWhere(include func(*AllocatableDevice) bool) kubeletplugin.DeviceHealthReport {
	d.healthMu.RLock()
	defer d.healthMu.RUnlock()

	var devices []kubeletplugin.DeviceHealth
	for _, devs := range d.state.perGPUAllocatable.allocatablesMap {
		for devname, dev := range devs {
			if !include(dev) {
				continue
			}
			if health, ok := d.deviceHealth[devname]; ok {
				devices = append(devices, health)
			}
		}
	}
	return kubeletplugin.DeviceHealthReport{Devices: devices}
}

// notifyHealthSubscribers fans a health report out to all pending
// WatchHealthStatus calls. The kubelet keeps the previous health of devices
// absent from a report until it goes stale, so per-source subset reports
// refresh exactly the devices their source verified. Sends are non-blocking:
// a subscriber which is not keeping up misses intermediate snapshots, not
// information, because each source's report is complete for that source.
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

// healthRecoveryInterval is how often the recovery loop probes unhealthy
// devices, and also the minimum quiet period (no new health events) before an
// unhealthy device may be considered healthy again.
const healthRecoveryInterval = 30 * time.Second

// deviceHealthRecovery periodically probes unhealthy devices and marks them
// healthy again once they respond via NVML and have been quiet for at least
// one full interval. Without this, a device which hit a transient error (for
// example an XID triggered by a misbehaving application) would be reported
// unhealthy until driver restart.
//
// Devices with an unknown health status are not recovered: unknown means the
// health monitor cannot watch the device, so a successful probe would not
// justify calling it healthy. Note that recovery applies to the health
// reported to the kubelet (KEP-4680) only; DRA device taints (KEP-5055) have
// no removal mechanism yet and remain in place.
//
// The same ticker drives the VFIO PCI prober, whose per-pass report send is
// the heartbeat for VFIO devices: if this loop wedges, those devices decay to
// unknown once their lease (vfioHealthCheckTimeout) lapses, independently of
// the NVML monitor's heartbeat.
func (d *driver) deviceHealthRecovery(ctx context.Context) {
	ticker := time.NewTicker(healthRecoveryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.V(6).Info("Stop probing unhealthy devices for recovery")
			return
		case <-ticker.C:
			d.refreshVfioHealth()
			d.recoverHealthyDevices()
		}
	}
}

// sysfsPCIDevicesRoot is the sysfs directory holding PCI device entries. A
// variable so tests can point it at a fixture.
var sysfsPCIDevicesRoot = "/sys/bus/pci/devices"

// probePCIDevice checks the bus-level health of a PCI device via sysfs. It
// deliberately involves no NVML: VFIO-bound devices are invisible to NVML and
// open NVML handles interfere with VFIO device binding.
func probePCIDevice(root, pciBusID string) error {
	addr := normalizePCIBusID(pciBusID)
	if addr == "" {
		return fmt.Errorf("device has no PCI bus ID")
	}
	config, err := os.ReadFile(filepath.Join(root, addr, "config"))
	if err != nil {
		return fmt.Errorf("PCI device %s is not present: %w", addr, err)
	}
	if len(config) < 4 {
		return fmt.Errorf("PCI device %s config space is truncated", addr)
	}
	// A device which fell off the bus reads all-ones.
	if config[0] == 0xff && config[1] == 0xff && config[2] == 0xff && config[3] == 0xff {
		return fmt.Errorf("PCI device %s does not respond (config space reads all-ones)", addr)
	}
	return nil
}

// normalizePCIBusID converts a PCI bus ID to the sysfs form: lower-case with a
// 4-digit domain (NVML reports an 8-digit domain, e.g. 00000000:65:00.0).
func normalizePCIBusID(id string) string {
	id = strings.ToLower(id)
	if parts := strings.SplitN(id, ":", 2); len(parts) == 2 && len(parts[0]) == 8 {
		id = parts[0][4:] + ":" + parts[1]
	}
	return id
}

// vfioHealthCheckTimeout is the lease declared for VFIO devices. It is twice
// the probing interval so that a single delayed probe does not flap the
// devices to unknown, while a dead prober still decays them within a minute.
const vfioHealthCheckTimeout = 2 * healthRecoveryInterval

// refreshVfioHealth updates the health of VFIO passthrough devices from a PCI
// bus-level probe. This intentionally reports link-level health only: once a
// GPU is handed to a guest, guest-side errors (XIDs) are not observable from
// the host.
func (d *driver) refreshVfioHealth() {
	found := false
	for _, devices := range d.state.perGPUAllocatable.allocatablesMap {
		for devname, dev := range devices {
			if dev.Type() != VfioDeviceType {
				continue
			}
			found = true

			health := kubeletplugin.HealthStatusHealthy
			message := "PCI bus-level health only (VFIO passthrough device)"
			if err := probePCIDevice(sysfsPCIDevicesRoot, dev.Vfio.PciBusID); err != nil {
				health = kubeletplugin.HealthStatusUnhealthy
				message = err.Error()
			}

			d.healthMu.Lock()
			d.deviceHealth[devname] = kubeletplugin.DeviceHealth{
				PoolName:           d.nodeName,
				DeviceName:         devname,
				Health:             health,
				LastUpdated:        time.Now(),
				HealthCheckTimeout: vfioHealthCheckTimeout,
				Message:            message,
			}
			d.healthMu.Unlock()
		}
	}
	if !found {
		return
	}

	// Send on every probing pass, changed or not: the send is the VFIO
	// heartbeat. The kubelet's lease for these devices is refreshed only by
	// evidence from this prober and decays to unknown if the prober stops.
	d.notifyHealthSubscribers(d.buildVfioHealthReport())
}

// recoverHealthyDevices marks unhealthy-but-responsive devices healthy again.
func (d *driver) recoverHealthyDevices() {
	// Snapshot the candidates so that NVML probing happens without holding
	// the health lock.
	d.healthMu.RLock()
	var candidates []string
	for name, health := range d.deviceHealth {
		if health.Health == kubeletplugin.HealthStatusUnhealthy && time.Since(health.LastUpdated) >= healthRecoveryInterval {
			candidates = append(candidates, name)
		}
	}
	d.healthMu.RUnlock()

	recovered := make(map[string]bool)
	for _, name := range candidates {
		dev := d.lookupAllocatableDevice(name)
		if dev == nil {
			continue
		}
		// VFIO devices are fully handled by refreshVfioHealth.
		if dev.Type() == VfioDeviceType {
			continue
		}
		if err := d.deviceHealthMonitor.ProbeDevice(dev); err != nil {
			klog.V(6).Infof("Device %s is still unhealthy: %v", name, err)
			continue
		}
		recovered[name] = true
	}
	if len(recovered) == 0 {
		return
	}

	changed := false
	d.healthMu.Lock()
	for name := range recovered {
		health, ok := d.deviceHealth[name]
		// Re-check under the lock: a health event which arrived while probing
		// wins over the probe result and restarts the quiet period.
		if !ok || health.Health != kubeletplugin.HealthStatusUnhealthy || time.Since(health.LastUpdated) < healthRecoveryInterval {
			continue
		}
		klog.Infof("Device %s recovered, reporting healthy again", name)
		d.deviceHealth[name] = kubeletplugin.DeviceHealth{
			PoolName:    d.nodeName,
			DeviceName:  name,
			Health:      kubeletplugin.HealthStatusHealthy,
			LastUpdated: time.Now(),
			Message:     "device recovered: responds to NVML and reported no health events recently",
		}
		changed = true
	}
	d.healthMu.Unlock()

	if changed {
		d.notifyHealthSubscribers(d.buildNvmlHealthReport())
	}
}

// lookupAllocatableDevice resolves a device name from the health map back to
// the allocatable device.
func (d *driver) lookupAllocatableDevice(name string) *AllocatableDevice {
	for _, devices := range d.state.perGPUAllocatable.allocatablesMap {
		if dev, ok := devices[name]; ok {
			return dev
		}
	}
	return nil
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
