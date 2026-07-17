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
