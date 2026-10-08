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

package common

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	cperrors "k8s.io/kubernetes/pkg/kubelet/checkpointmanager/errors"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsCheckpointCorruptionError(t *testing.T) {
	var invalidJSON map[string]any
	syntaxError := json.Unmarshal([]byte(`{"v2":`), &invalidJSON)

	var invalidType struct {
		Checksum int `json:"checksum"`
	}
	typeError := json.Unmarshal([]byte(`{"checksum":"invalid"}`), &invalidType)

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "soft corruption",
			err:      cperrors.CorruptCheckpointError{},
			expected: true,
		},
		{
			name:     "wrapped hard corruption with invalid JSON",
			err:      fmt.Errorf("read checkpoint: %w", syntaxError),
			expected: true,
		},
		{
			name:     "hard corruption with invalid field type",
			err:      typeError,
			expected: true,
		},
		{
			name:     "unrelated filesystem error",
			err:      errors.New("permission denied"),
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, IsCheckpointCorruptionError(tc.err))
		})
	}
}

func TestDescribeCheckpointCorruption(t *testing.T) {
	var invalidJSON map[string]any
	syntaxError := json.Unmarshal([]byte(`{"v2":`), &invalidJSON)

	tests := []struct {
		name     string
		err      error
		expected string
	}{
		{
			name:     "soft corruption",
			err:      cperrors.CorruptCheckpointError{},
			expected: "checksum verification failed",
		},
		{
			name:     "hard corruption",
			err:      syntaxError,
			expected: "invalid checkpoint JSON: unexpected end of JSON input",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, DescribeCheckpointCorruption(tc.err))
		})
	}
}

func TestHasClaimCDISpec(t *testing.T) {
	const driverName = "gpu.nvidia.com"

	tests := []struct {
		name     string
		files    []string
		expected bool
	}{
		{
			name: "no specs",
		},
		{
			name:     "claim YAML spec",
			files:    []string{"k8s.gpu.nvidia.com-claim_claim-uid.yaml"},
			expected: true,
		},
		{
			name:     "claim JSON spec",
			files:    []string{"k8s.gpu.nvidia.com-claim_claim-uid.json"},
			expected: true,
		},
		{
			name:  "unrelated base spec",
			files: []string{"k8s.gpu.nvidia.com-device_base.yaml"},
		},
		{
			name:  "other driver claim spec",
			files: []string{"k8s.compute-domain.nvidia.com-claim_claim-uid.yaml"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cdiRoot := t.TempDir()
			for _, name := range tc.files {
				require.NoError(t, os.WriteFile(filepath.Join(cdiRoot, name), []byte("spec"), 0o600))
			}

			hasClaimSpec, err := HasClaimCDISpec(cdiRoot, driverName)

			require.NoError(t, err)
			assert.Equal(t, tc.expected, hasClaimSpec)
		})
	}

	t.Run("directory read error", func(t *testing.T) {
		hasClaimSpec, err := HasClaimCDISpec(filepath.Join(t.TempDir(), "missing"), driverName)

		assert.False(t, hasClaimSpec)
		assert.ErrorContains(t, err, "read CDI directory")
	})
}
