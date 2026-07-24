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
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func TestApplyConsumableShares(t *testing.T) {
	defaultMemVal := resource.MustParse("16Gi")

	tests := []struct {
		name                 string
		featureGate          bool
		consumableSharesFlag string
		inputMemory          resource.Quantity
		expectedMaxMemory    resource.Quantity
		expectedMemDefault   resource.Quantity
		expectedMemMin       resource.Quantity
		expectedShares       *int64
		expectModified       bool
	}{
		{
			name:                 "feature gate disabled",
			featureGate:          false,
			consumableSharesFlag: "unlimited",
			inputMemory:          defaultMemVal,
			expectModified:       false,
		},
		{
			name:                 "flag disabled",
			featureGate:          true,
			consumableSharesFlag: "disabled",
			inputMemory:          defaultMemVal,
			expectModified:       false,
		},
		{
			name:                 "unlimited option",
			featureGate:          true,
			consumableSharesFlag: "unlimited",
			inputMemory:          defaultMemVal,
			expectedMaxMemory:    defaultMemVal,
			expectedMemDefault:   resource.MustParse("0"),
			expectedMemMin:       resource.MustParse("0"),
			expectModified:       true,
		},
		{
			name:                 "unlimited option with unaligned memory rounds down max",
			featureGate:          true,
			consumableSharesFlag: "unlimited",
			inputMemory:          *resource.NewQuantity(1048576*10+500, resource.BinarySI),
			expectedMaxMemory:    *resource.NewQuantity(1048576*10, resource.BinarySI),
			expectedMemDefault:   resource.MustParse("0"),
			expectedMemMin:       resource.MustParse("0"),
			expectModified:       true,
		},
		{
			name:                 "memory option",
			featureGate:          true,
			consumableSharesFlag: "memory",
			inputMemory:          defaultMemVal,
			expectedMaxMemory:    defaultMemVal,
			expectedMemDefault:   defaultMemVal,
			expectedMemMin:       resource.MustParse("1Mi"),
			expectModified:       true,
		},
		{
			name:                 "integer shares option (e.g. 4)",
			featureGate:          true,
			consumableSharesFlag: "4",
			inputMemory:          defaultMemVal,
			expectedMaxMemory:    defaultMemVal,
			expectedMemDefault:   resource.MustParse("0"),
			expectedMemMin:       resource.MustParse("0"),
			expectedShares:       new(int64(4)),
			expectModified:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
				string(featuregates.ConsumableShares): tc.featureGate,
			}))

			config := &Config{
				flags: &Flags{
					consumableShares: tc.consumableSharesFlag,
				},
			}

			dev := resourceapi.Device{
				Name: "gpu-0",
				Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
					"memory": {
						Value: tc.inputMemory,
					},
				},
			}

			applyConsumableShares(&dev, config)

			if !tc.expectModified {
				require.Nil(t, dev.AllowMultipleAllocations)
				require.Nil(t, dev.Capacity["memory"].RequestPolicy)
				require.NotContains(t, dev.Capacity, resourceapi.QualifiedName("shares"))
				return
			}

			require.NotNil(t, dev.AllowMultipleAllocations)
			require.True(t, *dev.AllowMultipleAllocations)

			memCap := dev.Capacity["memory"]
			require.Equal(t, tc.expectedMaxMemory, memCap.Value)
			require.NotNil(t, memCap.RequestPolicy)
			require.NotNil(t, memCap.RequestPolicy.Default)
			require.NotNil(t, memCap.RequestPolicy.ValidRange)
			require.Equal(t, resource.MustParse("1Mi"), *memCap.RequestPolicy.ValidRange.Step)
			require.Equal(t, tc.expectedMaxMemory, *memCap.RequestPolicy.ValidRange.Max)
			require.Equal(t, tc.expectedMemDefault, *memCap.RequestPolicy.Default)
			require.Equal(t, tc.expectedMemMin, *memCap.RequestPolicy.ValidRange.Min)

			if tc.expectedShares != nil {
				sharesCap, hasShares := dev.Capacity["shares"]
				require.True(t, hasShares)
				require.True(t, resource.NewQuantity(*tc.expectedShares, resource.DecimalSI).Equal(sharesCap.Value))
				require.NotNil(t, sharesCap.RequestPolicy)
				require.True(t, resource.MustParse("1").Equal(*sharesCap.RequestPolicy.Default))
				require.True(t, resource.MustParse("1").Equal(*sharesCap.RequestPolicy.ValidRange.Min))
				require.True(t, resource.NewQuantity(*tc.expectedShares, resource.DecimalSI).Equal(*sharesCap.RequestPolicy.ValidRange.Max))
				require.True(t, resource.MustParse("1").Equal(*sharesCap.RequestPolicy.ValidRange.Step))
			} else {
				require.NotContains(t, dev.Capacity, resourceapi.QualifiedName("shares"))
			}
		})
	}
}
