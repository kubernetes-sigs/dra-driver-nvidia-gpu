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

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func TestValidateCLIFlagsConsumableShares(t *testing.T) {
	tests := []struct {
		name                 string
		featureGate          bool
		consumableSharesFlag string
		expectErr            bool
	}{
		{
			name:                 "flag unlimited with feature gate enabled succeeds",
			featureGate:          true,
			consumableSharesFlag: "unlimited",
			expectErr:            false,
		},
		{
			name:                 "flag memory with feature gate enabled succeeds",
			featureGate:          true,
			consumableSharesFlag: "memory",
			expectErr:            false,
		},
		{
			name:                 "flag integer 4 with feature gate enabled succeeds",
			featureGate:          true,
			consumableSharesFlag: "4",
			expectErr:            false,
		},
		{
			name:                 "flag integer 0 with feature gate enabled fails",
			featureGate:          true,
			consumableSharesFlag: "0",
			expectErr:            true,
		},
		{
			name:                 "flag integer negative with feature gate enabled fails",
			featureGate:          true,
			consumableSharesFlag: "-2",
			expectErr:            true,
		},
		{
			name:                 "flag unlimited with feature gate disabled fails",
			featureGate:          false,
			consumableSharesFlag: "unlimited",
			expectErr:            true,
		},
		{
			name:                 "flag disabled with feature gate disabled succeeds",
			featureGate:          false,
			consumableSharesFlag: "disabled",
			expectErr:            false,
		},
		{
			name:                 "invalid flag value fails",
			featureGate:          true,
			consumableSharesFlag: "invalid",
			expectErr:            true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
				string(featuregates.ConsumableShares): tc.featureGate,
			}))

			flags := &Flags{
				consumableShares: tc.consumableSharesFlag,
			}

			err := validateCLIFlags(flags)
			if tc.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestResolveVgpuPolicy(t *testing.T) {
	writePolicy := func(t *testing.T, doc string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "policy.yaml")
		require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))
		return path
	}

	validPolicy := `
apiVersion: resource.nvidia.com/v1beta1
kind: VgpuProfilePolicy
rules:
- gpuModel: "NVIDIA L40S"
  profiles:
  - name: "NVIDIA L40S-12Q"
`

	t.Run("no policy file leaves the policy unset", func(t *testing.T) {
		flags := &Flags{}
		require.NoError(t, resolveVgpuPolicy(flags))
		require.Nil(t, flags.vgpuPolicy)
	})

	t.Run("policy file requires the VGPUSupport gate", func(t *testing.T) {
		setVgpuSupport(t, false)
		flags := &Flags{vgpuPolicyFile: writePolicy(t, validPolicy)}
		require.ErrorContains(t, resolveVgpuPolicy(flags), "VGPUSupport")
	})

	t.Run("policy file is parsed and scoped by GPU model", func(t *testing.T) {
		setVgpuSupport(t, true)
		flags := &Flags{vgpuPolicyFile: writePolicy(t, validPolicy)}
		require.NoError(t, resolveVgpuPolicy(flags))
		require.NotNil(t, flags.vgpuPolicy)
		require.Contains(t, flags.vgpuPolicy.ProfilesForGPU("NVIDIA L40S"), "NVIDIA L40S-12Q")
		require.Empty(t, flags.vgpuPolicy.ProfilesForGPU("NVIDIA A100-SXM4-40GB"))
	})

	t.Run("invalid policy file is rejected at startup", func(t *testing.T) {
		setVgpuSupport(t, true)
		flags := &Flags{vgpuPolicyFile: writePolicy(t, "apiVersion: example.com/v1\nkind: VgpuProfilePolicy\n")}
		require.ErrorContains(t, resolveVgpuPolicy(flags), "invalid --vgpu-policy-file")
	})

	t.Run("explicitly requested missing policy file is rejected at startup", func(t *testing.T) {
		setVgpuSupport(t, true)
		flags := &Flags{vgpuPolicyFile: filepath.Join(t.TempDir(), "absent.yaml")}
		require.ErrorContains(t, resolveVgpuPolicy(flags), "invalid --vgpu-policy-file")
	})

	// The flag defaults to the ConfigMap mount path, which only exists when
	// an administrator configured a policy. Its absence must not keep the
	// plugin from starting on a node with no vGPU policy at all.
	t.Run("absent default policy file is not an error", func(t *testing.T) {
		setVgpuSupport(t, true)
		require.NoFileExists(t, defaultVgpuPolicyFile, "test environment unexpectedly has a policy mounted")
		flags := &Flags{vgpuPolicyFile: defaultVgpuPolicyFile}
		require.NoError(t, resolveVgpuPolicy(flags))
		require.Nil(t, flags.vgpuPolicy)
	})

	// The gate check must not fire merely because the flag has a default.
	t.Run("absent default policy file does not require the gate", func(t *testing.T) {
		setVgpuSupport(t, false)
		flags := &Flags{vgpuPolicyFile: defaultVgpuPolicyFile}
		require.NoError(t, resolveVgpuPolicy(flags))
		require.Nil(t, flags.vgpuPolicy)
	})
}

// The default path is only correct as long as the chart keeps mounting the
// policy ConfigMap there, and nothing else ties the two together now that the
// chart no longer passes the path explicitly.
func TestDefaultVgpuPolicyFileMatchesChartMount(t *testing.T) {
	chart, err := os.ReadFile(filepath.Join("..", "..", "deployments", "helm",
		"dra-driver-nvidia-gpu", "templates", "kubeletplugin.yaml"))
	require.NoError(t, err)

	dir, file := filepath.Split(defaultVgpuPolicyFile)
	require.Contains(t, string(chart), "mountPath: "+filepath.Clean(dir),
		"chart mounts the vGPU policy ConfigMap somewhere other than defaultVgpuPolicyFile")

	cm, err := os.ReadFile(filepath.Join("..", "..", "deployments", "helm",
		"dra-driver-nvidia-gpu", "templates", "configmap-vgpu-policy.yaml"))
	require.NoError(t, err)
	require.Contains(t, string(cm), file+": |",
		"ConfigMap key does not match the basename of defaultVgpuPolicyFile")
}

func setVgpuSupport(t *testing.T, enabled bool) {
	t.Helper()
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.VGPUSupport): enabled,
	}))
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
			string(featuregates.VGPUSupport): false,
		}))
	})
}
