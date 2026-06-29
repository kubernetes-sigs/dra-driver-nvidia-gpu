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

	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func TestApplyConsumableShares(t *testing.T) {
	memVal := resource.MustParse("16Gi")

	tests := []struct {
		name                 string
		featureGate          bool
		consumableSharesFlag string
		expectModified       bool
		expectedPolicyType   string
		expectedSharesVal    int64
	}{
		{
			name:                 "feature gate disabled",
			featureGate:          false,
			consumableSharesFlag: "memory",
			expectModified:       false,
		},
		{
			name:                 "flag disabled",
			featureGate:          true,
			consumableSharesFlag: "disabled",
			expectModified:       false,
		},
		{
			name:                 "memory option",
			featureGate:          true,
			consumableSharesFlag: "memory",
			expectModified:       true,
			expectedPolicyType:   "memory",
		},
		{
			name:                 "unlimited option",
			featureGate:          true,
			consumableSharesFlag: "unlimited",
			expectModified:       true,
			expectedPolicyType:   "unlimited",
		},
		{
			name:                 "integer option",
			featureGate:          true,
			consumableSharesFlag: "10",
			expectModified:       true,
			expectedPolicyType:   "integer",
			expectedSharesVal:    10,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
				string(featuregates.ConsumableShares): tc.featureGate,
			}))

			config := &Config{
				flags: &Flags{
					consumableShares: tc.consumableSharesFlag,
				},
			}

			dev := resourceapi.Device{
				Name: "gpu-0",
				Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
					"memory": {
						Value: memVal,
					},
				},
			}

			applyConsumableShares(&dev, config)

			if !tc.expectModified {
				require.Nil(t, dev.AllowMultipleAllocations)
				require.Nil(t, dev.Capacity["memory"].RequestPolicy)
				_, hasShares := dev.Capacity["shares"]
				require.False(t, hasShares)
				return
			}

			require.NotNil(t, dev.AllowMultipleAllocations)
			require.True(t, *dev.AllowMultipleAllocations)

			memCap := dev.Capacity["memory"]
			require.NotNil(t, memCap.RequestPolicy)
			require.NotNil(t, memCap.RequestPolicy.Default)
			require.NotNil(t, memCap.RequestPolicy.ValidRange)
			require.Equal(t, resource.MustParse("1Gi"), *memCap.RequestPolicy.ValidRange.Step)
			require.Equal(t, memVal, *memCap.RequestPolicy.ValidRange.Max)

			switch tc.expectedPolicyType {
			case "memory":
				require.Equal(t, memVal, *memCap.RequestPolicy.Default)
				require.Equal(t, resource.MustParse("1Gi"), *memCap.RequestPolicy.ValidRange.Min)
			case "unlimited", "integer":
				require.Equal(t, resource.MustParse("0"), *memCap.RequestPolicy.Default)
				require.Equal(t, resource.MustParse("0"), *memCap.RequestPolicy.ValidRange.Min)
			}

			sharesCap, hasShares := dev.Capacity["shares"]
			if tc.expectedPolicyType == "integer" {
				require.True(t, hasShares)
				require.Equal(t, *resource.NewQuantity(tc.expectedSharesVal, resource.BinarySI), sharesCap.Value)
				require.NotNil(t, sharesCap.RequestPolicy)
				require.Equal(t, *resource.NewQuantity(1, resource.BinarySI), *sharesCap.RequestPolicy.Default)
				require.Equal(t, *resource.NewQuantity(1, resource.BinarySI), *sharesCap.RequestPolicy.ValidRange.Min)
				require.Equal(t, *resource.NewQuantity(tc.expectedSharesVal, resource.BinarySI), *sharesCap.RequestPolicy.ValidRange.Max)
				require.Equal(t, *resource.NewQuantity(1, resource.BinarySI), *sharesCap.RequestPolicy.ValidRange.Step)
			} else {
				require.False(t, hasShares)
			}
		})
	}
}

func TestValidateNoOverlappingPreparedDevices(t *testing.T) {
	perGPU := &PerGPUAllocatableDevices{
		allocatablesMap: map[PCIBusID]AllocatableDevices{
			"0000:00:00.0": {
				"gpu-0":  &AllocatableDevice{Gpu: &GpuInfo{minor: 0}},
				"vfio-0": &AllocatableDevice{Vfio: &VfioDeviceInfo{index: 0}},
			},
		},
	}

	checkpoint := &Checkpoint{
		V2: &CheckpointV2{
			PreparedClaims: PreparedClaimsByUID{
				"claim-1": {
					CheckpointState: ClaimCheckpointStatePrepareCompleted,
					Status: resourceapi.ResourceClaimStatus{
						Allocation: &resourceapi.AllocationResult{
							Devices: resourceapi.DeviceAllocationResult{
								Results: []resourceapi.DeviceRequestAllocationResult{
									{Driver: DriverName, Device: "gpu-0"},
									{Driver: DriverName, Device: "vfio-0"},
								},
							},
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name                 string
		featureGate          bool
		consumableSharesFlag string
		requestDevice        string
		expectErr            bool
	}{
		{
			name:                 "gpu overlap rejected when consumable shares disabled",
			featureGate:          false,
			consumableSharesFlag: "disabled",
			requestDevice:        "gpu-0",
			expectErr:            true,
		},
		{
			name:                 "gpu overlap allowed when consumable shares enabled",
			featureGate:          true,
			consumableSharesFlag: "memory",
			requestDevice:        "gpu-0",
			expectErr:            false,
		},
		{
			name:                 "vfio overlap rejected even when consumable shares enabled",
			featureGate:          true,
			consumableSharesFlag: "memory",
			requestDevice:        "vfio-0",
			expectErr:            true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
				string(featuregates.ConsumableShares): tc.featureGate,
			}))

			state := &DeviceState{
				config: &Config{
					flags: &Flags{
						consumableShares: tc.consumableSharesFlag,
					},
				},
				perGPUAllocatable: perGPU,
			}

			incomingClaim := &resourceapi.ResourceClaim{
				Status: resourceapi.ResourceClaimStatus{
					Allocation: &resourceapi.AllocationResult{
						Devices: resourceapi.DeviceAllocationResult{
							Results: []resourceapi.DeviceRequestAllocationResult{
								{Driver: DriverName, Device: tc.requestDevice},
							},
						},
					},
				},
			}

			err := state.validateNoOverlappingPreparedDevices(checkpoint, incomingClaim)
			if tc.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

