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
	"bytes"
	"testing"
	"text/template"

	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/util/yaml"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

func TestChannelAllocationModeFor(t *testing.T) {
	cdWithMode := func(mode string) *nvapi.ComputeDomain {
		cd := &nvapi.ComputeDomain{}
		cd.Spec.Channel = &nvapi.ComputeDomainChannelSpec{AllocationMode: mode}
		return cd
	}

	tests := []struct {
		name        string
		cd          *nvapi.ComputeDomain
		hostManaged bool
		want        string
	}{
		{
			name:        "host-managed always forces Single, even when the ComputeDomain requested All",
			cd:          cdWithMode(nvapi.ComputeDomainChannelAllocationModeAll),
			hostManaged: true,
			want:        nvapi.ComputeDomainChannelAllocationModeSingle,
		},
		{
			name:        "host-managed always forces Single, even when the ComputeDomain requested nothing",
			cd:          cdWithMode(""),
			hostManaged: true,
			want:        nvapi.ComputeDomainChannelAllocationModeSingle,
		},
		{
			name:        "driver-managed passes through the ComputeDomain's AllocationMode unchanged (All)",
			cd:          cdWithMode(nvapi.ComputeDomainChannelAllocationModeAll),
			hostManaged: false,
			want:        nvapi.ComputeDomainChannelAllocationModeAll,
		},
		{
			name:        "driver-managed passes through the ComputeDomain's AllocationMode unchanged (Single)",
			cd:          cdWithMode(nvapi.ComputeDomainChannelAllocationModeSingle),
			hostManaged: false,
			want:        nvapi.ComputeDomainChannelAllocationModeSingle,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, channelAllocationModeFor(tt.cd, tt.hostManaged))
		})
	}
}

func TestWorkloadTemplatePinsHostChannel(t *testing.T) {
	tmpl, err := template.ParseFiles("../../templates/compute-domain-workload-claim-template.tmpl.yaml")
	require.NoError(t, err)
	for _, id := range []*int{nil, new(int), func() *int { id := 1; return &id }()} {
		data := ResourceClaimTemplateTemplateData{
			Namespace: "workload", Name: "channels", Finalizer: computeDomainFinalizer,
			ComputeDomainLabelKey: computeDomainLabelKey, ComputeDomainLabelValue: "domain-a",
			TargetLabelKey: computeDomainResourceClaimTemplateTargetLabelKey, TargetLabelValue: computeDomainResourceClaimTemplateTargetWorkload,
			DeviceClassName: computeDomainDefaultChannelDeviceClass, DriverName: DriverName,
			ChannelConfig: nvapi.DefaultComputeDomainChannelConfig(), ChannelID: id,
		}
		data.ChannelConfig.DomainID = "domain-a"
		if id != nil {
			data.DeviceClassName = computeDomainChannelDeviceClass
		}
		var rendered bytes.Buffer
		require.NoError(t, tmpl.Execute(&rendered, data))
		var claimTemplate resourceapi.ResourceClaimTemplate
		require.NoError(t, yaml.NewYAMLOrJSONDecoder(&rendered, 4096).Decode(&claimTemplate))
		request := claimTemplate.Spec.Spec.Devices.Requests[0].Exactly
		require.Equal(t, data.DeviceClassName, request.DeviceClassName)
		if id == nil {
			require.Empty(t, request.Selectors)
		} else {
			require.Len(t, request.Selectors, 1)
			require.Equal(t, hostChannelSelector(*id), request.Selectors[0].CEL.Expression)
		}
	}
}
