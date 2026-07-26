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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// TestCheckHostIMEXReadyMissingBinary confirms that a driver root without
// nvidia-imex-ctl (e.g. a node where the optional nvidia-imex package was
// never installed) fails with an actionable error, without ever attempting
// to chroot/exec. The success path (chrooting in and parsing "READY") needs
// a real nvidia-imex-ctl binary and elevated privileges, so it is not
// covered by this unit test.
func TestCheckHostIMEXReadyMissingBinary(t *testing.T) {
	l := deviceLib{devRoot: t.TempDir()}

	err := l.checkHostIMEXReady(defaultIMEXHostSocketPath)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "nvidia-imex-ctl not found")
}

// TestEvaluateGpuFabricInfo covers the per-device fabric registration
// decision used by getCliqueIDStrict. Transitional registration states
// (NOT_STARTED, IN_PROGRESS) must fall back to "no clique" instead of
// returning an error: a GPU re-bound to the driver after Fabric Manager's
// initial registration sweep (e.g. post fell-off-the-bus remediation), or a
// GPU outside any activated partition in shared NVSwitch mode, legitimately
// reports these states, and an error here crashes the plugin on startup
// (issue #1264).
func TestEvaluateGpuFabricInfo(t *testing.T) {
	nonZeroUUID := [16]uint8{0xde, 0xad, 0xbe, 0xef, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}

	testCases := []struct {
		name      string
		info      nvml.GpuFabricInfo
		expectUse bool
		expectErr string
	}{
		{
			name: "fabric not supported by device",
			info: nvml.GpuFabricInfo{
				State: nvml.GPU_FABRIC_STATE_NOT_SUPPORTED,
			},
			expectUse: false,
		},
		{
			name: "registration not started is transitional, not fatal",
			info: nvml.GpuFabricInfo{
				State: nvml.GPU_FABRIC_STATE_NOT_STARTED,
			},
			expectUse: false,
		},
		{
			name: "registration in progress is transitional, not fatal",
			info: nvml.GpuFabricInfo{
				State: nvml.GPU_FABRIC_STATE_IN_PROGRESS,
			},
			expectUse: false,
		},
		{
			name: "completed registration with error status is fatal",
			info: nvml.GpuFabricInfo{
				State:  nvml.GPU_FABRIC_STATE_COMPLETED,
				Status: uint32(nvml.ERROR_UNKNOWN),
			},
			expectErr: "NVLink fabric registration error",
		},
		{
			name: "completed with zero cluster UUID falls back (non-MNNVL)",
			info: nvml.GpuFabricInfo{
				State:  nvml.GPU_FABRIC_STATE_COMPLETED,
				Status: uint32(nvml.SUCCESS),
			},
			expectUse: false,
		},
		{
			name: "completed with cluster UUID contributes to clique",
			info: nvml.GpuFabricInfo{
				State:       nvml.GPU_FABRIC_STATE_COMPLETED,
				Status:      uint32(nvml.SUCCESS),
				ClusterUuid: nonZeroUUID,
			},
			expectUse: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			use, err := evaluateGpuFabricInfo(0, "GPU-test-uuid", tc.info)

			if tc.expectErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.expectErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.expectUse, use)
		})
	}
}
