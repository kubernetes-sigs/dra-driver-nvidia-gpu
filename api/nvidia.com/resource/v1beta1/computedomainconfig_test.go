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

func TestValidateDomainID(t *testing.T) {
	testCases := []struct {
		description string
		domainID    string
		expectError bool
	}{
		{
			description: "valid UID-shaped domainID",
			domainID:    "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		},
		{
			description: "empty",
			domainID:    "",
			expectError: true,
		},
		{
			description: "dot",
			domainID:    ".",
			expectError: true,
		},
		{
			description: "dot-dot",
			domainID:    "..",
			expectError: true,
		},
		{
			description: "relative path traversal",
			domainID:    "../../../../../../escape-target/pwn",
			expectError: true,
		},
		{
			description: "absolute path",
			domainID:    "/etc/passwd",
			expectError: true,
		},
		{
			description: "embedded traversal segment",
			domainID:    "foo/../../bar",
			expectError: true,
		},
		{
			description: "single embedded slash",
			domainID:    "foo/bar",
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			err := configapi.ValidateDomainID(tc.domainID)
			if tc.expectError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestComputeDomainChannelConfigValidate(t *testing.T) {
	const validDomainID = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"

	testCases := []struct {
		description    string
		domainID       string
		allocationMode string
		expectError    bool
	}{
		{
			description:    "empty allocationMode",
			domainID:       validDomainID,
			allocationMode: "",
		},
		{
			description:    "Single allocationMode",
			domainID:       validDomainID,
			allocationMode: configapi.ComputeDomainChannelAllocationModeSingle,
		},
		{
			description:    "All allocationMode",
			domainID:       validDomainID,
			allocationMode: configapi.ComputeDomainChannelAllocationModeAll,
		},
		{
			description:    "lowercase all allocationMode",
			domainID:       validDomainID,
			allocationMode: "all",
			expectError:    true,
		},
		{
			description:    "lowercase single allocationMode",
			domainID:       validDomainID,
			allocationMode: "single",
			expectError:    true,
		},
		{
			description:    "uppercase ALL allocationMode",
			domainID:       validDomainID,
			allocationMode: "ALL",
			expectError:    true,
		},
		{
			description:    "unknown allocationMode",
			domainID:       validDomainID,
			allocationMode: "foo",
			expectError:    true,
		},
		{
			description:    "invalid domainID with valid allocationMode",
			domainID:       "",
			allocationMode: configapi.ComputeDomainChannelAllocationModeAll,
			expectError:    true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			config := configapi.DefaultComputeDomainChannelConfig()
			config.DomainID = tc.domainID
			config.AllocationMode = tc.allocationMode

			err := config.Validate()
			if tc.expectError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}
