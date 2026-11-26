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
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/stretchr/testify/require"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

type clockTestNVML struct {
	nvml.Interface
	devices       map[string]*clockTestNVMLDevice
	lookupReturns map[string]nvml.Return
	lookupCalls   []string
}

func (m *clockTestNVML) InitWithFlags(uint32) nvml.Return { return nvml.SUCCESS }
func (m *clockTestNVML) Shutdown() nvml.Return            { return nvml.SUCCESS }

func (m *clockTestNVML) DeviceGetHandleByUUID(uuid string) (nvml.Device, nvml.Return) {
	m.lookupCalls = append(m.lookupCalls, uuid)
	if ret, exists := m.lookupReturns[uuid]; exists && ret != nvml.SUCCESS {
		return nil, ret
	}
	device, exists := m.devices[uuid]
	if !exists {
		return nil, nvml.ERROR_NOT_FOUND
	}
	return device, nvml.SUCCESS
}

type clockTestNVMLDevice struct {
	nvml.Device
	setReturn   nvml.Return
	resetReturn nvml.Return
	setCalls    [][2]uint32
	resetCalls  int
}

func (d *clockTestNVMLDevice) SetGpuLockedClocks(minMHz, maxMHz uint32) nvml.Return {
	d.setCalls = append(d.setCalls, [2]uint32{minMHz, maxMHz})
	return d.setReturn
}

func (d *clockTestNVMLDevice) ResetGpuLockedClocks() nvml.Return {
	d.resetCalls++
	return d.resetReturn
}

func TestClockManagerSetClockRollsBackEarlierGPUs(t *testing.T) {
	for _, tc := range []struct {
		name              string
		failLookup        bool
		rollbackFails     bool
		wantMessage       string
		wantRollbackError bool
	}{
		{name: "clock setting fails", wantMessage: "failed to set clock speed for device GPU-2"},
		{name: "later device lookup fails", failLookup: true, wantMessage: "couldn't get device for uuid GPU-2"},
		{name: "clock setting and rollback fail", rollbackFails: true, wantMessage: "failed to set clock speed for device GPU-2", wantRollbackError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := &clockTestNVMLDevice{}
			second := &clockTestNVMLDevice{}
			if tc.failLookup {
				second = nil
			} else {
				second.setReturn = nvml.ERROR_UNKNOWN
			}
			if tc.rollbackFails {
				first.resetReturn = nvml.ERROR_UNKNOWN
			}
			lib := &clockTestNVML{
				devices: map[string]*clockTestNVMLDevice{"GPU-1": first},
			}
			if second != nil {
				lib.devices["GPU-2"] = second
			}
			if tc.failLookup {
				lib.lookupReturns = map[string]nvml.Return{"GPU-2": nvml.ERROR_NOT_FOUND}
			}

			manager := NewClockManager(&deviceLib{nvmllib: lib})
			clockMHz := int32(1200)
			err := manager.SetClock([]string{"GPU-1", "GPU-2"}, configapi.GpuClocks{SMLockedMHz: &clockMHz})

			require.ErrorContains(t, err, tc.wantMessage)
			require.Equal(t, [][2]uint32{{1200, 1200}}, first.setCalls)
			require.Equal(t, 1, first.resetCalls)
			require.Equal(t, []string{"GPU-1", "GPU-2", "GPU-1"}, lib.lookupCalls)
			if tc.wantRollbackError {
				require.ErrorContains(t, err, "rollback also failed")
				require.ErrorContains(t, err, "failed to reset locked GPU clocks for device UUID GPU-1")
			}
		})
	}
}

func TestClockManagerResetGpuClockSpeedContinuesAfterFailures(t *testing.T) {
	first := &clockTestNVMLDevice{resetReturn: nvml.ERROR_UNKNOWN}
	third := &clockTestNVMLDevice{}
	lib := &clockTestNVML{
		devices: map[string]*clockTestNVMLDevice{
			"GPU-1": first,
			"GPU-3": third,
		},
		lookupReturns: map[string]nvml.Return{"GPU-2": nvml.ERROR_NOT_FOUND},
	}

	err := NewClockManager(&deviceLib{nvmllib: lib}).ResetGpuClockSpeed([]string{"GPU-1", "GPU-2", "GPU-3"})

	require.Error(t, err)
	require.ErrorContains(t, err, "failed to reset locked GPU clocks for device UUID GPU-1")
	require.ErrorContains(t, err, "failed to get NVML device handle for UUID GPU-2")
	require.Equal(t, 1, first.resetCalls)
	require.Equal(t, 1, third.resetCalls)
	require.Equal(t, []string{"GPU-1", "GPU-2", "GPU-3"}, lib.lookupCalls)
}

func TestClockManagerSetClockRejectsMissingClock(t *testing.T) {
	err := NewClockManager(&deviceLib{}).SetClock([]string{"GPU-1"}, configapi.GpuClocks{})
	require.ErrorContains(t, err, "SMLockedMHz configuration is missing")
}

func TestClockManagerResetErrorIsJoined(t *testing.T) {
	first := &clockTestNVMLDevice{resetReturn: nvml.ERROR_UNKNOWN}
	second := &clockTestNVMLDevice{resetReturn: nvml.ERROR_UNKNOWN}
	lib := &clockTestNVML{devices: map[string]*clockTestNVMLDevice{"GPU-1": first, "GPU-2": second}}

	err := NewClockManager(&deviceLib{nvmllib: lib}).ResetGpuClockSpeed([]string{"GPU-1", "GPU-2"})

	require.Error(t, err)
	require.ErrorContains(t, err, "GPU-1")
	require.ErrorContains(t, err, "GPU-2")
}
