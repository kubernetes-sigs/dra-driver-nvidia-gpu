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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func TestShouldManageIMEX(t *testing.T) {
	previous := featuregates.Enabled(featuregates.NodeLocalFabricIPC)
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
			string(featuregates.NodeLocalFabricIPC): previous,
		}))
	})

	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.NodeLocalFabricIPC): false,
	}))
	assert.False(t, shouldManageIMEX(""))
	assert.True(t, shouldManageIMEX("clique-a"))

	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.NodeLocalFabricIPC): true,
	}))
	assert.True(t, shouldManageIMEX(""))
}
