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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeEnumerator implements deviceEnumerator for tests. Each call to
// enumerateAllPossibleDevices consumes the next result in the slice; once
// exhausted, the last result is repeated.
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

func oneDevice() *PerGPUAllocatableDevices {
	return &PerGPUAllocatableDevices{
		allocatablesMap: map[PCIBusID]AllocatableDevices{
			"0000:00:04.0": {"gpu-0": {Gpu: &GpuInfo{UUID: "GPU-fake-uuid"}}},
		},
	}
}

func TestEnumerateDevicesWithRetry(t *testing.T) {
	tests := map[string]struct {
		enumerator      *fakeEnumerator
		ctxFn           func() (context.Context, context.CancelFunc)
		timeout         time.Duration
		interval        time.Duration
		wantErr         error
		wantDeviceCount int
		wantMinCalls    int
	}{
		"devices found on first call": {
			enumerator:      &fakeEnumerator{results: []enumerateResult{{devices: oneDevice()}}},
			wantDeviceCount: 1,
			wantMinCalls:    1,
		},
		"devices found after retries": {
			enumerator: &fakeEnumerator{results: []enumerateResult{
				{devices: emptyDevices()},
				{devices: emptyDevices()},
				{devices: oneDevice()},
			}},
			interval:        1 * time.Millisecond,
			wantDeviceCount: 1,
			wantMinCalls:    3,
		},
		"error propagated immediately without retry": {
			enumerator:   &fakeEnumerator{results: []enumerateResult{{err: errors.New("nvml failed")}}},
			wantErr:      errors.New("nvml failed"),
			wantMinCalls: 1,
		},
		"context cancelled returns context error": {
			enumerator: &fakeEnumerator{results: []enumerateResult{{devices: emptyDevices()}}},
			ctxFn: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			interval: 1 * time.Millisecond,
			wantErr:  context.Canceled,
		},
		"timeout returns empty device set without error": {
			enumerator:      &fakeEnumerator{results: []enumerateResult{{devices: emptyDevices()}}},
			timeout:         1 * time.Millisecond,
			interval:        1 * time.Millisecond,
			wantDeviceCount: 0,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// Override package-level timing variables for fast tests.
			if tc.interval != 0 {
				old := deviceEnumerationInterval
				deviceEnumerationInterval = tc.interval
				defer func() { deviceEnumerationInterval = old }()
			}
			if tc.timeout != 0 {
				old := deviceEnumerationTimeout
				deviceEnumerationTimeout = tc.timeout
				defer func() { deviceEnumerationTimeout = old }()
			}

			ctx := context.Background()
			if tc.ctxFn != nil {
				var cancel context.CancelFunc
				ctx, cancel = tc.ctxFn()
				defer cancel()
			}

			got, err := enumerateDevicesWithRetry(ctx, tc.enumerator)

			if tc.wantErr != nil {
				require.ErrorContains(t, err, tc.wantErr.Error())
				return
			}
			require.NoError(t, err)
			assert.Len(t, got.allocatablesMap, tc.wantDeviceCount)
			assert.GreaterOrEqual(t, tc.enumerator.calls, tc.wantMinCalls)
		})
	}
}
