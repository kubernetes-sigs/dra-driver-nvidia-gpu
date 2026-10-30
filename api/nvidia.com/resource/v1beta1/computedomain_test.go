/*
Copyright The Kubernetes Authors.

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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

func TestGetChannels(t *testing.T) {
	template := func(name string) configapi.ComputeDomainResourceClaimTemplate {
		return configapi.ComputeDomainResourceClaimTemplate{Name: name}
	}

	tests := map[string]struct {
		spec configapi.ComputeDomainSpec
		want []configapi.ComputeDomainChannelSpec
	}{
		"channel is a list of one, keeping its allocation mode": {
			spec: configapi.ComputeDomainSpec{Channel: &configapi.ComputeDomainChannelSpec{
				ResourceClaimTemplate: template("job"),
				AllocationMode:        configapi.ComputeDomainChannelAllocationModeAll,
			}},
			want: []configapi.ComputeDomainChannelSpec{{
				ResourceClaimTemplate: template("job"),
				AllocationMode:        configapi.ComputeDomainChannelAllocationModeAll,
			}},
		},
		"channels get one channel each": {
			spec: configapi.ComputeDomainSpec{Channels: []configapi.ComputeDomainChannelEntry{
				{ResourceClaimTemplate: template("train")},
				{ResourceClaimTemplate: template("serve")},
			}},
			want: []configapi.ComputeDomainChannelSpec{
				{ResourceClaimTemplate: template("train"), AllocationMode: configapi.ComputeDomainChannelAllocationModeSingle},
				{ResourceClaimTemplate: template("serve"), AllocationMode: configapi.ComputeDomainChannelAllocationModeSingle},
			},
		},
		"neither": {
			want: []configapi.ComputeDomainChannelSpec{},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.spec.GetChannels())
		})
	}
}

func TestComputeDomainChannelConfigValidateTemplateName(t *testing.T) {
	config := configapi.DefaultComputeDomainChannelConfig()
	config.DomainID = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	config.ResourceClaimTemplateName = strings.Repeat("a", 253)
	require.NoError(t, config.Validate())

	config.ResourceClaimTemplateName += "a"
	require.Error(t, config.Validate())
}
