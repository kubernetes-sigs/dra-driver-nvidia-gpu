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

import (
	"unsafe"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// healthGPURecoveryActionField is NVML_FI_DEV_GET_GPU_RECOVERY_ACTION.
const healthGPURecoveryActionField uint32 = 230

// ClaimHealthControlEvent returns the next XID from the health-control file.
// GI and CI are whatever the file requested, including MIG instance IDs.
// The handle is the same unsafe.Pointer DeviceGetHandleByIndex returns.
func (e *Engine) ClaimHealthControlEvent() (unsafe.Pointer, uint64, uint32, uint32, bool) {
	event, ok := DefaultHealthStore().Claim()
	if !ok {
		return nil, 0, 0, 0, false
	}
	handle, ret := e.DeviceGetHandleByIndex(event.GPU)
	if ret != nvml.SUCCESS || handle == nil {
		debugLog("[ENGINE] health-control XID %d for GPU %d dropped: %v\n", event.XID, event.GPU, ret)
		return nil, 0, 0, 0, false
	}
	return handle, event.XID, event.GPUInstanceID, event.ComputeInstanceID, true
}

// healthRecoveryField reports NVML_FI_DEV_GET_GPU_RECOVERY_ACTION for this GPU.
// A GPU the control file does not mention stays at GPU_RECOVERY_ACTION_NONE.
func (d *ConfigurableDevice) healthRecoveryField(fieldID uint32) (uint32, bool) {
	if fieldID != healthGPURecoveryActionField {
		return 0, false
	}
	return DefaultHealthStore().RecoveryAction(d.index), true
}
