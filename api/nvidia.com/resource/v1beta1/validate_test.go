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

package v1beta1_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// TestGpuSharingValidateRejectsConfigOfOtherStrategy checks that a config
// which GetTimeSlicingConfig or GetMpsConfig would refuse when the claim is
// prepared is already refused by Validate, which the webhook runs.
func TestGpuSharingValidateRejectsConfigOfOtherStrategy(t *testing.T) {
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.TimeSlicingSettings): true,
		string(featuregates.MPSSupport):          true,
	}))

	testCases := []struct {
		description string
		sharing     *configapi.GpuSharing
		expectError string
	}{
		{
			description: "time-slicing with only a time-slicing config",
			sharing: &configapi.GpuSharing{
				Strategy:          configapi.TimeSlicingStrategy,
				TimeSlicingConfig: &configapi.TimeSlicingConfig{Interval: ptr(configapi.ShortTimeSlice)},
			},
		},
		{
			description: "time-slicing with an MPS config",
			sharing: &configapi.GpuSharing{
				Strategy:          configapi.TimeSlicingStrategy,
				TimeSlicingConfig: &configapi.TimeSlicingConfig{Interval: ptr(configapi.ShortTimeSlice)},
				MpsConfig:         &configapi.MpsConfig{},
			},
			expectError: "cannot use MpsConfig with the 'TimeSlicing' strategy",
		},
		{
			description: "MPS with only an MPS config",
			sharing: &configapi.GpuSharing{
				Strategy:  configapi.MpsStrategy,
				MpsConfig: &configapi.MpsConfig{},
			},
		},
		{
			description: "MPS with a time-slicing config",
			sharing: &configapi.GpuSharing{
				Strategy:          configapi.MpsStrategy,
				TimeSlicingConfig: &configapi.TimeSlicingConfig{Interval: ptr(configapi.ShortTimeSlice)},
				MpsConfig:         &configapi.MpsConfig{},
			},
			expectError: "cannot use TimeSlicingConfig with the 'MPS' strategy",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			config := &configapi.GpuConfig{Sharing: tc.sharing}
			require.NoError(t, config.Normalize())
			err := config.Validate()

			// Validate and the getter used at prepare time must agree.
			var getErr error
			if tc.sharing.IsTimeSlicing() {
				_, getErr = tc.sharing.GetTimeSlicingConfig()
			} else {
				_, getErr = tc.sharing.GetMpsConfig()
			}

			if tc.expectError == "" {
				require.NoError(t, err)
				require.NoError(t, getErr)
				return
			}
			require.EqualError(t, err, tc.expectError)
			require.Error(t, getErr)
		})
	}
}

func TestMpsConfigValidatePinnedMemoryLimits(t *testing.T) {
	testCases := []struct {
		description string
		config      *configapi.MpsConfig
		expectError error
	}{
		{
			description: "no limits",
			config:      &configapi.MpsConfig{},
		},
		{
			description: "valid default and per-device limits",
			config: &configapi.MpsConfig{
				DefaultPinnedDeviceMemoryLimit: ptr(resource.MustParse("1Gi")),
				DefaultPerDevicePinnedMemoryLimit: configapi.MpsPerDevicePinnedMemoryLimit{
					"0":        resource.MustParse("1Mi"),
					"GPU-UUID": resource.MustParse("2Gi"),
				},
			},
		},
		{
			description: "default limit below 1Mi",
			config: &configapi.MpsConfig{
				DefaultPinnedDeviceMemoryLimit: ptr(resource.MustParse("512Ki")),
			},
			expectError: configapi.ErrInvalidLimit,
		},
		{
			description: "zero default limit",
			config: &configapi.MpsConfig{
				DefaultPinnedDeviceMemoryLimit: ptr(resource.MustParse("0")),
			},
			expectError: configapi.ErrInvalidLimit,
		},
		{
			description: "negative per-device limit",
			config: &configapi.MpsConfig{
				DefaultPerDevicePinnedMemoryLimit: configapi.MpsPerDevicePinnedMemoryLimit{
					"0": resource.MustParse("-1Gi"),
				},
			},
			expectError: configapi.ErrInvalidLimit,
		},
		{
			description: "per-device limit below 1Mi",
			config: &configapi.MpsConfig{
				DefaultPerDevicePinnedMemoryLimit: configapi.MpsPerDevicePinnedMemoryLimit{
					"GPU-UUID": resource.MustParse("100"),
				},
			},
			expectError: configapi.ErrInvalidLimit,
		},
		{
			description: "negative device index",
			config: &configapi.MpsConfig{
				DefaultPerDevicePinnedMemoryLimit: configapi.MpsPerDevicePinnedMemoryLimit{
					"-1": resource.MustParse("1Gi"),
				},
			},
			expectError: configapi.ErrInvalidDeviceSelector,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			err := tc.config.Validate()
			if tc.expectError == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.expectError)

			// The same config also fails when the MPS control daemon is
			// started, so rejecting it at admission does not refuse
			// anything that could have been prepared.
			_, normalizeErr := tc.config.DefaultPerDevicePinnedMemoryLimit.Normalize([]string{"GPU-UUID"}, tc.config.DefaultPinnedDeviceMemoryLimit)
			require.ErrorIs(t, normalizeErr, tc.expectError)
		})
	}
}

func TestMigDeviceConfigValidateRejectsTooLowPinnedMemoryLimit(t *testing.T) {
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.MPSSupport): true,
	}))

	config := &configapi.MigDeviceConfig{
		Sharing: &configapi.MigDeviceSharing{
			Strategy: configapi.MpsStrategy,
			MpsConfig: &configapi.MpsConfig{
				DefaultPinnedDeviceMemoryLimit: ptr(resource.MustParse("100Ki")),
			},
		},
	}
	require.NoError(t, config.Normalize())
	require.ErrorIs(t, config.Validate(), configapi.ErrInvalidLimit)
}
