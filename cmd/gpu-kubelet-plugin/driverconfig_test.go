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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
)

const containerProfile = `
apiVersion: resource.nvidia.com/v1beta1
kind: GpuDriverConfig
advertisement: {gpu: true, mig: true}
`

// newDriverConfigTestEnv writes profiles into a temporary config directory and
// returns it with a fake client holding node-a with the given labels.
func newDriverConfigTestEnv(t *testing.T, labels map[string]string, profiles map[string]string) (string, *k8sfake.Clientset) {
	t.Helper()
	directory := t.TempDir()
	for name, contents := range profiles {
		require.NoError(t, os.WriteFile(filepath.Join(directory, name), []byte(contents), 0600))
	}
	client := k8sfake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: labels},
	})
	return directory, client
}

func TestDriverConfigAdvertises(t *testing.T) {
	tests := map[string]struct {
		config *DriverConfig
		want   map[string]bool
	}{
		"nil profile allows every type": {
			want: map[string]bool{GpuDeviceType: true, MigStaticDeviceType: true, MigDynamicDeviceType: true, VfioDeviceType: true},
		},
		"mig covers static and dynamic MIG": {
			config: &DriverConfig{Advertisement: configapi.GpuAdvertisement{Gpu: true, Mig: true}},
			want:   map[string]bool{GpuDeviceType: true, MigStaticDeviceType: true, MigDynamicDeviceType: true, VfioDeviceType: false},
		},
		"vfio only": {
			config: &DriverConfig{Advertisement: configapi.GpuAdvertisement{Vfio: true}},
			want:   map[string]bool{GpuDeviceType: false, MigStaticDeviceType: false, MigDynamicDeviceType: false, VfioDeviceType: true},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			for deviceType, want := range tc.want {
				assert.Equal(t, want, tc.config.advertises(deviceType), deviceType)
			}
		})
	}
}

func TestResolveDriverConfig(t *testing.T) {
	tests := map[string]struct {
		labels            map[string]string
		defaultProfile    string
		profiles          map[string]string
		wantProfile       string
		wantAdvertisement *configapi.GpuAdvertisement
		wantErr           string
	}{
		"node label overrides default": {
			labels:            map[string]string{driverConfigNodeLabel: "container"},
			defaultProfile:    "missing",
			profiles:          map[string]string{"container": containerProfile},
			wantProfile:       "container",
			wantAdvertisement: &configapi.GpuAdvertisement{Gpu: true, Mig: true},
		},
		"default applies to unlabeled node": {
			defaultProfile:    "container",
			profiles:          map[string]string{"container": containerProfile},
			wantProfile:       "container",
			wantAdvertisement: &configapi.GpuAdvertisement{Gpu: true, Mig: true},
		},
		"no label and no default applies no profile": {},
		"empty label fails closed": {
			labels:  map[string]string{driverConfigNodeLabel: ""},
			wantErr: "must not be empty",
		},
		"label names missing profile": {
			labels:  map[string]string{driverConfigNodeLabel: "missing"},
			wantErr: `read driver config profile "missing"`,
		},
		"default names missing profile": {
			defaultProfile: "missing",
			wantErr:        `read driver config profile "missing"`,
		},
		"profile name escaping the config directory": {
			defaultProfile: "../container",
			wantErr:        `invalid driver config profile "../container"`,
		},
		"other kind": {
			defaultProfile: "container",
			profiles: map[string]string{"container": `
apiVersion: resource.nvidia.com/v1beta1
kind: GpuConfig
`},
			wantErr: "expected kind GpuDriverConfig",
		},
		"unknown field fails strict decoding": {
			defaultProfile: "container",
			profiles: map[string]string{"container": `
apiVersion: resource.nvidia.com/v1beta1
kind: GpuDriverConfig
advertisement: {gpu: true}
sharing: {}
`},
			wantErr: `unknown field "sharing"`,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			directory, client := newDriverConfigTestEnv(t, tc.labels, tc.profiles)

			config, profile, err := resolveDriverConfigFromDirectory(context.Background(), client, "node-a", directory, tc.defaultProfile)

			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantProfile, profile)
			if tc.wantAdvertisement == nil {
				assert.Nil(t, config)
				return
			}
			require.NotNil(t, config)
			assert.Equal(t, *tc.wantAdvertisement, config.Advertisement)
		})
	}
}

func TestNewRuntimeConfig(t *testing.T) {
	tests := map[string]struct {
		gateEnabled       bool
		labels            map[string]string
		defaultConfig     string
		wantAdvertisement *configapi.GpuAdvertisement
		wantErr           string
	}{
		"gate enabled resolves the node profile": {
			gateEnabled:       true,
			labels:            map[string]string{driverConfigNodeLabel: "container"},
			wantAdvertisement: &configapi.GpuAdvertisement{Gpu: true, Mig: true},
		},
		"gate enabled without selection applies no profile": {
			gateEnabled: true,
		},
		"gate enabled reports resolution errors": {
			gateEnabled:   true,
			defaultConfig: "missing",
			wantErr:       `resolve driver config: read driver config profile "missing"`,
		},
		"gate disabled ignores the default": {
			labels:        map[string]string{driverConfigNodeLabel: "container"},
			defaultConfig: "container",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			directory, client := newDriverConfigTestEnv(t, tc.labels, map[string]string{"container": containerProfile})
			clientSets := pkgflags.ClientSets{Core: client}
			if !tc.gateEnabled {
				// The disabled path must not depend on the API server.
				clientSets = pkgflags.ClientSets{}
			}

			config, err := newRuntimeConfig(
				context.Background(),
				&Flags{nodeName: "node-a", defaultConfig: tc.defaultConfig},
				clientSets,
				directory,
				tc.gateEnabled,
			)

			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			if tc.wantAdvertisement == nil {
				assert.Nil(t, config.driverConfig)
				return
			}
			require.NotNil(t, config.driverConfig)
			assert.Equal(t, *tc.wantAdvertisement, config.driverConfig.Advertisement)
		})
	}
}
