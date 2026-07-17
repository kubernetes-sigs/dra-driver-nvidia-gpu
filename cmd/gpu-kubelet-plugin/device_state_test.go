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
	"testing"

	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

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
			name:                 "gpu overlap allowed when consumable shares enabled and matching configs",
			featureGate:          true,
			consumableSharesFlag: "unlimited",
			requestDevice:        "gpu-0",
			expectErr:            false,
		},
		{
			name:                 "vfio overlap rejected even when consumable shares enabled",
			featureGate:          true,
			consumableSharesFlag: "unlimited",
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

func TestSharingReferenceCountingHelpers(t *testing.T) {
	checkpoint := &Checkpoint{
		V2: &CheckpointV2{
			PreparedClaims: PreparedClaimsByUID{
				"claim-1": {
					CheckpointState: ClaimCheckpointStatePrepareCompleted,
					PreparedDevices: PreparedDevices{
						{
							Devices: PreparedDeviceList{
								{
									Gpu: &PreparedGpu{
										Info: &GpuInfo{UUID: "GPU-1111"},
										Device: &CheckpointedDevice{
											DeviceName: "gpu-0",
										},
									},
								},
								{
									Mig: &PreparedMigDevice{
										Concrete: &MigLiveTuple{MigUUID: "MIG-2222"},
										Device: &CheckpointedDevice{
											DeviceName: "mig-0",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// Active claim claim-1 uses GPU-1111 and mig-0 (MIG-2222).
	// Releasing claim-2 (a different claim) should detect that GPU-1111 and MIG-2222 are in use.
	require.True(t, isGpuUUIDInUseByOtherClaims(checkpoint, "claim-2", "GPU-1111"))
	require.False(t, isGpuUUIDInUseByOtherClaims(checkpoint, "claim-2", "GPU-9999"))

	// Releasing claim-1 should return false because claim-1 is being released.
	require.False(t, isGpuUUIDInUseByOtherClaims(checkpoint, "claim-1", "GPU-1111"))

	require.True(t, isMigDeviceInUseByOtherClaims(checkpoint, "claim-2", "MIG-2222", "mig-0"))
	require.False(t, isMigDeviceInUseByOtherClaims(checkpoint, "claim-2", "MIG-9999", "mig-9"))
	require.False(t, isMigDeviceInUseByOtherClaims(checkpoint, "claim-1", "MIG-2222", "mig-0"))
}

func TestIsMpsInUseByOtherClaims(t *testing.T) {
	checkpoint := &Checkpoint{
		V2: &CheckpointV2{
			PreparedClaims: PreparedClaimsByUID{
				"claim-1": {
					CheckpointState: ClaimCheckpointStatePrepareCompleted,
					PreparedDevices: PreparedDevices{
						{
							ConfigState: DeviceConfigState{
								MpsApplied: new(true),
							},
							Devices: PreparedDeviceList{
								{
									Gpu: &PreparedGpu{
										Info: &GpuInfo{UUID: "GPU-1111"},
									},
								},
							},
						},
					},
				},
				"claim-2": {
					CheckpointState: ClaimCheckpointStatePrepareCompleted,
					PreparedDevices: PreparedDevices{
						{
							ConfigState: DeviceConfigState{
								MpsControlDaemonID: "legacy-mps-id",
							},
							Devices: PreparedDeviceList{
								{
									Gpu: &PreparedGpu{
										Info: &GpuInfo{UUID: "GPU-2222"},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// For a new claim (claim-3), isMpsInUseByOtherClaims should return true since claim-1 and claim-2 use MPS.
	require.True(t, isMpsInUseByOtherClaims(checkpoint, "claim-3"))

	// For claim-1, isMpsInUseByOtherClaims should return true because claim-2 is still in completed state.
	require.True(t, isMpsInUseByOtherClaims(checkpoint, "claim-1"))

	// Delete claim-2; now for claim-1, other claims using MPS should be false.
	delete(checkpoint.V2.PreparedClaims, "claim-2")
	require.False(t, isMpsInUseByOtherClaims(checkpoint, "claim-1"))
}

func TestMpsCheckpointReconciliation(t *testing.T) {
	// A checkpoint with no claims
	var cpEmpty *Checkpoint
	require.False(t, isMpsInUseByOtherClaims(cpEmpty, ""))

	// A checkpoint with a claim in PrepareStarted state only
	cpStarted := &Checkpoint{
		V2: &CheckpointV2{
			PreparedClaims: PreparedClaimsByUID{
				"claim-started": {
					CheckpointState: ClaimCheckpointStatePrepareStarted,
					PreparedDevices: PreparedDevices{
						{
							ConfigState: DeviceConfigState{
								MpsApplied: new(true),
							},
						},
					},
				},
			},
		},
	}
	// isMpsInUseByOtherClaims ignores claims not in PrepareCompleted state
	require.False(t, isMpsInUseByOtherClaims(cpStarted, ""))

	// Once claim transitions to PrepareCompleted
	cpStarted.V2.PreparedClaims["claim-started"] = PreparedClaim{
		CheckpointState: ClaimCheckpointStatePrepareCompleted,
		PreparedDevices: PreparedDevices{
			{
				ConfigState: DeviceConfigState{
					MpsApplied: new(true),
				},
			},
		},
	}
	require.True(t, isMpsInUseByOtherClaims(cpStarted, ""))
}
