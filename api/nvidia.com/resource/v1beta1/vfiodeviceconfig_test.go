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
)

func TestVfioDeviceConfigNormalizeEnableAPIDevice(t *testing.T) {
	testCases := map[string]struct {
		config           *configapi.VfioDeviceConfig
		wantEnableAPIDev bool
	}{
		"nil iommu defaults to enabled": {
			config:           &configapi.VfioDeviceConfig{},
			wantEnableAPIDev: true,
		},
		"nil enableAPIDevice defaults to enabled": {
			config: &configapi.VfioDeviceConfig{
				Iommu: &configapi.IOMMUConfig{},
			},
			wantEnableAPIDev: true,
		},
		"explicit false is preserved": {
			config: &configapi.VfioDeviceConfig{
				Iommu: &configapi.IOMMUConfig{EnableAPIDevice: ptr(false)},
			},
			wantEnableAPIDev: false,
		},
		"explicit true is preserved": {
			config: &configapi.VfioDeviceConfig{
				Iommu: &configapi.IOMMUConfig{EnableAPIDevice: ptr(true)},
			},
			wantEnableAPIDev: true,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, tc.config.Normalize())
			require.Equal(t, tc.wantEnableAPIDev, tc.config.Iommu.ShouldEnableAPIDevice())
		})
	}
}
