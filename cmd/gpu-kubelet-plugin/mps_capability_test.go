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
	"fmt"
	"testing"

	"github.com/Masterminds/semver/v3"
	"github.com/stretchr/testify/require"
)

func TestEnsureCapability(t *testing.T) {
	mockGpus := map[string]*GpuInfo{
		"3fee046f-55d0-4b6a-ac23-b80f6af87123": {CudaComputeCapability: "10.0"},
		"025f9375-cd42-40ec-aa2f-7719143654c5": {CudaComputeCapability: "8.5"},
		"04d8ce42-b7f6-4bd8-9eb8-7bc8d2af4111": {CudaComputeCapability: "7.0"},
		"4af11389-15b5-4fcd-9524-97d4a13ed838": {CudaComputeCapability: "6.2"},
		"ffcb7875-4980-4c6b-8bac-55f8278e0ea6": {CudaComputeCapability: "0.0"},
		"43ce870d-cc46-41ba-b818-8211db9f95b8": {CudaComputeCapability: ""},
	}

	tests := map[string]struct {
		gpuIDs        []string
		expectErr     bool
		expectedError string
	}{
		"all GPUs meet capability": {
			gpuIDs: []string{
				"3fee046f-55d0-4b6a-ac23-b80f6af87123",
				"025f9375-cd42-40ec-aa2f-7719143654c5",
				"04d8ce42-b7f6-4bd8-9eb8-7bc8d2af4111",
			},
			expectErr: false,
		},

		"single insufficient capability": {
			gpuIDs: []string{
				"3fee046f-55d0-4b6a-ac23-b80f6af87123",
				"025f9375-cd42-40ec-aa2f-7719143654c5",
				"04d8ce42-b7f6-4bd8-9eb8-7bc8d2af4111",
				"4af11389-15b5-4fcd-9524-97d4a13ed838",
			},
			expectErr: true,
			expectedError: fmt.Sprintf(
				"4af11389-15b5-4fcd-9524-97d4a13ed838 has insufficient cudaComputeCapability %q, wanted >= %q",
				mockGpus["4af11389-15b5-4fcd-9524-97d4a13ed838"].CudaComputeCapability,
				voltaCudaComputeCapability,
			),
		},

		"multiple insufficient capabilities": {
			gpuIDs: []string{
				"3fee046f-55d0-4b6a-ac23-b80f6af87123",
				"025f9375-cd42-40ec-aa2f-7719143654c5",
				"04d8ce42-b7f6-4bd8-9eb8-7bc8d2af4111",
				"4af11389-15b5-4fcd-9524-97d4a13ed838",
				"ffcb7875-4980-4c6b-8bac-55f8278e0ea6",
			},
			expectErr: true,
			expectedError: fmt.Sprintf(
				"ffcb7875-4980-4c6b-8bac-55f8278e0ea6 has insufficient cudaComputeCapability %q, wanted >= %q\n%s has insufficient cudaComputeCapability %q, wanted >= %q",
				mockGpus["ffcb7875-4980-4c6b-8bac-55f8278e0ea6"].CudaComputeCapability,
				voltaCudaComputeCapability,
				"4af11389-15b5-4fcd-9524-97d4a13ed838",
				mockGpus["4af11389-15b5-4fcd-9524-97d4a13ed838"].CudaComputeCapability,
				voltaCudaComputeCapability,
			),
		},

		"parse failure": {
			gpuIDs: []string{
				"3fee046f-55d0-4b6a-ac23-b80f6af87123",
				"025f9375-cd42-40ec-aa2f-7719143654c5",
				"04d8ce42-b7f6-4bd8-9eb8-7bc8d2af4111",
				"43ce870d-cc46-41ba-b818-8211db9f95b8",
			},
			expectErr: true,
			expectedError: fmt.Sprintf(
				"gpu 43ce870d-cc46-41ba-b818-8211db9f95b8 has invalid cudaComputeCapability %q: %s",
				mockGpus["43ce870d-cc46-41ba-b818-8211db9f95b8"].CudaComputeCapability,
				semver.ErrInvalidSemVer.Error(),
			),
		},

		"gpu info not found": {
			gpuIDs: []string{
				"3fee046f-55d0-4b6a-ac23-b80f6af87123",
				"025f9375-cd42-40ec-aa2f-7719143654c5",
				"04d8ce42-b7f6-4bd8-9eb8-7bc8d2af4111",
				"e72a9f56-04d3-4360-9923-5d0cd2b226bc",
			},
			expectErr: true,
			expectedError: fmt.Sprintf(
				"could not check gpu e72a9f56-04d3-4360-9923-5d0cd2b226bc: missing gpu info (required >= %q)",
				voltaCudaComputeCapability,
			),
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := ensureCapability(mockGpus, tc.gpuIDs, voltaCudaComputeCapability)

			if tc.expectErr {
				require.Error(t, err)
				require.Equal(t, tc.expectedError, err.Error())
				return
			}

			require.NoError(t, err)
		})
	}
}
