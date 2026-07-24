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

import (
	"strconv"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func isConsumableSharesEnabled(config *Config) bool {
	if !featuregates.Enabled(featuregates.ConsumableShares) {
		return false
	}
	if config == nil || config.flags == nil {
		return false
	}
	sharesOption := strings.TrimSpace(config.flags.consumableShares)
	return sharesOption != "" && sharesOption != "disabled"
}

func applyConsumableShares(dev *resourceapi.Device, config *Config) {
	if !isConsumableSharesEnabled(config) {
		return
	}
	sharesOption := strings.TrimSpace(config.flags.consumableShares)

	dev.AllowMultipleAllocations = new(true)

	if dev.Capacity == nil {
		dev.Capacity = make(map[resourceapi.QualifiedName]resourceapi.DeviceCapacity)
	}

	memCap, hasMemory := dev.Capacity["memory"]
	if !hasMemory {
		return
	}

	// Kubernetes CapacityRequestPolicyRange requires that Max (if set) and Default
	// be exact multiples of Step. We use 1Mi for fine-grained allocation granularity
	// and round down max memory to the nearest multiple of Step to ensure ResourceSlice
	// objects pass API server validation if raw device memory is not step-aligned.
	zero := resource.MustParse("0")
	step := resource.MustParse("1Mi")
	stepBytes := step.Value()
	maxBytes := (memCap.Value.Value() / stepBytes) * stepBytes
	maxMem := resource.NewQuantity(maxBytes, resource.BinarySI)
	memCap.Value = maxMem.DeepCopy()

	switch sharesOption {
	case "unlimited":
		memCap.RequestPolicy = &resourceapi.CapacityRequestPolicy{
			Default: new(zero.DeepCopy()),
			ValidRange: &resourceapi.CapacityRequestPolicyRange{
				Min:  new(zero.DeepCopy()),
				Max:  new(maxMem.DeepCopy()),
				Step: new(step.DeepCopy()),
			},
		}
		dev.Capacity["memory"] = memCap

	case "memory":
		memCap.RequestPolicy = &resourceapi.CapacityRequestPolicy{
			Default: new(maxMem.DeepCopy()),
			ValidRange: &resourceapi.CapacityRequestPolicyRange{
				Min:  new(step.DeepCopy()),
				Max:  new(maxMem.DeepCopy()),
				Step: new(step.DeepCopy()),
			},
		}
		dev.Capacity["memory"] = memCap

	default:
		val, err := strconv.Atoi(sharesOption)
		if err == nil && val > 0 {
			memCap.RequestPolicy = &resourceapi.CapacityRequestPolicy{
				Default: new(zero.DeepCopy()),
				ValidRange: &resourceapi.CapacityRequestPolicyRange{
					Min:  new(zero.DeepCopy()),
					Max:  new(maxMem.DeepCopy()),
					Step: new(step.DeepCopy()),
				},
			}
			dev.Capacity["memory"] = memCap

			sharesVal := *resource.NewQuantity(int64(val), resource.DecimalSI)
			oneVal := *resource.NewQuantity(1, resource.DecimalSI)
			dev.Capacity["shares"] = resourceapi.DeviceCapacity{
				Value: sharesVal,
				RequestPolicy: &resourceapi.CapacityRequestPolicy{
					Default: new(oneVal.DeepCopy()),
					ValidRange: &resourceapi.CapacityRequestPolicyRange{
						Min:  new(oneVal.DeepCopy()),
						Max:  new(sharesVal.DeepCopy()),
						Step: new(oneVal.DeepCopy()),
					},
				},
			}
		}
	}
}
