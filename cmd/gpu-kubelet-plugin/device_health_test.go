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

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	resourceapi "k8s.io/api/resource/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockHealthMonitor implements deviceHealthMonitor for testing healthEventToTaint.
type mockHealthMonitor struct {
	nonFatalXids map[uint64]bool
}

func (m *mockHealthMonitor) Start(context.Context) error          { return nil }
func (m *mockHealthMonitor) Stop()                                {}
func (m *mockHealthMonitor) Unhealthy() <-chan *DeviceHealthEvent { return nil }
func (m *mockHealthMonitor) IsEventNonFatal(e *DeviceHealthEvent) bool {
	if e.EventType == HealthEventXID {
		return m.nonFatalXids[e.EventData]
	}
	return false
}

func TestAddOrUpdateTaint_NewTaint(t *testing.T) {
	dev := &AllocatableDevice{}
	taint := &resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "48",
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	}

	changed := dev.AddOrUpdateTaint(taint)

	require.True(t, changed)
	require.Len(t, dev.Taints(), 1)
	assert.Equal(t, TaintKeyXID, dev.Taints()[0].Key)
	assert.Equal(t, "48", dev.Taints()[0].Value)
	assert.Equal(t, resourceapi.DeviceTaintEffectNoSchedule, dev.Taints()[0].Effect)
}

func TestAddOrUpdateTaint_DuplicateNoChange(t *testing.T) {
	dev := &AllocatableDevice{}
	taint := &resourceapi.DeviceTaint{
		Key:    TaintKeyGPULost,
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	}

	dev.AddOrUpdateTaint(taint)
	changed := dev.AddOrUpdateTaint(taint)

	assert.False(t, changed, "identical taint should not count as a change")
	assert.Len(t, dev.Taints(), 1)
}

func TestAddOrUpdateTaint_UpdateValue(t *testing.T) {
	dev := &AllocatableDevice{}
	dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "48",
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	})

	changed := dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "63",
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	})

	require.True(t, changed)
	require.Len(t, dev.Taints(), 1)
	assert.Equal(t, "63", dev.Taints()[0].Value, "value should be overwritten to latest XID")
}

func TestAddOrUpdateTaint_UpdateEffect(t *testing.T) {
	dev := &AllocatableDevice{}
	dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "48",
		Effect: resourceapi.DeviceTaintEffectNone,
	})

	changed := dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "48",
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	})

	require.True(t, changed)
	assert.Equal(t, resourceapi.DeviceTaintEffectNoSchedule, dev.Taints()[0].Effect)
}

func TestAddOrUpdateTaint_DifferentKeysAppended(t *testing.T) {
	dev := &AllocatableDevice{}
	dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "48",
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	})
	dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyGPULost,
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	})

	taints := dev.Taints()
	require.Len(t, taints, 2)
	assert.Equal(t, TaintKeyXID, taints[0].Key)
	assert.Equal(t, TaintKeyGPULost, taints[1].Key)
}

func TestAddOrUpdateTaint_TimeAddedResetOnChange(t *testing.T) {
	dev := &AllocatableDevice{}
	dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "48",
		Effect: resourceapi.DeviceTaintEffectNone,
	})

	dev.AddOrUpdateTaint(&resourceapi.DeviceTaint{
		Key:    TaintKeyXID,
		Value:  "63",
		Effect: resourceapi.DeviceTaintEffectNoSchedule,
	})

	assert.Nil(t, dev.Taints()[0].TimeAdded, "TimeAdded should be nil so the API server sets a fresh timestamp")
}

func TestHealthEventToTaint(t *testing.T) {
	monitor := &mockHealthMonitor{
		nonFatalXids: map[uint64]bool{13: true, 31: true},
	}

	tests := []struct {
		name           string
		event          *DeviceHealthEvent
		monitor        deviceHealthMonitor
		expectedKey    string
		expectedValue  string
		expectedEffect resourceapi.DeviceTaintEffect
	}{
		{
			name: "fatal XID",
			event: &DeviceHealthEvent{
				EventType: HealthEventXID,
				EventData: 48,
			},
			monitor:        monitor,
			expectedKey:    TaintKeyXID,
			expectedValue:  "48",
			expectedEffect: resourceapi.DeviceTaintEffectNoSchedule,
		},
		{
			name: "non-fatal XID (skipped)",
			event: &DeviceHealthEvent{
				EventType: HealthEventXID,
				EventData: 13,
			},
			monitor:        monitor,
			expectedKey:    TaintKeyXID,
			expectedValue:  "13",
			expectedEffect: resourceapi.DeviceTaintEffectNone,
		},
		{
			name: "XID with nil monitor defaults to fatal",
			event: &DeviceHealthEvent{
				EventType: HealthEventXID,
				EventData: 13,
			},
			monitor:        nil,
			expectedKey:    TaintKeyXID,
			expectedValue:  "13",
			expectedEffect: resourceapi.DeviceTaintEffectNoSchedule,
		},
		{
			name: "GPU lost",
			event: &DeviceHealthEvent{
				EventType: HealthEventGPULost,
			},
			monitor:        monitor,
			expectedKey:    TaintKeyGPULost,
			expectedValue:  "",
			expectedEffect: resourceapi.DeviceTaintEffectNoSchedule,
		},
		{
			name: "unmonitored",
			event: &DeviceHealthEvent{
				EventType: HealthEventUnmonitored,
			},
			monitor:        monitor,
			expectedKey:    TaintKeyUnmonitored,
			expectedValue:  "",
			expectedEffect: resourceapi.DeviceTaintEffectNone,
		},
		{
			name: "unknown event type defaults to unmonitored",
			event: &DeviceHealthEvent{
				EventType: DeviceHealthEventType("bogus"),
			},
			monitor:        monitor,
			expectedKey:    TaintKeyUnmonitored,
			expectedValue:  "",
			expectedEffect: resourceapi.DeviceTaintEffectNone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			taint := healthEventToTaint(tc.monitor, tc.event)
			assert.Equal(t, tc.expectedKey, taint.Key)
			assert.Equal(t, tc.expectedValue, taint.Value)
			assert.Equal(t, tc.expectedEffect, taint.Effect)
		})
	}
}

func TestIsEventNonFatal(t *testing.T) {
	m := &nvmlDeviceHealthMonitor{
		skippedXids: map[uint64]bool{
			13: true,
			31: true,
			43: true,
		},
	}

	tests := []struct {
		name     string
		event    *DeviceHealthEvent
		expected bool
	}{
		{
			name: "skipped XID is non-fatal",
			event: &DeviceHealthEvent{
				EventType: HealthEventXID,
				EventData: 13,
			},
			expected: true,
		},
		{
			name: "non-skipped XID is fatal",
			event: &DeviceHealthEvent{
				EventType: HealthEventXID,
				EventData: 48,
			},
			expected: false,
		},
		{
			name: "GPU_LOST is always fatal",
			event: &DeviceHealthEvent{
				EventType: HealthEventGPULost,
			},
			expected: false,
		},
		{
			name: "unmonitored is not an XID event",
			event: &DeviceHealthEvent{
				EventType: HealthEventUnmonitored,
			},
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, m.IsEventNonFatal(tc.event))
		})
	}
}

func TestResolveEventDeviceByPCIBusID(t *testing.T) {
	parent := &GpuInfo{UUID: "GPU-parent-1", minor: 0, pciBusID: "0000:01:00.0"}
	fullGPU := &AllocatableDevice{Gpu: parent}
	staticMIG := &AllocatableDevice{
		MigStatic: &MigDeviceInfo{
			parent: parent,
			gIInfo: &nvml.GpuInstanceInfo{Id: 2},
			cIInfo: &nvml.ComputeInstanceInfo{Id: 3},
		},
	}
	perGPU := &PerGPUAllocatableDevices{
		allocatablesMap: map[PCIBusID]AllocatableDevices{
			parent.pciBusID: {
				"gpu":     fullGPU,
				"static":  staticMIG,
				"unknown": {},
			},
		},
	}

	tests := []struct {
		name string
		gi   uint32
		ci   uint32
		want *AllocatableDevice
	}{
		{name: "full GPU", gi: FullGPUInstanceID, ci: FullGPUInstanceID, want: fullGPU},
		{name: "static MIG", gi: 2, ci: 3, want: staticMIG},
		{name: "unknown placement", gi: 7, ci: 0, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveEventDeviceByPCIBusID(perGPU, parent.UUID, parent.pciBusID, tc.gi, tc.ci)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestResolveDeviceForEventUsesGPUUUIDIndex(t *testing.T) {
	parent := &GpuInfo{UUID: "GPU-parent-1", minor: 0, pciBusID: "0000:01:00.0"}
	staticMIG := &AllocatableDevice{
		MigStatic: &MigDeviceInfo{
			parent: parent,
			gIInfo: &nvml.GpuInstanceInfo{Id: 2},
			cIInfo: &nvml.ComputeInstanceInfo{Id: 3},
		},
	}
	monitor := &nvmlDeviceHealthMonitor{
		perGPUAllocatable: &PerGPUAllocatableDevices{
			allocatablesMap: map[PCIBusID]AllocatableDevices{
				parent.pciBusID: {
					"static": staticMIG,
				},
			},
		},
		gpuInfosByUUID: map[string]*GpuInfo{parent.UUID: parent},
	}
	got, err := monitor.resolveDeviceForEvent(parent.UUID, 2, 3)
	require.NoError(t, err)
	require.Equal(t, staticMIG, got)

	_, err = monitor.resolveDeviceForEvent("GPU-unknown", 2, 3)
	require.ErrorContains(t, err, "not in the discovered GPU inventory")
}

func TestResolveEventDeviceByPCIBusIDRejectsWrongParent(t *testing.T) {
	parent := &GpuInfo{
		UUID:     "GPU-parent-1",
		pciBusID: "0000:01:00.0",
	}
	otherParent := &GpuInfo{
		UUID:     "GPU-parent-2",
		pciBusID: parent.pciBusID,
	}

	perGPU := &PerGPUAllocatableDevices{
		allocatablesMap: map[PCIBusID]AllocatableDevices{
			parent.pciBusID: {
				"wrong-gpu": {
					Gpu: otherParent,
				},
				"wrong-static-mig": {
					MigStatic: &MigDeviceInfo{
						parent: otherParent,
						gIInfo: &nvml.GpuInstanceInfo{Id: 2},
						cIInfo: &nvml.ComputeInstanceInfo{Id: 3},
					},
				},
			},
		},
	}

	got, err := resolveEventDeviceByPCIBusID(
		perGPU,
		parent.UUID,
		parent.pciBusID,
		FullGPUInstanceID,
		FullGPUInstanceID,
	)
	require.NoError(t, err)
	require.Nil(t, got)

	got, err = resolveEventDeviceByPCIBusID(
		perGPU,
		parent.UUID,
		parent.pciBusID,
		2,
		3,
	)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestHealthMonitorStartRequiresRegisteredEvents(t *testing.T) {
	m := &nvmlDeviceHealthMonitor{}
	require.ErrorContains(t, m.Start(context.Background()), "events have not been registered")
}
