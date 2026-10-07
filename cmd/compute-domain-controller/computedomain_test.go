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
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/component-base/metrics/legacyregistry"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
)

func TestCalculateGlobalStatusHostManaged(t *testing.T) {
	m := &ComputeDomainManager{
		config: &ManagerConfig{
			imexConfig: imex.Config{Mode: imex.ModeHostManaged, Isolation: imex.IsolationIMEXDomain},
		},
	}

	// In driver-managed mode this would be NotReady (required nodes are
	// missing). Under host-managed IMEX the controller does not track
	// per-node readiness, so it reports Ready once admitted.
	cd := &nvapi.ComputeDomain{}
	cd.Spec.NumNodes = 8
	require.Equal(t, nvapi.ComputeDomainStatusReady, m.calculateGlobalStatus(cd))
}

func TestCalculateGlobalStatusDriverManagedUnaffected(t *testing.T) {
	m := &ComputeDomainManager{
		config: &ManagerConfig{
			imexConfig: imex.Config{Mode: imex.ModeDriverManaged},
		},
	}

	cd := &nvapi.ComputeDomain{}
	cd.Spec.NumNodes = 8
	require.Equal(t, nvapi.ComputeDomainStatusNotReady, m.calculateGlobalStatus(cd))
}

func computeDomainInfoValue(t *testing.T, status string) float64 {
	mfs, err := legacyregistry.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			if mf.GetName() == "nvidia_dra_compute_domain_info" && m.GetLabel()[0].GetValue() == status {
				return m.GetGauge().GetValue()
			}
		}
	}
	return 0
}

// After a controller restart, ComputeDomains whose status is already up to
// date must still be counted, even though no status update is issued.
func TestUpdateGlobalStatusObservesUnchangedStatus(t *testing.T) {
	tests := map[string]struct {
		mode   imex.Config
		status string
	}{
		"driver-managed": {imex.Config{Mode: imex.ModeDriverManaged}, nvapi.ComputeDomainStatusNotReady},
		"host-managed":   {imex.Config{Mode: imex.ModeHostManaged, Isolation: imex.IsolationIMEXDomain}, nvapi.ComputeDomainStatusReady},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			m := &ComputeDomainManager{config: &ManagerConfig{imexConfig: tc.mode}}
			cd := &nvapi.ComputeDomain{}
			cd.UID = types.UID(name)
			cd.Spec.NumNodes = 2
			cd.Status.Status = tc.status

			before := computeDomainInfoValue(t, tc.status)
			require.NoError(t, m.updateGlobalStatus(context.Background(), cd))
			require.NoError(t, m.updateGlobalStatus(context.Background(), cd)) // resync: no double count
			require.Equal(t, before+1, computeDomainInfoValue(t, tc.status))
		})
	}
}

// NewComputeDomainManager only stores clientsets on the informer factories it
// builds (it never calls them synchronously), so a zero-value ClientSets is
// sufficient here: these tests only assert on which sub-managers get
// constructed, not on their runtime behavior.

func TestNewComputeDomainManagerHostManagedSkipsDaemonAndNodeManagers(t *testing.T) {
	config := &ManagerConfig{
		imexConfig: imex.Config{Mode: imex.ModeHostManaged, Isolation: imex.IsolationIMEXDomain},
	}

	m := NewComputeDomainManager(config)

	// Host-managed IMEX never creates DaemonSets or ComputeDomain node
	// labels, so this machinery (including the DaemonSet manager's nested
	// ComputeDomainClique/status tracking) must not even be constructed.
	require.Nil(t, m.daemonSetManager, "daemonSetManager must not be constructed under host-managed IMEX")
	require.Nil(t, m.nodeManager, "nodeManager must not be constructed under host-managed IMEX")
	require.NotNil(t, m.resourceClaimTemplateManager, "resourceClaimTemplateManager is still needed to manage the workload ResourceClaimTemplate")
}

func TestNewComputeDomainManagerDriverManagedConstructsDaemonAndNodeManagers(t *testing.T) {
	config := &ManagerConfig{
		imexConfig: imex.Config{Mode: imex.ModeDriverManaged},
	}

	m := NewComputeDomainManager(config)

	require.NotNil(t, m.daemonSetManager)
	require.NotNil(t, m.nodeManager)
	require.NotNil(t, m.resourceClaimTemplateManager)
}
