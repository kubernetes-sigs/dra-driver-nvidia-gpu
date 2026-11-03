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

package vgpuprofiles

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	tests := map[string]struct {
		doc         string
		wantErr     string
		wantRules   int
		wantProfile string
	}{
		"minimal valid policy": {
			doc: `
apiVersion: resource.nvidia.com/v1beta1
kind: VgpuProfilePolicy
rules:
- gpuModel: "NVIDIA L40S"
  profiles:
  - name: "NVIDIA L40S-12Q"
`,
			wantRules:   1,
			wantProfile: "NVIDIA L40S-12Q",
		},
		"catch-all rule without gpuModel": {
			doc: `
apiVersion: resource.nvidia.com/v1beta1
kind: VgpuProfilePolicy
rules:
- profiles:
  - name: "NVIDIA L40S-12Q"
`,
			wantRules:   1,
			wantProfile: "NVIDIA L40S-12Q",
		},
		"no rules is valid and advertises nothing": {
			doc: `
apiVersion: resource.nvidia.com/v1beta1
kind: VgpuProfilePolicy
`,
			wantRules: 0,
		},
		"wrong apiVersion": {
			doc: `
apiVersion: example.com/v1
kind: VgpuProfilePolicy
`,
			wantErr: "unsupported apiVersion",
		},
		"wrong kind": {
			doc: `
apiVersion: resource.nvidia.com/v1beta1
kind: NotACatalog
`,
			wantErr: "unsupported kind",
		},
		"missing envelope": {
			doc: `
rules:
- profiles:
  - name: "NVIDIA L40S-12Q"
`,
			wantErr: "unsupported apiVersion",
		},
		// Strict decoding is what makes the format safe to extend: a
		// field name that does not exist yet must not be silently ignored.
		"unknown field is rejected": {
			doc: `
apiVersion: resource.nvidia.com/v1beta1
kind: VgpuProfilePolicy
rules:
- profiles:
  - name: "NVIDIA L40S-12Q"
    maxInstances: 4
`,
			wantErr: "error decoding vGPU profile policy",
		},
		"rule without profiles": {
			doc: `
apiVersion: resource.nvidia.com/v1beta1
kind: VgpuProfilePolicy
rules:
- gpuModel: "NVIDIA L40S"
  profiles: []
`,
			wantErr: "at least one profile is required",
		},
		"profile without name": {
			doc: `
apiVersion: resource.nvidia.com/v1beta1
kind: VgpuProfilePolicy
rules:
- profiles:
  - name: ""
`,
			wantErr: "name is required",
		},
		"duplicate profile in one rule": {
			doc: `
apiVersion: resource.nvidia.com/v1beta1
kind: VgpuProfilePolicy
rules:
- profiles:
  - name: "NVIDIA L40S-12Q"
  - name: "NVIDIA L40S-12Q"
`,
			wantErr: "duplicate profile",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c, err := Parse([]byte(tc.doc))
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Len(t, c.Rules, tc.wantRules)
			if tc.wantProfile != "" {
				assert.Equal(t, tc.wantProfile, c.Rules[0].Profiles[0].Name)
			}
		})
	}
}

func TestParseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
apiVersion: resource.nvidia.com/v1beta1
kind: VgpuProfilePolicy
rules:
- profiles:
  - name: "NVIDIA L40S-12Q"
`), 0o600))

	c, err := ParseFile(path)
	require.NoError(t, err)
	require.False(t, c.Empty())

	_, err = ParseFile(filepath.Join(t.TempDir(), "missing.yaml"))
	require.Error(t, err)
}

func TestProfilesForGPU(t *testing.T) {
	c, err := Parse([]byte(`
apiVersion: resource.nvidia.com/v1beta1
kind: VgpuProfilePolicy
rules:
- gpuModel: "NVIDIA L40S"
  profiles:
  - name: "NVIDIA L40S-12Q"
  - name: "NVIDIA L40S-24Q"
- gpuModel: "NVIDIA A100-SXM4-40GB"
  profiles:
  - name: "NVIDIA A100-4C"
- profiles:
  - name: "NVIDIA SHARED-EVERYWHERE"
`))
	require.NoError(t, err)

	t.Run("model-specific rule unions with catch-all", func(t *testing.T) {
		got := c.ProfilesForGPU("NVIDIA L40S")
		require.Len(t, got, 3)
		assert.Contains(t, got, "NVIDIA L40S-12Q")
		assert.Contains(t, got, "NVIDIA L40S-24Q")
		assert.Contains(t, got, "NVIDIA SHARED-EVERYWHERE")
		assert.NotContains(t, got, "NVIDIA A100-4C")
	})

	t.Run("unmatched model still gets the catch-all rule", func(t *testing.T) {
		got := c.ProfilesForGPU("NVIDIA H100 PCIe")
		require.Len(t, got, 1)
		assert.Contains(t, got, "NVIDIA SHARED-EVERYWHERE")
	})

	t.Run("empty policy matches nothing", func(t *testing.T) {
		empty := &Policy{APIVersion: APIVersion, Kind: Kind}
		require.NoError(t, empty.Validate())
		assert.True(t, empty.Empty())
		assert.Nil(t, empty.ProfilesForGPU("NVIDIA L40S"))
	})
}
