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

// TestValidateNoSharingWithAdminAccess covers the validation in isolation. It needs no
// NVML, no MPS daemon and no DeviceState at all, because the function under test is a
// pure function of (config, results) — that is the reason it was factored out of
// applySharingConfig rather than written inline.
//
// The two axes being crossed are "does the config request sharing" and "is any result
// allocated with adminAccess". Only the both-true corner may be rejected; the other
// three corners are existing supported behaviour and would be regressions if broken.
func TestValidateNoSharingWithAdminAccess(t *testing.T) {
	// Small constructor so each case reads as intent rather than struct literal noise.
	// Driver is set to DriverName because prepareDevices only ever passes this
	// function results belonging to our driver (it filters on result.Driver first).
	result := func(request string, adminAccess bool) *resourceapi.DeviceRequestAllocationResult {
		return &resourceapi.DeviceRequestAllocationResult{
			Request:     request,
			Driver:      DriverName,
			AdminAccess: ptr.To(adminAccess),
		}
	}

	// Map-based table, matching the style used elsewhere in this file. An empty
	// errContains means "must be accepted"; a non-empty one is matched as a substring
	// so the assertion pins the meaningful part of the message without becoming
	// brittle about the trailing explanation.
	testCases := map[string]struct {
		config      configapi.Sharing
		results     []*resourceapi.DeviceRequestAllocationResult
		errContains string
	}{
		// The important negative case: this is what a normal monitoring/debug claim
		// looks like. DefaultGpuConfig().Sharing is a nil *GpuSharing, which reaches
		// the function as a non-nil interface holding a nil pointer. It must be
		// accepted, and it exercises the nil-receiver safety of IsTimeSlicing/IsMps.
		"no sharing config with admin access is allowed": {
			config:  configapi.DefaultGpuConfig().Sharing,
			results: []*resourceapi.DeviceRequestAllocationResult{result("admin", true)},
		},
		// Ordinary time-slicing workload: sharing is exactly what it is for.
		"time-slicing without admin access is allowed": {
			config:  &configapi.GpuSharing{Strategy: configapi.TimeSlicingStrategy},
			results: []*resourceapi.DeviceRequestAllocationResult{result("workload", false)},
		},
		// AdminAccess is a *bool, and nil is the common real-world encoding of "not
		// an admin request". Constructed literally here (not via the helper) so the
		// pointer really is nil, proving the guard does not treat nil as true.
		"admin access without the flag set is allowed": {
			config: &configapi.GpuSharing{Strategy: configapi.TimeSlicingStrategy},
			results: []*resourceapi.DeviceRequestAllocationResult{
				{Request: "workload", Driver: DriverName},
			},
		},
		// The bug from the issue, TimeSlicing half.
		"time-slicing with admin access is rejected": {
			config:      &configapi.GpuSharing{Strategy: configapi.TimeSlicingStrategy},
			results:     []*resourceapi.DeviceRequestAllocationResult{result("admin", true)},
			errContains: `TimeSlicing sharing configuration is not allowed for request "admin"`,
		},
		// The bug from the issue, MPS half. Worth covering separately from
		// TimeSlicing: they are different branches of the switch, and MPS is the more
		// destructive of the two (a stopped daemon kills running CUDA contexts).
		"MPS with admin access is rejected": {
			config:      &configapi.GpuSharing{Strategy: configapi.MpsStrategy},
			results:     []*resourceapi.DeviceRequestAllocationResult{result("admin", true)},
			errContains: `MPS sharing configuration is not allowed for request "admin"`,
		},
		// MigDeviceSharing is a different concrete type implementing the same
		// configapi.Sharing interface. It reaches applySharingConfig through the
		// MigDeviceConfig arm of applyConfig, so it must be covered too — this is the
		// case that would break if someone later type-asserted to *GpuSharing.
		"MPS on a MIG device with admin access is rejected": {
			config:      &configapi.MigDeviceSharing{Strategy: configapi.MpsStrategy},
			results:     []*resourceapi.DeviceRequestAllocationResult{result("admin", true)},
			errContains: `MPS sharing configuration is not allowed for request "admin"`,
		},
		// The subtle one. A config whose Requests list is empty applies to every
		// request in the claim, so a single group can hold both kinds of request. The
		// admin result is placed SECOND on purpose: an implementation that only
		// inspected results[0] would pass every other case in this table and still
		// ship the bug.
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

// TestApplySharingConfigRejectsAdminAccess asserts that the validation is actually
// REACHED from the prepare path.
//
// Why a second test at all: TestValidateNoSharingWithAdminAccess above tests the
// function directly, so it keeps passing even if someone deletes the call site in
// applySharingConfig. That would leave the bug fully reintroduced with a green suite.
// This test fails in that scenario, so it is the one protecting the fix.
//
// Why a zero-value DeviceState works, with no mocks, no NVML and no fixtures: the
// guard is the first statement in applySharingConfig, so for a rejected input the
// function returns before it dereferences s.perGPUAllocatable, s.tsManager or
// s.mpsManager. Those being nil is not an oversight in the test — it is the assertion.
// If the guard is moved down or removed, this test does not merely fail, it panics with
// a nil pointer dereference, which is a loud signal that the ordering invariant broke.
// TestGetConfigResultsMapSharingSourceForAdminAccess pins which sharing configs are
// allowed to reach applySharingConfig for an adminAccess result.
//
// A DeviceClass-sourced sharing config applies to every matching request in every
// claim and the claim author cannot remove it, so rejecting it would leave monitoring
// pods permanently unpreparable on that class. It is skipped here instead, and the
// result falls through to the default config, which requests no sharing. A
// claim-sourced config is the author's own mistake, so it is left in place and
// validateNoSharingWithAdminAccess reports it.
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

func TestApplySharingConfigRejectsAdminAccess(t *testing.T) {
	state := &DeviceState{}

	// An empty claim is fine: the guard never reads the claim. It only appears in the
	// error messages further down the function, which we never get to here.
	claim := &resourceapi.ResourceClaim{}

	results := []*resourceapi.DeviceRequestAllocationResult{
		{Request: "admin", Driver: DriverName, AdminAccess: ptr.To(true)},
	}

	// The checkpoint is nil for the same reason the DeviceState fields are: the guard
	// returns before anything downstream touches it.
	_, err := state.applySharingConfig(
		context.Background(),
		&configapi.GpuSharing{Strategy: configapi.TimeSlicingStrategy},
		claim,
		results,
		nil,
	)
	// Deliberately a looser substring than the table test uses: this test is about the
	// call being wired up, not about the exact wording, which is pinned above.
	require.ErrorContains(t, err, `not allowed for request "admin"`)
}
