/*
 * Copyright 2026 The Kubernetes Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

// mockFileChecker implements fileChecker for tests.
// existingPath is the single path Stat should report as existing; empty means nothing exists.
type mockFileChecker struct {
	existingPath string
}

func (m *mockFileChecker) Stat(path string) error {
	if path == m.existingPath {
		return nil
	}
	return errors.New("not found")
}

func TestSetMpsShmMountPath(t *testing.T) {
	testCases := map[string]struct {
		existingPath      string
		expectedMountPath string
	}{
		// /dev/shm exists under the driver root → daemon uses chroot → shm at <driverRootMountDir>/dev/shm.
		"dev/shm exists under driver root": {
			existingPath:      filepath.Join(driverRootMountDir, "dev", "shm"),
			expectedMountPath: filepath.Join(driverRootMountDir, "dev", "shm"),
		},
		// /dev/shm not present under driver root (e.g. GKE COS) → daemon runs directly
		// in the container namespace → shm at /dev/shm.
		"dev/shm does not exist under driver root — case for GKE COS": {
			existingPath:      "",
			expectedMountPath: MpsDefaultShmMountPath,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			checker := &mockFileChecker{existingPath: tc.existingPath}
			require.Equal(t, tc.expectedMountPath, setMpsShmMountPath(checker))
		})
	}
}

func TestRenderMpsControlDaemonDeploymentImagePullSettings(t *testing.T) {
	deployment, err := renderMpsControlDaemonDeployment(
		filepath.Join("..", "..", "templates", "mps-control-daemon.tmpl.yaml"),
		MpsControlDaemonTemplateData{
			NodeName:                  "node-a",
			MpsControlDaemonNamespace: "dra-driver-nvidia-gpu",
			MpsControlDaemonName:      "mps-control-daemon-test",
			CUDA_VISIBLE_DEVICES:      "GPU-0",
			NvidiaDriverRoot:          "/",
			MpsShmDirectory:           "/var/lib/kubelet/plugins/gpu.nvidia.com/mps/test/shm",
			MpsPipeDirectory:          "/var/lib/kubelet/plugins/gpu.nvidia.com/mps/test/pipe",
			MpsLogDirectory:           "/var/lib/kubelet/plugins/gpu.nvidia.com/mps/test/log",
			MpsImageName:              "registry.example.com/dra-driver:dev",
			MpsImagePullPolicy:        "Always",
			MpsImagePullSecretNames:   []string{"regcred", "mirrorcred"},
			MpsShmMountPath:           MpsDefaultShmMountPath,
		},
	)
	require.NoError(t, err)

	require.Equal(t, []corev1.LocalObjectReference{
		{Name: "regcred"},
		{Name: "mirrorcred"},
	}, deployment.Spec.Template.Spec.ImagePullSecrets)
	require.Len(t, deployment.Spec.Template.Spec.Containers, 1)
	require.Equal(t, corev1.PullAlways, deployment.Spec.Template.Spec.Containers[0].ImagePullPolicy)
}

// TestValidateNoSharingWithAdminAccess crosses "does the config request sharing"
// with "is any result allocated with adminAccess". Only the both-true corner is
// rejected; the other three are supported behaviour today.
func TestValidateNoSharingWithAdminAccess(t *testing.T) {
	result := func(request string, adminAccess bool) *resourceapi.DeviceRequestAllocationResult {
		return &resourceapi.DeviceRequestAllocationResult{
			Request:     request,
			Driver:      DriverName,
			AdminAccess: ptr.To(adminAccess),
		}
	}

	// An empty errContains means the config must be accepted.
	testCases := map[string]struct {
		config      configapi.Sharing
		results     []*resourceapi.DeviceRequestAllocationResult
		errContains string
	}{
		// The normal monitoring claim. DefaultGpuConfig().Sharing is a nil
		// *GpuSharing inside a non-nil interface, so this also covers the
		// nil-receiver safety of IsTimeSlicing/IsMps.
		"no sharing config with admin access is allowed": {
			config:  configapi.DefaultGpuConfig().Sharing,
			results: []*resourceapi.DeviceRequestAllocationResult{result("admin", true)},
		},
		"time-slicing without admin access is allowed": {
			config:  &configapi.GpuSharing{Strategy: configapi.TimeSlicingStrategy},
			results: []*resourceapi.DeviceRequestAllocationResult{result("workload", false)},
		},
		// Built literally, not via the helper, so AdminAccess really is nil: the
		// guard must not read nil as true.
		"admin access without the flag set is allowed": {
			config: &configapi.GpuSharing{Strategy: configapi.TimeSlicingStrategy},
			results: []*resourceapi.DeviceRequestAllocationResult{
				{Request: "workload", Driver: DriverName},
			},
		},
		"time-slicing with admin access is rejected": {
			config:      &configapi.GpuSharing{Strategy: configapi.TimeSlicingStrategy},
			results:     []*resourceapi.DeviceRequestAllocationResult{result("admin", true)},
			errContains: `TimeSlicing sharing configuration is not allowed for request "admin"`,
		},
		"MPS with admin access is rejected": {
			config:      &configapi.GpuSharing{Strategy: configapi.MpsStrategy},
			results:     []*resourceapi.DeviceRequestAllocationResult{result("admin", true)},
			errContains: `MPS sharing configuration is not allowed for request "admin"`,
		},
		// MigDeviceSharing is a second implementation of configapi.Sharing: this
		// case breaks if anyone type-asserts to *GpuSharing.
		"MPS on a MIG device with admin access is rejected": {
			config:      &configapi.MigDeviceSharing{Strategy: configapi.MpsStrategy},
			results:     []*resourceapi.DeviceRequestAllocationResult{result("admin", true)},
			errContains: `MPS sharing configuration is not allowed for request "admin"`,
		},
		// The admin result is second on purpose: an implementation that only
		// inspected results[0] passes every other case in this table.
		"admin access mixed with a workload request is rejected": {
			config: &configapi.GpuSharing{Strategy: configapi.TimeSlicingStrategy},
			results: []*resourceapi.DeviceRequestAllocationResult{
				result("workload", false),
				result("admin", true),
			},
			errContains: `TimeSlicing sharing configuration is not allowed for request "admin"`,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			err := validateNoSharingWithAdminAccess(tc.config, tc.results)
			if tc.errContains == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.errContains)
		})
	}
}

// TestGetConfigResultsMapSharingSourceForAdminAccess pins which sharing configs
// reach applySharingConfig for an adminAccess result. DeviceClass-sourced sharing
// is skipped, so the result falls through to the default config; claim-sourced
// sharing is left in place for validateNoSharingWithAdminAccess to reject.
func TestGetConfigResultsMapSharingSourceForAdminAccess(t *testing.T) {
	sharingConfig := configapi.DefaultGpuConfig()
	sharingConfig.Sharing = &configapi.GpuSharing{Strategy: configapi.TimeSlicingStrategy}
	raw, err := json.Marshal(sharingConfig)
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		source          resourceapi.AllocationConfigSource
		expectedSharing bool
	}{
		"DeviceClass sharing config is not applied to an adminAccess result": {
			source:          resourceapi.AllocationConfigSourceClass,
			expectedSharing: false,
		},
		"claim sharing config still reaches the adminAccess result": {
			source:          resourceapi.AllocationConfigSourceClaim,
			expectedSharing: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			state := &DeviceState{
				perGPUAllocatable: &PerGPUAllocatableDevices{
					allocatablesMap: map[PCIBusID]AllocatableDevices{
						"0000:00:00.0": {
							"gpu-0": &AllocatableDevice{Gpu: &GpuInfo{UUID: "GPU-0000"}},
						},
					},
				},
			}

			allocation := &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{
							Request:     "monitor",
							Driver:      DriverName,
							Device:      "gpu-0",
							AdminAccess: ptr.To(true),
						},
					},
					Config: []resourceapi.DeviceAllocationConfiguration{
						{
							Source: tc.source,
							DeviceConfiguration: resourceapi.DeviceConfiguration{
								Opaque: &resourceapi.OpaqueDeviceConfiguration{
									Driver:     DriverName,
									Parameters: runtime.RawExtension{Raw: raw},
								},
							},
						},
					},
				},
			}

			configResultsMap, err := state.getConfigResultsMap(allocation)
			require.NoError(t, err)
			require.Len(t, configResultsMap, 1, "the result must be grouped under exactly one config")

			for c := range configResultsMap {
				require.Equal(t, tc.expectedSharing, sharingRequested(c))
			}
		})
	}
}

// TestApplySharingConfigRejectsAdminAccess asserts the validation is reached from
// the prepare path: the table test above keeps passing if the call site is deleted.
//
// The zero-value DeviceState is the assertion, not a shortcut. The guard is the
// first statement in applySharingConfig, so a rejected input returns before any of
// the nil fields is dereferenced. Move the guard down and this test panics.
func TestApplySharingConfigRejectsAdminAccess(t *testing.T) {
	state := &DeviceState{}

	// The claim and the checkpoint are empty because the guard reads neither.
	claim := &resourceapi.ResourceClaim{}

	results := []*resourceapi.DeviceRequestAllocationResult{
		{Request: "admin", Driver: DriverName, AdminAccess: ptr.To(true)},
	}

	_, err := state.applySharingConfig(
		context.Background(),
		&configapi.GpuSharing{Strategy: configapi.TimeSlicingStrategy},
		claim,
		results,
		nil,
	)
	require.ErrorContains(t, err, `not allowed for request "admin"`)
}
