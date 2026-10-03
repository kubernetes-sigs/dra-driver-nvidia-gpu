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

package main

/*
#include "nvml_types.h"

#define MOCK_NVML_VALUE_TYPE_UNSIGNED_INT 1

static unsigned int healthFieldID(nvmlFieldValue_t *fv) { return fv->fieldId; }

static void healthSetTimestamp(nvmlFieldValue_t *fv, long long ts) { fv->timestamp = ts; }

static void healthSetReturn(nvmlFieldValue_t *fv, nvmlReturn_t ret) { fv->nvmlReturn = ret; }

static void healthSetRecovery(nvmlFieldValue_t *fv, unsigned int action) {
	fv->valueType = MOCK_NVML_VALUE_TYPE_UNSIGNED_INT;
	fv->nvmlReturn = NVML_SUCCESS;
	fv->value.uiVal = action;
}
*/
import "C"
import (
	"time"
	"unsafe"

	"github.com/NVIDIA/k8s-test-infra/pkg/gpu/mocknvml/engine"
)

// healthGPURecoveryActionField is NVML_FI_DEV_GET_GPU_RECOVERY_ACTION.
const healthGPURecoveryActionField = 230

//export nvmlDeviceGetFieldValues
func nvmlDeviceGetFieldValues(device C.nvmlDevice_t, valuesCount C.int, values *C.nvmlFieldValue_t) C.nvmlReturn_t {
	if values == nil || valuesCount <= 0 {
		return C.NVML_ERROR_INVALID_ARGUMENT
	}
	fields := unsafe.Slice(values, int(valuesCount))
	sawRecovery := false
	for i := range fields {
		if uint32(C.healthFieldID(&fields[i])) == healthGPURecoveryActionField {
			sawRecovery = true
			break
		}
	}
	if !sawRecovery {
		// Callers that never ask for the recovery action keep the historical
		// not-supported stub, so nvidia-smi field queries stay unchanged.
		return stubReturn("nvmlDeviceGetFieldValues")
	}
	if device.handle == nil {
		return C.NVML_ERROR_INVALID_ARGUMENT
	}

	action := engine.GetEngine().HealthRecoveryAction(uintptr(unsafe.Pointer(device.handle)))
	ts := C.longlong(time.Now().UnixMicro())
	for i := range fields {
		field := &fields[i]
		C.healthSetTimestamp(field, ts)
		if uint32(C.healthFieldID(field)) != healthGPURecoveryActionField {
			C.healthSetReturn(field, C.NVML_ERROR_NOT_SUPPORTED)
			continue
		}
		C.healthSetRecovery(field, C.uint(action))
	}
	return C.NVML_SUCCESS
}
