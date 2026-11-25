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
	"os/exec"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func TestCheckIMEXReadiness(t *testing.T) {
	// An empty PATH makes an attempted readiness probe fail deterministically.
	t.Setenv("PATH", t.TempDir())
	previous := featuregates.Enabled(featuregates.NodeLocalFabricIPC)
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
			string(featuregates.NodeLocalFabricIPC): previous,
		}))
	})

	for name, tc := range map[string]struct {
		cliqueID            string
		daemonSetUID        string
		featureGateEnabled  bool
		injectedEligibility string
		wantProbe           bool
	}{
		"no clique, no CDI value":           {},
		"no clique, feature gate disabled":  {injectedEligibility: "false"},
		"no clique, gate on but ineligible": {featureGateEnabled: true, injectedEligibility: "false"},
		"no clique, eligible":               {featureGateEnabled: true, injectedEligibility: "true", wantProbe: true},
		"no clique, use the CDI decision":   {injectedEligibility: "true", wantProbe: true},
		"static daemon without a clique":    {daemonSetUID: "daemon-set-uid", injectedEligibility: "true"},
		"native clique":                     {cliqueID: "clique-a", injectedEligibility: "false", wantProbe: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CLIQUE_ID", tc.cliqueID)
			t.Setenv("COMPUTE_DOMAIN_DAEMONSET_UID", tc.daemonSetUID)
			t.Setenv("FEATURE_GATES", "NodeLocalFabricIPC="+strconv.FormatBool(tc.featureGateEnabled))
			t.Setenv("NODE_LOCAL_FABRIC_IPC_ENABLED", tc.injectedEligibility)

			app := newApp()
			// Kubernetes logging is configured once per process.
			app.Before = nil
			err := app.Run([]string{"compute-domain-daemon", "check"})
			if tc.wantProbe {
				assert.ErrorIs(t, err, exec.ErrNotFound)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
