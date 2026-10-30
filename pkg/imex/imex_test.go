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
)

func TestConfigValidate(t *testing.T) {
	all := Gates{HostManagedIMEXDaemon: true, ComputeDomainChannelReservations: true, ComputeDomainDaemonSet: true}
	tests := map[string]struct {
		config  Config
		gates   Gates
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
			gates:  Gates{ComputeDomainChannelReservations: true},
		},
		"channel isolation requires channel reservations": {
			config:  Config{Mode: ModeDriverManaged, Isolation: IsolationIMEXChannel},
			gates:   Gates{ComputeDomainDaemonSet: true},
			wantErr: "ComputeDomainChannelReservations",
		},
		"driverManaged cluster scope runs static daemons": {
			config: Config{Mode: ModeDriverManaged, Scope: ScopeCluster, Isolation: IsolationIMEXChannel},
			gates:  Gates{ComputeDomainChannelReservations: true, ComputeDomainDaemonSet: true},
		},
		"cluster scope requires the ComputeDomainDaemonSet gate": {
			config:  Config{Mode: ModeDriverManaged, Scope: ScopeCluster, Isolation: IsolationIMEXChannel},
			gates:   Gates{ComputeDomainChannelReservations: true},
			wantErr: "ComputeDomainDaemonSet",
		},
		"cluster scope requires channel isolation": {
			config:  Config{Mode: ModeDriverManaged, Scope: ScopeCluster},
			gates:   all,
			wantErr: "isolated by their channels",
		},
		"hostManaged requires the feature gate": {
			config:  Config{Mode: ModeHostManaged},
			wantErr: "HostManagedIMEXDaemon",
		},
		"hostManaged defaults to cluster scope": {
			config: Config{Mode: ModeHostManaged, Isolation: IsolationIMEXDomain},
			gates:  Gates{HostManagedIMEXDaemon: true},
		},
		"hostManaged with channel isolation": {
			config: Config{Mode: ModeHostManaged, Scope: ScopeCluster, Isolation: IsolationIMEXChannel},
			gates:  Gates{HostManagedIMEXDaemon: true, ComputeDomainChannelReservations: true},
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
			err := tc.config.Validate(tc.gates)
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

func TestConfigStaticDaemon(t *testing.T) {
	static := Config{Mode: ModeDriverManaged, Scope: ScopeCluster, Isolation: IsolationIMEXChannel}
	require.True(t, static.StaticDaemon())
	require.True(t, static.SharedIMEXDomain())
	require.Equal(t, "mode=driverManaged scope=cluster isolation=channel", static.String())

	host := Config{Mode: ModeHostManaged, Isolation: IsolationIMEXChannel}
	require.False(t, host.StaticDaemon())
	require.True(t, host.SharedIMEXDomain())
	require.Equal(t, ScopeCluster, host.EffectiveScope())

	perComputeDomain := Config{Mode: ModeDriverManaged, Isolation: IsolationIMEXChannel}
	require.False(t, perComputeDomain.StaticDaemon(), "channel isolation alone keeps per-ComputeDomain daemons")
	require.False(t, perComputeDomain.SharedIMEXDomain())
	require.Equal(t, ScopeComputeDomain, perComputeDomain.EffectiveScope())
}
