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
	"testing"

	"github.com/stretchr/testify/require"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func TestCalculateGlobalStatusHostManaged(t *testing.T) {
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{string(featuregates.HostManagedIMEX): true}))
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{string(featuregates.HostManagedIMEX): false}))
	})

	m := &ComputeDomainManager{}

	// In driver-managed mode this would be NotReady (required nodes are
	// missing). Under HostManagedIMEX the controller does not track per-node
	// readiness, so it reports Ready once admitted.
	cd := &nvapi.ComputeDomain{}
	cd.Spec.NumNodes = 8
	require.Equal(t, nvapi.ComputeDomainStatusReady, m.calculateGlobalStatus(cd))
}
