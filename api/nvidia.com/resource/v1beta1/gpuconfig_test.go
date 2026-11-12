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

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func TestGpuConfigNormalizeTimeSlicing(t *testing.T) {
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.TimeSlicingSettings): true,
	}))

	testCases := []struct {
		description      string
		config           *configapi.GpuConfig
		expectedInterval configapi.TimeSliceInterval
	}{
		{
			description: "nil time-slicing config gets default interval",
			config: &configapi.GpuConfig{
				Sharing: &configapi.GpuSharing{
					Strategy: configapi.TimeSlicingStrategy,
				},
			},
			expectedInterval: configapi.DefaultTimeSlice,
		},
		{
			description: "empty time-slicing config gets default interval",
			config: &configapi.GpuConfig{
				Sharing: &configapi.GpuSharing{
					Strategy:          configapi.TimeSlicingStrategy,
					TimeSlicingConfig: &configapi.TimeSlicingConfig{},
				},
			},
			expectedInterval: configapi.DefaultTimeSlice,
		},
		{
			description: "explicit interval is preserved",
			config: &configapi.GpuConfig{
				Sharing: &configapi.GpuSharing{
					Strategy: configapi.TimeSlicingStrategy,
					TimeSlicingConfig: &configapi.TimeSlicingConfig{
						Interval: ptr(configapi.ShortTimeSlice),
					},
				},
			},
			expectedInterval: configapi.ShortTimeSlice,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			require.NoError(t, tc.config.Normalize())
			require.NoError(t, tc.config.Validate())
			require.NotNil(t, tc.config.Sharing.TimeSlicingConfig.Interval)
			require.Equal(t, tc.expectedInterval, *tc.config.Sharing.TimeSlicingConfig.Interval)
		})
	}
}

func TestTimeSlicingConfigValidate(t *testing.T) {
	testCases := []struct {
		description string
		config      *configapi.TimeSlicingConfig
		expectError bool
	}{
		{
			description: "nil config",
			config:      nil,
			expectError: true,
		},
		{
			description: "nil interval",
			config:      &configapi.TimeSlicingConfig{},
			expectError: true,
		},
		{
			description: "valid interval",
			config:      &configapi.TimeSlicingConfig{Interval: ptr(configapi.LongTimeSlice)},
			expectError: false,
		},
		{
			description: "unknown interval",
			config:      &configapi.TimeSlicingConfig{Interval: ptr(configapi.TimeSliceInterval("Bogus"))},
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			err := tc.config.Validate()
			if tc.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestGpuConfigValidateRejectsConfigOfOtherStrategy(t *testing.T) {
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
			description: "time-slicing with time-slicing config",
			sharing: &configapi.GpuSharing{
				Strategy:          configapi.TimeSlicingStrategy,
				TimeSlicingConfig: &configapi.TimeSlicingConfig{Interval: ptr(configapi.LongTimeSlice)},
			},
		},
		{
			description: "time-slicing with mps config",
			sharing: &configapi.GpuSharing{
				Strategy:          configapi.TimeSlicingStrategy,
				TimeSlicingConfig: &configapi.TimeSlicingConfig{Interval: ptr(configapi.LongTimeSlice)},
				MpsConfig:         &configapi.MpsConfig{},
			},
			expectError: "cannot use MpsConfig with the 'TimeSlicing' strategy",
		},
		{
			description: "mps with mps config",
			sharing: &configapi.GpuSharing{
				Strategy:  configapi.MpsStrategy,
				MpsConfig: &configapi.MpsConfig{},
			},
		},
		{
			description: "mps with time-slicing config",
			sharing: &configapi.GpuSharing{
				Strategy:          configapi.MpsStrategy,
				MpsConfig:         &configapi.MpsConfig{},
				TimeSlicingConfig: &configapi.TimeSlicingConfig{Interval: ptr(configapi.LongTimeSlice)},
			},
			expectError: "cannot use TimeSlicingConfig with the 'MPS' strategy",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			config := &configapi.GpuConfig{Sharing: tc.sharing}
			require.NoError(t, config.Normalize())
			err := config.Validate()
			if tc.expectError == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tc.expectError)

			// Validate must reject what the kubelet plugin rejects when it
			// reads the config for the selected strategy.
			if config.Sharing.IsTimeSlicing() {
				_, getErr := config.Sharing.GetTimeSlicingConfig()
				require.EqualError(t, getErr, tc.expectError)
			} else {
				_, getErr := config.Sharing.GetMpsConfig()
				require.EqualError(t, getErr, tc.expectError)
			}
		})
	}
}
