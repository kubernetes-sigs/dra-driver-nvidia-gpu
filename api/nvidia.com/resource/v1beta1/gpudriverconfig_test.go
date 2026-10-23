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

package v1beta1_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

func TestGpuDriverConfigValidate(t *testing.T) {
	tests := map[string]struct {
		advertisement configapi.GpuAdvertisement
		wantErr       bool
	}{
		"all device types": {
			advertisement: configapi.GpuAdvertisement{Gpu: true, Mig: true, Vfio: true},
		},
		"MIG only": {
			advertisement: configapi.GpuAdvertisement{Mig: true},
		},
		"no device type advertised": {
			wantErr: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			config := &configapi.GpuDriverConfig{Advertisement: tc.advertisement}
			require.NoError(t, config.Normalize())
			err := config.Validate()
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestGpuDriverConfigStrictDecode(t *testing.T) {
	tests := map[string]struct {
		raw     string
		want    configapi.GpuAdvertisement
		wantErr string
	}{
		"valid": {
			raw:  `{"apiVersion":"resource.nvidia.com/v1beta1","kind":"GpuDriverConfig","advertisement":{"gpu":true,"vfio":true}}`,
			want: configapi.GpuAdvertisement{Gpu: true, Vfio: true},
		},
		"unknown device type field": {
			raw:     `{"apiVersion":"resource.nvidia.com/v1beta1","kind":"GpuDriverConfig","advertisement":{"migdyn":true}}`,
			wantErr: `unknown field "advertisement.migdyn"`,
		},
		"missing kind": {
			raw:     `{"apiVersion":"resource.nvidia.com/v1beta1","advertisement":{"gpu":true}}`,
			wantErr: "Kind",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			obj, err := runtime.Decode(configapi.StrictDecoder, []byte(tc.raw))
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			config, ok := obj.(*configapi.GpuDriverConfig)
			require.True(t, ok, "decoded %T", obj)
			require.Equal(t, tc.want, config.Advertisement)
		})
	}
}
