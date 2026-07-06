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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
)

const (
	FullGPUInstanceID uint32 = 0xFFFFFFFF
)

const (
	TaintKeyXID         = DriverName + "/xid"
	TaintKeyGPULost     = DriverName + "/gpu-lost"
	TaintKeyUnmonitored = DriverName + "/unmonitored"
)

// DeviceHealthEventType classifies the category of health event detected by
// the NVML health monitor.
type DeviceHealthEventType string

const (
	HealthEventXID         DeviceHealthEventType = "xid"
	HealthEventGPULost     DeviceHealthEventType = "gpu-lost"
	HealthEventUnmonitored DeviceHealthEventType = "unmonitored"
)

// DeviceHealthEvent carries a typed health notification from the NVML health
// monitor to the driver's event handler, enabling the driver to set the
// appropriate DRA device taint per the Option A schema (KEP-5055).
// Devices is a batch: for GPU_LOST and unmonitored events where all affected devices
// are aggregated into a single event so the consumer applies one ResourceSlice
// update instead of N.
type DeviceHealthEvent struct {
	Devices   []*AllocatableDevice
	EventType DeviceHealthEventType
	// inspired by NVML Event type and only meaningful for xid errors.
	// may have to create a custom type based on future device-api
	EventData uint64
}

// healthEventToTaint maps a DeviceHealthEvent to the corresponding DRA
// DeviceTaint using the Option A taint key schema: one key per health
// dimension under the gpu.nvidia.com domain.
func healthEventToTaint(monitor deviceHealthMonitor, event *DeviceHealthEvent) *resourceapi.DeviceTaint {
	switch event.EventType {
	case HealthEventXID:
		effect := resourceapi.DeviceTaintEffectNoSchedule
		if monitor != nil && monitor.IsEventNonFatal(event) {
			effect = resourceapi.DeviceTaintEffectNone
		}
		return &resourceapi.DeviceTaint{
			Key:    TaintKeyXID,
			Value:  strconv.FormatUint(event.EventData, 10),
			Effect: effect,
		}
	case HealthEventGPULost:
		return &resourceapi.DeviceTaint{
			Key:    TaintKeyGPULost,
			Effect: resourceapi.DeviceTaintEffectNoSchedule,
		}
	case HealthEventUnmonitored:
		return &resourceapi.DeviceTaint{
			Key:    TaintKeyUnmonitored,
			Effect: resourceapi.DeviceTaintEffectNone,
		}
	default:
		klog.Errorf("Unknown health event type %q, defaulting to unmonitored taint", event.EventType)
		return &resourceapi.DeviceTaint{
			Key:    TaintKeyUnmonitored,
			Effect: resourceapi.DeviceTaintEffectNone,
		}
	}
}

// For a MIG device the placement is defined by the 3-tuple <parent UUID, GI, CI>.
// For a full device the returned 3-tuple is the device's uuid and (FullGPUInstanceID) 0xFFFFFFFF for the other two elements.
type devicePlacementMap map[string]map[uint32]map[uint32]*AllocatableDevice

// monitorHeartbeatInterval is how often the monitor's event loop signals that
// it is alive. The kubelet reports device health as unknown when it is not
// refreshed within each device's health check timeout (30 seconds by
// default), so the driver re-sends its health report on every heartbeat. The
// heartbeat deliberately originates from the event loop itself: if the loop
// wedges, the resends stop and the kubelet correctly decays the devices'
// health to unknown instead of trusting a stale report.
const monitorHeartbeatInterval = 15 * time.Second

type nvmlDeviceHealthMonitor struct {
	nvmllib           nvml.Interface
	eventSet          nvml.EventSet
	unhealthy         chan *DeviceHealthEvent
	heartbeat         chan struct{}
	lastHeartbeat     time.Time
	deviceByPlacement devicePlacementMap
	skippedXids       map[uint64]bool
	wg                sync.WaitGroup
}

func newNvmlDeviceHealthMonitor(config *Config, perGPUAllocatable *PerGPUAllocatableDevices, nvdevlib *deviceLib) (*nvmlDeviceHealthMonitor, error) {
	if nvdevlib.nvmllib == nil {
		return nil, fmt.Errorf("nvml library is nil")
	}
	if ret := nvdevlib.nvmllib.Init(); ret != nvml.SUCCESS {
		return nil, fmt.Errorf("failed to initialize NVML: %v", ret)
	}
	defer func() {
		_ = nvdevlib.nvmllib.Shutdown()
	}()

	if perGPUAllocatable == nil {
		return nil, fmt.Errorf("perGPUAllocatable is nil")
	}
	all := perGPUAllocatable.GetAllDevices()
	m := &nvmlDeviceHealthMonitor{
		nvmllib:           nvdevlib.nvmllib,
		unhealthy:         make(chan *DeviceHealthEvent, len(all)),
		heartbeat:         make(chan struct{}, 1),
		deviceByPlacement: getDevicePlacementMap(all),
		skippedXids:       xidsToSkip(config.flags.additionalXidsToIgnore),
	}
	return m, nil
}

func (m *nvmlDeviceHealthMonitor) Start(ctx context.Context) (rerr error) {
	if ret := m.nvmllib.Init(); ret != nvml.SUCCESS {
		return fmt.Errorf("failed to initialize NVML: %v", ret)
	}

	defer func() {
		if rerr != nil {
			_ = m.nvmllib.Shutdown()
		}
	}()

	klog.V(4).Info("creating NVML events for device health monitor")
	eventSet, ret := m.nvmllib.EventSetCreate()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("failed to create event set: %w", ret)
	}

	m.eventSet = eventSet

	klog.V(4).Info("registering NVML events for device health monitor")
	m.registerEventsForDevices()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.run(ctx)
	}()

	klog.V(4).Info("started device health monitoring")
	return nil
}

func (m *nvmlDeviceHealthMonitor) registerEventsForDevices() {
	eventMask := uint64(nvml.EventTypeXidCriticalError | nvml.EventTypeDoubleBitEccError | nvml.EventTypeSingleBitEccError)

	for parentUUID, giMap := range m.deviceByPlacement {
		gpu, ret := m.nvmllib.DeviceGetHandleByUUID(parentUUID)
		if ret != nvml.SUCCESS {
			klog.Warningf("Unable to get device handle from UUID[%s]: %v; marking devices as unmonitored", parentUUID, ret)
			m.sendHealthEventForDevices(giMap, HealthEventUnmonitored)
			continue
		}

		supportedEvents, ret := gpu.GetSupportedEventTypes()
		if ret != nvml.SUCCESS {
			klog.Warningf("unable to determine the supported events for %s: %v; marking devices as unmonitored", parentUUID, ret)
			m.sendHealthEventForDevices(giMap, HealthEventUnmonitored)
			continue
		}

		ret = gpu.RegisterEvents(eventMask&supportedEvents, m.eventSet)
		if ret == nvml.ERROR_NOT_SUPPORTED {
			klog.Warningf("Device %v is too old to support healthchecking.", parentUUID)
			m.sendHealthEventForDevices(giMap, HealthEventUnmonitored)
		} else if ret != nvml.SUCCESS {
			klog.Warningf("unable to register events for %s: %v; marking devices as unmonitored", parentUUID, ret)
			m.sendHealthEventForDevices(giMap, HealthEventUnmonitored)
		}
	}
}

func (m *nvmlDeviceHealthMonitor) Stop() {
	if m == nil {
		return
	}
	klog.V(6).Info("stopping health monitor")

	m.wg.Wait()

	if ret := m.eventSet.Free(); ret != nvml.SUCCESS {
		klog.Warningf("failed to unset events: %v", ret)
	}

	if ret := m.nvmllib.Shutdown(); ret != nvml.SUCCESS {
		klog.Warningf("failed to shutdown NVML: %v", ret)
	}
	close(m.unhealthy)
}

func (m *nvmlDeviceHealthMonitor) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			klog.V(6).Info("Stopping event-driven GPU health monitor...")
			return
		default:
			// Every pass of this loop, whether an event arrived or the wait
			// timed out, proves the monitor is alive.
			m.beat()

			event, ret := m.eventSet.Wait(5000) // timeout in 5000 ms.
			if ret == nvml.ERROR_TIMEOUT {
				continue
			}
			// not all return errors are handled as currently there is no proper way to process these errors other than marking all devices healthy.
			// Ref doc: [https://docs.nvidia.com/deploy/nvml-api/group__nvmlEvents.html#group__nvmlEvents_1g9714b0ca9a34c7a7780f87fee16b205c].
			if ret != nvml.SUCCESS {
				if ret == nvml.ERROR_GPU_IS_LOST {
					klog.Warningf("GPU is lost error: %v; Tainting all devices with %s", ret, TaintKeyGPULost)
					m.sendHealthEventForAllDevices(HealthEventGPULost)
					continue
				}
				klog.V(6).Infof("Error waiting for NVML event: %v. Retrying...", ret)
				continue
			}

			// TODO: check why other supported types are not considered?
			eType := event.EventType
			xid := event.EventData
			gi := event.GpuInstanceId
			ci := event.ComputeInstanceId
			if eType != nvml.EventTypeXidCriticalError {
				klog.V(6).Infof("Skipping non-nvmlEventTypeXidCriticalError event: Data=%d, Type=%d, GI=%d, CI=%d", xid, eType, gi, ci)
				continue
			}

			klog.V(4).Infof("Processing event XID=%d event", xid)
			// this seems an extreme action.
			// should we just log the error and proceed anyway.
			// TODO: look into how to properly handle this error.
			eventUUID, ret := event.Device.GetUUID()
			if ret != nvml.SUCCESS {
				klog.Warningf("Failed to determine uuid for event %v: %v; Tainting all devices with %s", event, ret, TaintKeyGPULost)
				m.sendHealthEventForAllDevices(HealthEventGPULost)
				continue
			}
			affectedDevice := m.deviceByPlacement.get(eventUUID, gi, ci)
			if affectedDevice == nil {
				klog.V(6).Infof("Ignoring event for unexpected device (UUID:%s, GI:%d, CI:%d)", eventUUID, gi, ci)
				continue
			}

			klog.V(4).Infof("Sending XID=%d health event for device %s", xid, affectedDevice.UUID())
			m.unhealthy <- &DeviceHealthEvent{
				Devices:   []*AllocatableDevice{affectedDevice},
				EventType: HealthEventXID,
				EventData: xid,
			}
		}
	}
}

func (m *nvmlDeviceHealthMonitor) Unhealthy() <-chan *DeviceHealthEvent {
	return m.unhealthy
}

// beat signals liveness of the event loop at most once per
// monitorHeartbeatInterval. Only called from the run goroutine.
func (m *nvmlDeviceHealthMonitor) beat() {
	if time.Since(m.lastHeartbeat) < monitorHeartbeatInterval {
		return
	}
	m.lastHeartbeat = time.Now()
	select {
	case m.heartbeat <- struct{}{}:
	default:
	}
}

// Heartbeat signals that the monitor's event loop is alive. The driver
// re-sends its current health report on each heartbeat to keep the kubelet's
// health data fresh.
func (m *nvmlDeviceHealthMonitor) Heartbeat() <-chan struct{} {
	return m.heartbeat
}

// ProbeDevice checks whether the device currently responds via NVML. The
// driver's health recovery loop uses it to detect that a previously unhealthy
// device works again (see device_health_status.go).
func (m *nvmlDeviceHealthMonitor) ProbeDevice(dev *AllocatableDevice) error {
	var parentUUID string
	switch dev.Type() {
	case GpuDeviceType:
		parentUUID = dev.UUID()
	case MigStaticDeviceType:
		parentUUID = dev.MigStatic.parent.UUID
	case MigDynamicDeviceType:
		// A dynamic MIG placeholder is an abstract partition of its parent
		// GPU: probing the parent decides its health.
		if dev.MigDynamic.Parent == nil {
			return fmt.Errorf("dynamic MIG device has no parent GPU")
		}
		parentUUID = dev.MigDynamic.Parent.UUID
	case VfioDeviceType:
		// VFIO-bound devices are invisible to NVML and must not be touched
		// through it (open NVML handles interfere with VFIO binding); probe
		// at the PCI bus level instead.
		return probePCIDevice(sysfsPCIDevicesRoot, dev.Vfio.PciBusID)
	default:
		return fmt.Errorf("device type %q does not support health probing", dev.Type())
	}
	if parentUUID == "" {
		return fmt.Errorf("device has no UUID")
	}

	gpu, ret := m.nvmllib.DeviceGetHandleByUUID(parentUUID)
	if ret != nvml.SUCCESS {
		return fmt.Errorf("device %s does not respond to NVML: %v", parentUUID, ret)
	}
	// A cheap query which requires talking to the device, to distinguish a
	// stale handle from a working device.
	if _, ret := gpu.GetPciInfo(); ret != nvml.SUCCESS {
		return fmt.Errorf("device %s failed PCI info query: %v", parentUUID, ret)
	}
	return nil
}

// sendHealthEventForAllDevices aggregates every device across all GPUs into a
// single batched DeviceHealthEvent so the consumer makes one ResourceSlice
// update.
func (m *nvmlDeviceHealthMonitor) sendHealthEventForAllDevices(eventType DeviceHealthEventType) {
	var devices []*AllocatableDevice
	for _, giMap := range m.deviceByPlacement {
		devices = append(devices, flattenMIGDeviceMap(giMap)...)
	}
	m.sendBatchedHealthEvent(devices, eventType)
}

// sendHealthEventForDevices aggregates all devices under a single parent GPU
// into one batched DeviceHealthEvent.
func (m *nvmlDeviceHealthMonitor) sendHealthEventForDevices(giMap map[uint32]map[uint32]*AllocatableDevice, eventType DeviceHealthEventType) {
	m.sendBatchedHealthEvent(flattenMIGDeviceMap(giMap), eventType)
}

// flattenMIGDeviceMap flattens a GI→CI device map into a slice.
func flattenMIGDeviceMap(giMap map[uint32]map[uint32]*AllocatableDevice) []*AllocatableDevice {
	var devices []*AllocatableDevice
	for _, ciMap := range giMap {
		for _, dev := range ciMap {
			devices = append(devices, dev)
		}
	}
	return devices
}

// sendBatchedHealthEvent sends a single DeviceHealthEvent containing all
// affected devices. Uses a non-blocking send to protect the monitor goroutine
// from deadlocks when the channel is full.
func (m *nvmlDeviceHealthMonitor) sendBatchedHealthEvent(devices []*AllocatableDevice, eventType DeviceHealthEventType) {
	if len(devices) == 0 {
		return
	}
	event := &DeviceHealthEvent{
		Devices:   devices,
		EventType: eventType,
	}
	select {
	case m.unhealthy <- event:
		klog.V(6).Infof("Sent batched %s health event for %d device(s)", eventType, len(devices))
	default:
		klog.Errorf("Health event channel full; dropping batched %s event for %d device(s)", eventType, len(devices))
	}
}

// The purpose of this function is to allow for a O(1) lookup of
// AllocatableDevice by ([parent]UUID, GI, CI) when processing health events. It
// currently assumes that this is constant for the lifetime of the healthchecker
// which does not hold for Dynamic MIG. This will have to be resolved once we
// support device health checking with dynamic MIG.
func getDevicePlacementMap(allocatable AllocatableDevices) devicePlacementMap {
	placementMap := make(devicePlacementMap)

	for _, d := range allocatable {
		var parentUUID string
		var giID, ciID uint32

		switch d.Type() {
		case GpuDeviceType:
			parentUUID = d.UUID()
			if parentUUID == "" {
				continue
			}
			giID = FullGPUInstanceID
			ciID = FullGPUInstanceID

		case MigStaticDeviceType:
			parentUUID = d.MigStatic.parent.UUID

			// Note(JP): it's unclear why we handle this case here (and why do
			// we think this can be empty?)
			if parentUUID == "" {
				continue
			}
			giID = d.MigStatic.gIInfo.Id
			ciID = d.MigStatic.cIInfo.Id

		default:
			// This may be a problem; and should be logged
			klog.V(4).Infof("getDevicePlacementMap: skipping device with type: %s", d.Type())
			continue
		}
		placementMap.addDevice(parentUUID, giID, ciID, d)
	}
	return placementMap
}

func (p devicePlacementMap) addDevice(parentUUID string, giID uint32, ciID uint32, d *AllocatableDevice) {
	if _, ok := p[parentUUID]; !ok {
		p[parentUUID] = make(map[uint32]map[uint32]*AllocatableDevice)
	}
	if _, ok := p[parentUUID][giID]; !ok {
		p[parentUUID][giID] = make(map[uint32]*AllocatableDevice)
	}
	p[parentUUID][giID][ciID] = d
}

func (p devicePlacementMap) get(uuid string, gi, ci uint32) *AllocatableDevice {
	giMap, ok := p[uuid]
	if !ok {
		return nil
	}

	ciMap, ok := giMap[gi]
	if !ok {
		return nil
	}
	return ciMap[ci]
}

// getAdditionalXids returns a list of additional Xids to skip from the specified string.
// The input is treated as a comma-separated string and all valid uint64 values are considered as Xid values.
// Invalid values are ignored.
// TODO: add list of EXPLICIT XIDs from [https://github.com/NVIDIA/k8s-device-plugin/pull/1443].
func getAdditionalXids(input string) []uint64 {
	if input == "" {
		return nil
	}

	var additionalXids []uint64
	klog.V(6).Infof("Creating a list of additional xids to ignore: [%s]", input)
	for _, additionalXid := range strings.Split(input, ",") {
		trimmed := strings.TrimSpace(additionalXid)
		if trimmed == "" {
			continue
		}
		xid, err := strconv.ParseUint(trimmed, 10, 64)
		if err != nil {
			klog.V(6).Infof("Ignoring malformed Xid value %v: %v", trimmed, err)
			continue
		}
		additionalXids = append(additionalXids, xid)
	}

	return additionalXids
}

func xidsToSkip(additionalXids string) map[uint64]bool {
	// Add the list of hardcoded disabled (ignored) XIDs:
	// http://docs.nvidia.com/deploy/xid-errors/index.html#topic_4
	// Application errors: the GPU should still be healthy.
	ignoredXids := []uint64{
		13,  // Graphics Engine Exception
		31,  // GPU memory page fault
		43,  // GPU stopped processing
		45,  // Preemptive cleanup, due to previous errors
		68,  // Video processor exception
		109, // Context Switch Timeout Error
	}

	skippedXids := make(map[uint64]bool)
	for _, id := range ignoredXids {
		skippedXids[id] = true
	}

	for _, additionalXid := range getAdditionalXids(additionalXids) {
		skippedXids[additionalXid] = true
	}
	return skippedXids
}

// IsEventNonFatal evaluates whether a hardware event is considered an application-level
// warning (None) rather than a critical hardware failure (NoSchedule).
// Currently, it only checks for XID events.
func (m *nvmlDeviceHealthMonitor) IsEventNonFatal(event *DeviceHealthEvent) bool {
	if event.EventType == HealthEventXID {
		return m.skippedXids[event.EventData]
	}
	return false
}
