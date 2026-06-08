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
	"testing"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// setPassthrough toggles the PassthroughSupport feature gate on the global featuregates.
func setPassthrough(t *testing.T, enabled bool) {
	t.Helper()
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.PassthroughSupport): enabled,
	}))
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
			string(featuregates.PassthroughSupport): false,
		}))
	})
}

// fakeEnumerator implements deviceEnumerator for tests.
// Each call to enumerateAllPossibleDevices consumes the next result in the slice,
// once exhausted, the last result is repeated.
type fakeEnumerator struct {
	results []enumerateResult
	calls   int
}

type enumerateResult struct {
	devices *PerGPUAllocatableDevices
	err     error
}

func (f *fakeEnumerator) enumerateAllPossibleDevices() (*PerGPUAllocatableDevices, error) {
	idx := f.calls
	if idx >= len(f.results) {
		idx = len(f.results) - 1
	}
	f.calls++
	return f.results[idx].devices, f.results[idx].err
}

func emptyDevices() *PerGPUAllocatableDevices {
	return &PerGPUAllocatableDevices{allocatablesMap: map[PCIBusID]AllocatableDevices{}}
}

func oneGPUDevice() *PerGPUAllocatableDevices {
	return &PerGPUAllocatableDevices{
		allocatablesMap: map[PCIBusID]AllocatableDevices{
			"0000:00:04.0": {"gpu-0": {Gpu: &GpuInfo{UUID: "GPU-fake-uuid"}}},
		},
	}
}

// vfioOnlyDevices returns a PerGPUAllocatableDevices with a single vfio-type device.
func vfioOnlyDevices() *PerGPUAllocatableDevices {
	return &PerGPUAllocatableDevices{
		allocatablesMap: map[PCIBusID]AllocatableDevices{
			"0000:00:04.0": {
				"gpu-vfio-0": {Vfio: &VfioDeviceInfo{
					UUID:     "vfio-fake-uuid",
					PciBusID: "0000:00:04.0",
				}},
			},
		},
	}
}

// mixedDevices returns a PerGPUAllocatableDevices with two GPUs:
//   - GPU at 0000:00:04.0: a vfio-type device
//   - GPU at 0000:00:05.0: a gpu-type device
func mixedDevices() *PerGPUAllocatableDevices {
	return &PerGPUAllocatableDevices{
		allocatablesMap: map[PCIBusID]AllocatableDevices{
			"0000:00:04.0": {
				"gpu-vfio-0": {Vfio: &VfioDeviceInfo{
					UUID:     "vfio-fake-uuid",
					PciBusID: "0000:00:04.0",
				}},
			},
			"0000:00:05.0": {"gpu-1": {Gpu: &GpuInfo{UUID: "GPU-fake-uuid-2"}}},
		},
	}
}

// nonEmptyCheckpoint builds a Checkpoint with one prepared device.
func nonEmptyCheckpoint() *Checkpoint {
	return &Checkpoint{
		V2: &CheckpointV2{
			PreparedClaims: PreparedClaimsByUID{
				"claim-uid-1": {
					CheckpointState: ClaimCheckpointStatePrepareCompleted,
					PreparedDevices: PreparedDevices{
						{Devices: PreparedDeviceList{
							{Gpu: &PreparedGpu{
								Info:   &GpuInfo{UUID: "GPU-fake-uuid"},
								Device: &CheckpointedDevice{DeviceName: "gpu-0"},
							}},
						}},
					},
				},
			},
		},
	}
}

func TestEnumerateDevicesWithRetry(t *testing.T) {
	// Cases that exercise the vfio-only branch must enable PassthroughSupport, which
	// manipulates the global featuregates - subtests cannot run in parallel.

	// fastBackoff is a zero-jitter, millisecond-interval backoff used across
	// test cases so retries are deterministic and tests finish quickly.
	fastBackoff := func(steps int) wait.Backoff {
		return wait.Backoff{
			Duration: 1 * time.Millisecond,
			Factor:   1.0,
			Jitter:   0.0,
			Steps:    steps,
		}
	}

	tests := map[string]struct {
		enumerator         *fakeEnumerator
		checkpoint         *Checkpoint
		ctxFn              func() (context.Context, context.CancelFunc)
		backoff            wait.Backoff
		passthroughEnabled bool
		wantErr            error
		wantDeviceCount    int
		wantCalls          int
	}{
		"devices found on first call": {
			enumerator:      &fakeEnumerator{results: []enumerateResult{{devices: oneGPUDevice()}}},
			backoff:         fastBackoff(10),
			wantDeviceCount: 1,
			wantCalls:       1,
		},
		"devices found after retries": {
			enumerator: &fakeEnumerator{results: []enumerateResult{
				{devices: emptyDevices()},
				{devices: emptyDevices()},
				{devices: oneGPUDevice()},
			}},
			backoff:         fastBackoff(10),
			wantDeviceCount: 1,
			wantCalls:       3,
		},
		// A pre-cancelled context short-circuits ExponentialBackoffWithContext before the condition
		// runs, so the enumerator is never invoked regardless of the backoff budget.
		"context cancelled returns context error": {
			enumerator: &fakeEnumerator{results: []enumerateResult{{devices: emptyDevices()}}},
			ctxFn: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			backoff:   fastBackoff(10),
			wantErr:   context.Canceled,
			wantCalls: 0,
		},
		"retry budget exhausted returns sentinel error to force crashloop": {
			enumerator: &fakeEnumerator{results: []enumerateResult{{devices: emptyDevices()}}},
			backoff:    fastBackoff(3),
			wantErr:    ErrDeviceEnumerationTimeout,
			wantCalls:  3,
		},
		"transient NVML error retried and then succeeds": {
			enumerator: &fakeEnumerator{results: []enumerateResult{
				{err: fmt.Errorf("ensureNVML failed: %w", nvml.ERROR_UNINITIALIZED)},
				{err: fmt.Errorf("ensureNVML failed: %w", nvml.ERROR_DRIVER_NOT_LOADED)},
				{devices: oneGPUDevice()},
			}},
			backoff:         fastBackoff(10),
			wantDeviceCount: 1,
			wantCalls:       3,
		},
		"non-transient NVML error fails immediately": {
			enumerator: &fakeEnumerator{results: []enumerateResult{
				{err: fmt.Errorf("ensureNVML failed: %w", nvml.ERROR_GPU_IS_LOST)},
			}},
			backoff:   fastBackoff(10),
			wantErr:   nvml.ERROR_GPU_IS_LOST,
			wantCalls: 1,
		},
		"empty then non-transient error propagates mid-retry": {
			enumerator: &fakeEnumerator{results: []enumerateResult{
				{devices: emptyDevices()},
				{err: fmt.Errorf("ensureNVML failed: %w", nvml.ERROR_GPU_IS_LOST)},
			}},
			backoff:   fastBackoff(10),
			wantErr:   nvml.ERROR_GPU_IS_LOST,
			wantCalls: 2,
		},
		"passthrough on: vfio-only with empty checkpoint - retry": {
			enumerator: &fakeEnumerator{results: []enumerateResult{
				{devices: vfioOnlyDevices()},
				{devices: vfioOnlyDevices()},
				{devices: oneGPUDevice()},
			}},
			passthroughEnabled: true,
			backoff:            fastBackoff(10),
			wantDeviceCount:    1,
			wantCalls:          3,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			setPassthrough(t, tc.passthroughEnabled)

			ctx := context.Background()
			if tc.ctxFn != nil {
				var cancel context.CancelFunc
				ctx, cancel = tc.ctxFn()
				defer cancel()
			}

			cp := tc.checkpoint
			if cp == nil {
				cp = &Checkpoint{V2: &CheckpointV2{PreparedClaims: PreparedClaimsByUID{}}}
			}
			got, err := enumerateDevicesWithRetry(ctx, tc.enumerator, tc.backoff, cp)

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				assert.Len(t, got.allocatablesMap, tc.wantDeviceCount)
			}
			assert.Equal(t, tc.wantCalls, tc.enumerator.calls, "enumerator call count")
		})
	}
}

func TestAllocatableHasNonVfio(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		perGPU *PerGPUAllocatableDevices
		want   bool
	}{
		"empty allocatable":  {perGPU: emptyDevices(), want: false},
		"gpu-only":           {perGPU: oneGPUDevice(), want: true},
		"vfio-only":          {perGPU: vfioOnlyDevices(), want: false},
		"mixed gpu and vfio": {perGPU: mixedDevices(), want: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, allocatableHasNonVfio(tc.perGPU))
		})
	}
}

func TestCheckpointHasPreparedDevices(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		checkpoint *Checkpoint
		want       bool
	}{
		"nil checkpoint":       {checkpoint: nil, want: false},
		"nil V2":               {checkpoint: &Checkpoint{}, want: false},
		"empty PreparedClaims": {checkpoint: &Checkpoint{V2: &CheckpointV2{PreparedClaims: PreparedClaimsByUID{}}}, want: false},
		"non-empty checkpoint": {checkpoint: nonEmptyCheckpoint(), want: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, checkpointHasPreparedDevices(tc.checkpoint))
		})
	}
}

func TestEnumerateDevices(t *testing.T) {
	// Cases that exercise the vfio-only branch must enable PassthroughSupport, which
	// manipulates the global featuregates singleton — subtests cannot run in parallel.

	emptyCheckpoint := &Checkpoint{V2: &CheckpointV2{PreparedClaims: PreparedClaimsByUID{}}}

	tests := map[string]struct {
		enumerator         *fakeEnumerator
		checkpoint         *Checkpoint
		passthroughEnabled bool
		wantNil            bool
		wantErr            error
		wantDeviceCount    int
	}{
		"gpu device discovered — accept": {
			enumerator:      &fakeEnumerator{results: []enumerateResult{{devices: oneGPUDevice()}}},
			wantDeviceCount: 1,
		},
		"empty allocatable — retry": {
			enumerator: &fakeEnumerator{results: []enumerateResult{{devices: emptyDevices()}}},
			wantNil:    true,
		},
		"transient NVML error — retry": {
			enumerator: &fakeEnumerator{results: []enumerateResult{
				{err: fmt.Errorf("ensureNVML failed: %w", nvml.ERROR_UNINITIALIZED)},
			}},
			wantNil: true,
		},
		"non-transient NVML error — propagates immediately": {
			enumerator: &fakeEnumerator{results: []enumerateResult{
				{err: fmt.Errorf("ensureNVML failed: %w", nvml.ERROR_GPU_IS_LOST)},
			}},
			wantErr: nvml.ERROR_GPU_IS_LOST,
		},
		"passthrough on: vfio-only allocatable, empty checkpoint — retry": {
			enumerator:         &fakeEnumerator{results: []enumerateResult{{devices: vfioOnlyDevices()}}},
			checkpoint:         emptyCheckpoint,
			passthroughEnabled: true,
			wantNil:            true,
		},
		"passthrough on: vfio-only allocatable, non-empty checkpoint — accept": {
			enumerator:         &fakeEnumerator{results: []enumerateResult{{devices: vfioOnlyDevices()}}},
			checkpoint:         nonEmptyCheckpoint(),
			passthroughEnabled: true,
			wantDeviceCount:    1,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			setPassthrough(t, tc.passthroughEnabled)

			got, err := enumerateDevices(tc.enumerator, tc.checkpoint)

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			if tc.wantNil {
				assert.Nil(t, got)
			} else {
				require.NotNil(t, got)
				assert.Len(t, got.allocatablesMap, tc.wantDeviceCount)
			}
		})
	}
}

func TestIsTransientNVMLError(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err         error
		isTransient bool
	}{
		"bare nvml.ERROR_UNINITIALIZED": {
			err:         nvml.ERROR_UNINITIALIZED,
			isTransient: true,
		},
		"bare nvml.ERROR_DRIVER_NOT_LOADED": {
			err:         nvml.ERROR_DRIVER_NOT_LOADED,
			isTransient: true,
		},
		"bare nvml.ERROR_GPU_IS_LOST is not transient": {
			err:         nvml.ERROR_GPU_IS_LOST,
			isTransient: false,
		},
		"single-wrap as ensureNVML returns it": {
			err:         fmt.Errorf("ensureNVML failed: %w", nvml.ERROR_UNINITIALIZED),
			isTransient: true,
		},
		"double-wrap with nvml.ERROR_UNINITIALIZED": {
			err: fmt.Errorf("error enumerating allocatable devices: %w",
				fmt.Errorf("ensureNVML failed: %w", nvml.ERROR_UNINITIALIZED)),
			isTransient: true,
		},
		"double-wrap with ERROR_DRIVER_NOT_LOADED": {
			err: fmt.Errorf("error enumerating allocatable devices: %w",
				fmt.Errorf("ensureNVML failed: %w", nvml.ERROR_DRIVER_NOT_LOADED)),
			isTransient: true,
		},
		"double-wrap with non-transient ERROR_GPU_IS_LOST": {
			err: fmt.Errorf("error enumerating allocatable devices: %w",
				fmt.Errorf("ensureNVML failed: %w", nvml.ERROR_GPU_IS_LOST)),
			isTransient: false,
		},
		"plain non-NVML error": {
			err:         fmt.Errorf("some other failure"),
			isTransient: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.isTransient, isTransientNVMLError(tc.err))
		})
	}
}
