//go:build ignore

/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package engine

import "github.com/NVIDIA/go-nvml/pkg/nvml"

// ClaimHealthControlEvent returns the next XID from the health-control file.
// GI and CI are whatever the file requested, including MIG instance IDs.
func (e *Engine) ClaimHealthControlEvent() (uintptr, uint64, uint32, uint32, bool) {
	event, ok := DefaultHealthStore().Claim()
	if !ok {
		return 0, 0, 0, 0, false
	}
	handle, ret := e.DeviceGetHandleByIndex(event.GPU)
	if ret != nvml.SUCCESS || handle == 0 {
		debugLog("[ENGINE] health-control XID %d for GPU %d dropped: %v\n", event.XID, event.GPU, ret)
		return 0, 0, 0, 0, false
	}
	return handle, event.XID, event.GPUInstanceID, event.ComputeInstanceID, true
}

// HealthRecoveryAction returns NVML_FI_DEV_GET_GPU_RECOVERY_ACTION for the GPU
// behind handle. An unknown handle or a GPU omitted from the control file is
// GPU_RECOVERY_ACTION_NONE.
func (e *Engine) HealthRecoveryAction(handle uintptr) uint32 {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if e.initCount == 0 || e.server == nil {
		return RecoveryActionNone
	}
	device := e.handles.Lookup(handle)
	configured, ok := device.(*ConfigurableDevice)
	if !ok || configured == nil {
		return RecoveryActionNone
	}
	for i := range e.server.configurableDevices {
		if e.server.configurableDevices[i] == configured {
			return DefaultHealthStore().RecoveryAction(i)
		}
	}
	return RecoveryActionNone
}
