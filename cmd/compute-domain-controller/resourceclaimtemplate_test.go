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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

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

const testWorkloadTemplatePath = "../../templates/compute-domain-workload-claim-template.tmpl.yaml"

func workloadTemplateData(namespace, name string) *ResourceClaimTemplateTemplateData {
	channelConfig := nvapi.DefaultComputeDomainChannelConfig()
	channelConfig.DomainID = "cd-uid"
	return &ResourceClaimTemplateTemplateData{
		Namespace:               namespace,
		Name:                    name,
		Finalizer:               computeDomainFinalizer,
		ComputeDomainLabelKey:   computeDomainLabelKey,
		ComputeDomainLabelValue: "cd-uid",
		TargetLabelKey:          computeDomainResourceClaimTemplateTargetLabelKey,
		TargetLabelValue:        computeDomainResourceClaimTemplateTargetWorkload,
		DeviceClassName:         computeDomainDefaultChannelDeviceClass,
		DriverName:              DriverName,
		ChannelConfig:           channelConfig,
	}
}

func TestRenderResourceClaimTemplate(t *testing.T) {
	rct, err := renderResourceClaimTemplate(testWorkloadTemplatePath, workloadTemplateData("tenant-a", "imex-channel-0"))
	require.NoError(t, err)
	require.Equal(t, "tenant-a", rct.Namespace)
	require.Equal(t, "imex-channel-0", rct.Name)
}

// Names carrying YAML must be rejected.
func TestRenderResourceClaimTemplateRejectsInjectedName(t *testing.T) {
	for name, templateName := range map[string]string{
		"namespace injection": "imex-channel-0\n  namespace: victim-b\n  annotations:\n    injected-by: tenant-a",
		"uppercase":           "Imex-Channel",
		"empty":               "",
		"too long":            strings.Repeat("a", 254),
		"trailing whitespace": "imex-channel-0 ",
		"flow mapping":        "{namespace: victim-b}",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := renderResourceClaimTemplate(testWorkloadTemplatePath, workloadTemplateData("tenant-a", templateName))
			require.Error(t, err)
		})
	}
}

// The post-render check catches a wrong namespace on its own.
func TestRenderResourceClaimTemplateRejectsNamespaceMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rct.tmpl.yaml")
	require.NoError(t, os.WriteFile(path, []byte("apiVersion: resource.k8s.io/v1\nkind: ResourceClaimTemplate\nmetadata:\n  namespace: victim-b\n  name: {{ .Name }}\n"), 0o600))

	_, err := renderResourceClaimTemplate(path, workloadTemplateData("tenant-a", "imex-channel-0"))
	require.ErrorContains(t, err, "want tenant-a/imex-channel-0")
}
