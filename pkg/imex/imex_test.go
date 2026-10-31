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

package imex

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/component-base/featuregate"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// setGates enables the given IMEX feature gates and disables the others for
// the duration of the test.
func setGates(t *testing.T, enabled ...featuregate.Feature) {
	t.Helper()
	gates := map[string]bool{
		string(featuregates.HostManagedIMEXDaemon):            false,
		string(featuregates.ComputeDomainChannelReservations): false,
	}
	restore := make(map[string]bool, len(gates))
	for name := range gates {
		restore[name] = featuregates.Enabled(featuregate.Feature(name))
	}
	for _, gate := range enabled {
		gates[string(gate)] = true
	}
	require.NoError(t, featuregates.FeatureGates().SetFromMap(gates))
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(restore))
	})
}

func TestConfigValidate(t *testing.T) {
	tests := map[string]struct {
		config  Config
		gates   []featuregate.Feature
		wantErr string
	}{
		"perComputeDomain": {
			config: Config{DaemonLifecycle: DaemonLifecyclePerComputeDomain},
		},
		"hostManaged requires the feature gate": {
			config:  Config{DaemonLifecycle: DaemonLifecycleHostManaged},
			wantErr: "HostManagedIMEXDaemon",
		},
		"hostManaged": {
			config: Config{DaemonLifecycle: DaemonLifecycleHostManaged},
			gates:  []featuregate.Feature{featuregates.HostManagedIMEXDaemon},
		},
		"unknown lifecycle": {
			config:  Config{DaemonLifecycle: "bogus"},
			wantErr: "unknown imex.daemonLifecycle",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			setGates(t, tc.gates...)
			err := tc.config.Validate()
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestResolveConfig(t *testing.T) {
	tests := map[string]struct {
		lifecycle, mode, isolation string
		want                       DaemonLifecycle
		wantWarnings               int
		wantErr                    string
	}{
		"defaults to perComputeDomain": {
			want: DaemonLifecyclePerComputeDomain,
		},
		"daemonLifecycle": {
			lifecycle: "hostManaged",
			want:      DaemonLifecycleHostManaged,
		},
		"v0.5 driverManaged defaults": {
			mode: "driverManaged", isolation: "domain",
			want:         DaemonLifecyclePerComputeDomain,
			wantWarnings: 2,
		},
		"v0.5 hostManaged": {
			mode: "hostManaged", isolation: "domain",
			want:         DaemonLifecycleHostManaged,
			wantWarnings: 2,
		},
		"daemonLifecycle wins over the driverManaged default": {
			lifecycle: "hostManaged", mode: "driverManaged",
			want:         DaemonLifecycleHostManaged,
			wantWarnings: 1,
		},
		"hostManaged mode contradicts another lifecycle": {
			lifecycle: "perComputeDomain", mode: "hostManaged",
			wantErr: "contradicts",
		},
		"unknown mode": {
			mode:    "bogus",
			wantErr: "unknown imex.mode",
		},
		"isolation is ignored": {
			isolation:    "channel",
			want:         DaemonLifecyclePerComputeDomain,
			wantWarnings: 1,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			config, warnings, err := ResolveConfig(tc.lifecycle, tc.mode, tc.isolation)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, config.DaemonLifecycle)
			require.Len(t, warnings, tc.wantWarnings, warnings)
		})
	}
}

func TestConfigSharedIMEXDaemons(t *testing.T) {
	host := Config{DaemonLifecycle: DaemonLifecycleHostManaged}
	require.True(t, host.EffectiveHostManaged())
	require.True(t, host.SharedIMEXDaemons())
	require.Equal(t, "daemonLifecycle=hostManaged", host.String())

	perComputeDomain := Config{DaemonLifecycle: DaemonLifecyclePerComputeDomain}
	require.False(t, perComputeDomain.EffectiveHostManaged())
	require.False(t, perComputeDomain.SharedIMEXDaemons())
	require.False(t, Config{}.EffectiveHostManaged())
}
