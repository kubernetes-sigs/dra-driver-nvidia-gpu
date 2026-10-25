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
	"context"
	"os/exec"
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
		cliqueID  string
		enabled   bool
		wantProbe bool
	}{
		"no clique, gate disabled": {},
		"no clique, gate enabled":  {enabled: true, wantProbe: true},
		"clique, gate disabled":    {cliqueID: "clique-a", wantProbe: true},
		"clique, gate enabled":     {cliqueID: "clique-a", enabled: true, wantProbe: true},
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
				string(featuregates.NodeLocalFabricIPC): tc.enabled,
			}))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := check(ctx, cancel, &Flags{cliqueID: tc.cliqueID})
			if tc.wantProbe {
				assert.ErrorIs(t, err, exec.ErrNotFound)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
