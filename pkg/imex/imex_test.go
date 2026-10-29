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
	all := []featuregate.Feature{featuregates.HostManagedIMEXDaemon, featuregates.ComputeDomainChannelReservations}
	tests := map[string]struct {
		config  Config
		gates   []featuregate.Feature
		wantErr string
	}{
		"driverManaged defaults": {
			config: Config{Mode: ModeDriverManaged},
		},
		"driverManaged per ComputeDomain with domain isolation": {
			config: Config{Mode: ModeDriverManaged, Scope: ScopeComputeDomain, Isolation: IsolationIMEXDomain},
		},
		"driverManaged per ComputeDomain with channel isolation": {
			config: Config{Mode: ModeDriverManaged, Scope: ScopeComputeDomain, Isolation: IsolationIMEXChannel},
			gates:  []featuregate.Feature{featuregates.ComputeDomainChannelReservations},
		},
		"channel isolation requires channel reservations": {
			config:  Config{Mode: ModeDriverManaged, Isolation: IsolationIMEXChannel},
			wantErr: "ComputeDomainChannelReservations",
		},
		"driverManaged cluster scope is not implemented yet": {
			config:  Config{Mode: ModeDriverManaged, Scope: ScopeCluster, Isolation: IsolationIMEXChannel},
			gates:   all,
			wantErr: "not supported yet",
		},
		"hostManaged requires the feature gate": {
			config:  Config{Mode: ModeHostManaged},
			wantErr: "HostManagedIMEXDaemon",
		},
		"hostManaged defaults to cluster scope": {
			config: Config{Mode: ModeHostManaged, Isolation: IsolationIMEXDomain},
			gates:  []featuregate.Feature{featuregates.HostManagedIMEXDaemon},
		},
		"hostManaged with channel isolation": {
			config: Config{Mode: ModeHostManaged, Scope: ScopeCluster, Isolation: IsolationIMEXChannel},
			gates:  all,
		},
		"hostManaged per ComputeDomain": {
			config:  Config{Mode: ModeHostManaged, Scope: ScopeComputeDomain},
			gates:   all,
			wantErr: "shared by all ComputeDomains",
		},
		"unknown isolation": {
			config:  Config{Mode: ModeDriverManaged, Isolation: "bogus"},
			gates:   all,
			wantErr: "unknown imex.isolation",
		},
		"unknown scope": {
			config:  Config{Mode: ModeDriverManaged, Scope: "bogus"},
			gates:   all,
			wantErr: "unknown imex.scope",
		},
		"unknown mode": {
			config:  Config{Mode: "bogus"},
			gates:   all,
			wantErr: "unknown imex.mode",
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

func TestConfigEffectiveHostManaged(t *testing.T) {
	require.True(t, Config{Mode: ModeHostManaged}.EffectiveHostManaged())
	require.False(t, Config{Mode: ModeDriverManaged}.EffectiveHostManaged())
	require.False(t, Config{}.EffectiveHostManaged())
}

func TestConfigScope(t *testing.T) {
	host := Config{Mode: ModeHostManaged, Isolation: IsolationIMEXChannel}
	require.True(t, host.SharedIMEXDaemons())
	require.Equal(t, ScopeCluster, host.EffectiveScope())
	require.Equal(t, "mode=hostManaged scope=cluster isolation=channel", host.String())

	perComputeDomain := Config{Mode: ModeDriverManaged, Isolation: IsolationIMEXChannel}
	require.False(t, perComputeDomain.SharedIMEXDaemons(), "channel isolation alone keeps per-ComputeDomain daemons")
	require.Equal(t, ScopeComputeDomain, perComputeDomain.EffectiveScope())
}
