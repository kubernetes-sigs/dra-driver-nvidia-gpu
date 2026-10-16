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

	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
)

// TestDriverConfigValidate locks down the versioned profile schema and public device-type allow list.
func TestDriverConfigValidate(t *testing.T) {
	tests := map[string]struct {
		config  *DriverConfig
		wantErr bool
	}{
		"all device types": {
			config: &DriverConfig{
				Version: driverConfigVersion,
				GPU:     &GPUDriverConfig{AdvertisedDeviceTypes: []string{GpuDeviceType, MigStaticDeviceType, VfioDeviceType}},
			},
		},
		"MIG uses public device type": {
			config: &DriverConfig{
				Version: driverConfigVersion,
				GPU:     &GPUDriverConfig{AdvertisedDeviceTypes: []string{MigStaticDeviceType}},
			},
		},
		"missing version": {
			config:  &DriverConfig{GPU: &GPUDriverConfig{AdvertisedDeviceTypes: []string{GpuDeviceType}}},
			wantErr: true,
		},
		"missing GPU config": {
			config:  &DriverConfig{Version: driverConfigVersion},
			wantErr: true,
		},
		"empty device types": {
			config:  &DriverConfig{Version: driverConfigVersion, GPU: &GPUDriverConfig{}},
			wantErr: true,
		},
		"internal dynamic MIG type": {
			config: &DriverConfig{
				Version: driverConfigVersion,
				GPU:     &GPUDriverConfig{AdvertisedDeviceTypes: []string{MigDynamicDeviceType}},
			},
			wantErr: true,
		},
		"unknown type": {
			config: &DriverConfig{
				Version: driverConfigVersion,
				GPU:     &GPUDriverConfig{AdvertisedDeviceTypes: []string{"unknown"}},
			},
			wantErr: true,
		},
		"duplicate type": {
			config: &DriverConfig{
				Version: driverConfigVersion,
				GPU:     &GPUDriverConfig{AdvertisedDeviceTypes: []string{GpuDeviceType, GpuDeviceType}},
			},
			wantErr: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := tc.config.validate()
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestDriverConfigAdvertises covers dynamic-MIG mapping and the nil allow-all policy.
func TestDriverConfigAdvertises(t *testing.T) {
	config := &DriverConfig{
		Version: driverConfigVersion,
		GPU:     &GPUDriverConfig{AdvertisedDeviceTypes: []string{GpuDeviceType, MigStaticDeviceType}},
	}

	assert.True(t, config.advertises(GpuDeviceType))
	assert.True(t, config.advertises(MigStaticDeviceType))
	assert.True(t, config.advertises(MigDynamicDeviceType))
	assert.False(t, config.advertises(VfioDeviceType))
	assert.True(t, (*DriverConfig)(nil).advertises(VfioDeviceType))
}

// TestResolveDriverConfig covers selection precedence and strict startup failures.
func TestResolveDriverConfig(t *testing.T) {
	writeProfile := func(t *testing.T, directory, name, contents string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(directory, name), []byte(contents), 0600))
	}

	t.Run("node label selects profile", func(t *testing.T) {
		directory := t.TempDir()
		writeProfile(t, directory, "container", `
version: v1alpha1
gpu:
  advertisedDeviceTypes: [gpu, mig]
`)
		client := k8sfake.NewSimpleClientset(&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node-a",
				Labels: map[string]string{driverConfigNodeLabel: "container"},
			},
		})

		config, profile, err := resolveDriverConfigFromDirectory(
			context.Background(), client, "node-a", directory, "missing",
		)
		require.NoError(t, err)
		assert.Equal(t, "container", profile)
		assert.Equal(t, []string{GpuDeviceType, MigStaticDeviceType}, config.GPU.AdvertisedDeviceTypes)
	})

	t.Run("no profile is used when label and configured default are absent", func(t *testing.T) {
		directory := t.TempDir()
		client := k8sfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})

		config, profile, err := resolveDriverConfigFromDirectory(
			context.Background(), client, "node-a", directory, "",
		)
		require.NoError(t, err)
		assert.Nil(t, config)
		assert.Empty(t, profile)
	})

	t.Run("configured default profile is used when label is absent", func(t *testing.T) {
		directory := t.TempDir()
		writeProfile(t, directory, "container", `
version: v1alpha1
gpu:
  advertisedDeviceTypes: [gpu, mig]
`)
		client := k8sfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})

		config, profile, err := resolveDriverConfigFromDirectory(
			context.Background(), client, "node-a", directory, "container",
		)
		require.NoError(t, err)
		assert.Equal(t, "container", profile)
		assert.ElementsMatch(t, []string{GpuDeviceType, MigStaticDeviceType}, config.GPU.AdvertisedDeviceTypes)
	})

	t.Run("missing configured default profile fails", func(t *testing.T) {
		directory := t.TempDir()
		client := k8sfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})

		_, _, err := resolveDriverConfigFromDirectory(
			context.Background(), client, "node-a", directory, "missing",
		)
		require.Error(t, err)
		assert.ErrorContains(t, err, `read driver config profile "missing"`)
	})

	t.Run("explicit empty label fails closed", func(t *testing.T) {
		directory := t.TempDir()
		client := k8sfake.NewSimpleClientset(&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node-a",
				Labels: map[string]string{driverConfigNodeLabel: ""},
			},
		})

		_, _, err := resolveDriverConfigFromDirectory(
			context.Background(), client, "node-a", directory, "",
		)
		require.Error(t, err)
		assert.ErrorContains(t, err, "must not be empty")
	})

	t.Run("unknown profile fails", func(t *testing.T) {
		directory := t.TempDir()
		client := k8sfake.NewSimpleClientset(&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node-a",
				Labels: map[string]string{driverConfigNodeLabel: "missing"},
			},
		})

		_, _, err := resolveDriverConfigFromDirectory(
			context.Background(), client, "node-a", directory, "",
		)
		require.Error(t, err)
		assert.ErrorContains(t, err, `read driver config profile "missing"`)
	})

	t.Run("unknown field fails strict decoding", func(t *testing.T) {
		directory := t.TempDir()
		writeProfile(t, directory, "container", `
version: v1alpha1
gpu:
  advertisedDeviceTypes: [gpu, mig, vfio]
  sharing: {}
`)
		client := k8sfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})

		_, _, err := resolveDriverConfigFromDirectory(
			context.Background(), client, "node-a", directory, "container",
		)
		require.Error(t, err)
		assert.ErrorContains(t, err, `unknown field "sharing"`)
	})
}

// TestResolveDriverConfigAtStartupAppliesPublicationPolicy ensures the parsed profile reaches slice generation.
func TestResolveDriverConfigAtStartupAppliesPublicationPolicy(t *testing.T) {
	directory := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(directory, "container"), []byte(`
version: v1alpha1
gpu:
  advertisedDeviceTypes: [gpu, mig]
`), 0600))
	client := k8sfake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-a",
			Labels: map[string]string{driverConfigNodeLabel: "container"},
		},
	})

	runtimeConfig, err := newRuntimeConfig(
		context.Background(),
		&Flags{nodeName: "node-a", defaultConfig: "missing"},
		pkgflags.ClientSets{Core: client},
		directory,
		true,
	)
	require.NoError(t, err)
	require.NotNil(t, runtimeConfig.driverConfig)
	assert.Equal(t, []string{GpuDeviceType, MigStaticDeviceType}, runtimeConfig.driverConfig.GPU.AdvertisedDeviceTypes)

	pciBusID := "0000:01:00.0"
	gpu := newTestGpuInfo(nil)
	gpu.pciBusID = pciBusID
	vfio := &VfioDeviceInfo{
		index:    0,
		UUID:     gpu.UUID,
		PciBusID: pciBusID,
	}
	d := &driver{
		state: &DeviceState{
			config: runtimeConfig,
			perGPUAllocatable: &PerGPUAllocatableDevices{
				allocatablesMap: map[PCIBusID]AllocatableDevices{
					pciBusID: {
						gpu.CanonicalName():  {Gpu: gpu},
						vfio.CanonicalName(): {Vfio: vfio},
					},
				},
			},
		},
	}
	d.state.perGPUAllocatable.ApplyDriverConfig(runtimeConfig.driverConfig)

	devices := d.state.perGPUAllocatable.GetAllDevices()
	require.Len(t, devices, 1)
	assert.Contains(t, devices, gpu.CanonicalName())
	assert.NotContains(t, devices, vfio.CanonicalName())
}

// TestResolveDriverConfigAtStartupRejectsMissingDefault ensures explicit defaults fail with startup context.
func TestResolveDriverConfigAtStartupRejectsMissingDefault(t *testing.T) {
	client := k8sfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})

	_, err := newRuntimeConfig(
		context.Background(),
		&Flags{nodeName: "node-a", defaultConfig: "missing"},
		pkgflags.ClientSets{Core: client},
		t.TempDir(),
		true,
	)
	require.Error(t, err)
	assert.ErrorContains(t, err, `resolve driver config: read driver config profile "missing"`)
}

// TestResolveDriverConfigAtStartupWithoutSelectionReturnsNoPolicy preserves release-wide settings.
func TestResolveDriverConfigAtStartupWithoutSelectionReturnsNoPolicy(t *testing.T) {
	client := k8sfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})
	runtimeConfig, err := newRuntimeConfig(
		context.Background(),
		&Flags{nodeName: "node-a"},
		pkgflags.ClientSets{Core: client},
		t.TempDir(),
		true,
	)
	require.NoError(t, err)
	require.Nil(t, runtimeConfig.driverConfig)
}

// TestResolveDriverConfigAtStartupDisabled ensures gate-off startup retains a nil allow-all policy.
func TestResolveDriverConfigAtStartupDisabled(t *testing.T) {
	runtimeConfig, err := newRuntimeConfig(
		context.Background(),
		&Flags{nodeName: "node-a", defaultConfig: "container"},
		pkgflags.ClientSets{},
		t.TempDir(),
		false,
	)
	require.NoError(t, err)
	assert.Nil(t, runtimeConfig.driverConfig)
}
