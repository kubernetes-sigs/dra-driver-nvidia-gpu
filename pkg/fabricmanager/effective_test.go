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

package fabricmanager

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func TestCheckFabricManagerSupport(t *testing.T) {
	previous := checkFMSupport.Load()
	t.Cleanup(func() { checkFMSupport.Store(previous) })

	moduleRoot := func(hostRoot string) string { return filepath.Join(hostRoot, "sys", "module") }
	addNVSwitch := func(t *testing.T, hostRoot string) {
		devicesDir := filepath.Join(hostRoot, "proc", "driver", "nvidia-nvswitch", "devices")
		require.NoError(t, os.MkdirAll(devicesDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(devicesDir, "0000:00:00.0"), []byte("device"), 0o644))
	}

	previousGate := featuregates.Enabled(featuregates.FabricManagerPartitioning)
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
			string(featuregates.FabricManagerPartitioning): previousGate,
		}))
	})

	for name, tc := range map[string]struct {
		gateDisabled bool
		setup        func(t *testing.T, hostRoot string)
		wantEnabled  bool
		wantError    bool
	}{
		"fabric present and vGPU module absent": {
			setup: func(t *testing.T, hostRoot string) {
				require.NoError(t, os.MkdirAll(moduleRoot(hostRoot), 0o755))
				addNVSwitch(t, hostRoot)
			},
			wantEnabled: true,
		},
		"feature gate disabled": {
			gateDisabled: true,
			setup: func(t *testing.T, hostRoot string) {
				require.NoError(t, os.MkdirAll(moduleRoot(hostRoot), 0o755))
				addNVSwitch(t, hostRoot)
			},
		},
		"feature gate disabled skips host checks": {
			gateDisabled: true,
			setup:        func(t *testing.T, hostRoot string) {},
		},
		"no fabric": {
			setup: func(t *testing.T, hostRoot string) {
				require.NoError(t, os.MkdirAll(moduleRoot(hostRoot), 0o755))
			},
		},
		"vGPU module loaded": {
			setup: func(t *testing.T, hostRoot string) {
				require.NoError(t, os.MkdirAll(filepath.Join(moduleRoot(hostRoot), vgpuHostModule), 0o755))
				addNVSwitch(t, hostRoot)
			},
		},
		"host module directory missing": {
			setup:     addNVSwitch,
			wantError: true,
		},
		"host module path is not a directory": {
			setup: func(t *testing.T, hostRoot string) {
				require.NoError(t, os.MkdirAll(filepath.Dir(moduleRoot(hostRoot)), 0o755))
				require.NoError(t, os.WriteFile(moduleRoot(hostRoot), nil, 0o644))
			},
			wantError: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
				string(featuregates.FabricManagerPartitioning): !tc.gateDisabled,
			}))
			checkFMSupport.Store(!tc.wantEnabled)
			hostRoot := t.TempDir()
			tc.setup(t, hostRoot)

			err := CheckFabricManagerSupport(hostRoot)
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantEnabled, SupportsFabricManagerPartitioning())
		})
	}
}
