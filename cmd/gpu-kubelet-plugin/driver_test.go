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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	coreclientset "k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

// fakeClientWithVersion returns a fake core clientset whose discovery endpoint
// reports gitVersion as the API server version.
func fakeClientWithVersion(t *testing.T, gitVersion string) coreclientset.Interface {
	t.Helper()
	client := k8sfake.NewSimpleClientset()
	fd, ok := client.Discovery().(*fakediscovery.FakeDiscovery)
	require.True(t, ok, "expected fake discovery client")
	fd.FakedServerVersion = &version.Info{GitVersion: gitVersion}
	return client
}

func TestGetAPIServerVersion(t *testing.T) {
	tests := map[string]struct {
		gitVersion  string
		wantVersion string
		wantErr     bool
	}{
		"valid with v prefix":       {gitVersion: "v1.35.2", wantVersion: "1.35.2"},
		"valid without prefix":      {gitVersion: "1.34.0", wantVersion: "1.34.0"},
		"valid with build metadata": {gitVersion: "v1.35.0-alpha.1", wantVersion: "1.35.0-alpha.1"},
		"unparseable":               {gitVersion: "not-a-version", wantErr: true},
		"empty":                     {gitVersion: "", wantErr: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			client := fakeClientWithVersion(t, tc.gitVersion)
			got, err := getAPIServerVersion(client)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantVersion, got.String())
		})
	}
}

func TestShouldUseSplitResourceSlices(t *testing.T) {
	tests := map[string]struct {
		gitVersion string
		wantSplit  bool
		wantErr    bool
	}{
		"1.33 combined":           {gitVersion: "v1.33.4", wantSplit: false},
		"1.34 combined":           {gitVersion: "v1.34.5", wantSplit: false},
		"1.35.0 split (boundary)": {gitVersion: "v1.35.0", wantSplit: true},
		"1.35.2 split":            {gitVersion: "v1.35.2", wantSplit: true},
		"1.36 split":              {gitVersion: "v1.36.0", wantSplit: true},
		"unparseable errors":      {gitVersion: "garbage", wantErr: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			client := fakeClientWithVersion(t, tc.gitVersion)
			gotSplit, err := shouldUseSplitResourceSlices(client)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantSplit, gotSplit)
		})
	}
}

// newTestDriverWithGPUs builds a driver whose state announces the given GPUs,
// keyed by PCI bus ID. GPUs carry no MIG capacity data, so PartSharedCounterSets
// returns nil (the Ampere-MIG-disabled / vGPU case) and no CounterSets are
// emitted.
func newTestDriverWithGPUs(useSplit bool, gpus map[PCIBusID]*GpuInfo) *driver {
	allocatablesMap := make(map[PCIBusID]AllocatableDevices, len(gpus))
	for pciBusID, gpu := range gpus {
		allocatablesMap[pciBusID] = AllocatableDevices{
			gpu.CanonicalName(): &AllocatableDevice{Gpu: gpu},
		}
	}
	return &driver{
		useSplitResourceSlices: useSplit,
		state: &DeviceState{
			perGPUAllocatable: &PerGPUAllocatableDevices{allocatablesMap: allocatablesMap},
		},
	}
}

func TestGenerateDriverResources(t *testing.T) {
	// Both GPUs need valid semver fields; Attributes() calls semver.MustParse
	// on the CUDA/driver version strings. newTestGpuInfo provides them.
	gpu1 := newTestGpuInfo(nil)
	gpu1.minor = 1 // -> "gpu-1"
	gpu1.UUID = "GPU-test-1"
	gpus := map[PCIBusID]*GpuInfo{
		"0000:01:00.0": newTestGpuInfo(nil), // minor 0 -> "gpu-0"
		"0000:02:00.0": gpu1,
	}

	t.Run("split emits shared-counters slice plus one slice per GPU", func(t *testing.T) {
		d := newTestDriverWithGPUs(true, gpus)
		res := d.GenerateDriverResources("node-a")

		pool, ok := res.Pools["node-a"]
		require.True(t, ok, "expected pool keyed by node name")
		// 1 shared-counters slice + 2 device slices.
		require.Len(t, pool.Slices, 3)

		// First slice is the shared-counters slice (no devices).
		assert.Empty(t, pool.Slices[0].Devices)

		// Remaining slices each carry exactly one device, ordered by PCI bus ID.
		require.Len(t, pool.Slices[1].Devices, 1)
		require.Len(t, pool.Slices[2].Devices, 1)
		assert.Equal(t, "gpu-0", pool.Slices[1].Devices[0].Name)
		assert.Equal(t, "gpu-1", pool.Slices[2].Devices[0].Name)
	})

	t.Run("combined emits one slice per GPU", func(t *testing.T) {
		d := newTestDriverWithGPUs(false, gpus)
		res := d.GenerateDriverResources("node-a")

		pool, ok := res.Pools["node-a"]
		require.True(t, ok)
		require.Len(t, pool.Slices, 2)
		assert.Equal(t, "gpu-0", pool.Slices[0].Devices[0].Name)
		assert.Equal(t, "gpu-1", pool.Slices[1].Devices[0].Name)
	})
}

func TestGenerateDriverResourcesAppliesDriverConfig(t *testing.T) {
	pciBusID := "0000:01:00.0"
	parent := newPartTestGpu(PartCapacityMap{"multiprocessors": intcap(132)}, 8)
	parent.pciBusID = pciBusID
	mig := newPartTestMigSpec(parent, 0, 1)
	counterSet := parent.GetSharedCounterSetName()

	type wantSlice struct {
		counterSets []string
		devices     []string
	}
	tests := map[string]struct {
		useSplit       bool
		advertisement  configapi.GpuAdvertisement
		want           []wantSlice
		wantAdvertised int
	}{
		"combined, MIG only keeps the parent counter set": {
			advertisement:  configapi.GpuAdvertisement{Mig: true},
			want:           []wantSlice{{counterSets: []string{counterSet}, devices: []string{mig.CanonicalName()}}},
			wantAdvertised: 1,
		},
		"split, MIG only keeps the parent counter set": {
			useSplit:      true,
			advertisement: configapi.GpuAdvertisement{Mig: true},
			want: []wantSlice{
				{counterSets: []string{counterSet}},
				{devices: []string{mig.CanonicalName()}},
			},
			wantAdvertised: 1,
		},
		"combined, every device excluded publishes no slice": {
			advertisement: configapi.GpuAdvertisement{Vfio: true},
		},
		"split, every device excluded publishes only an empty counters slice": {
			useSplit:      true,
			advertisement: configapi.GpuAdvertisement{Vfio: true},
			want:          []wantSlice{{}},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			config := &Config{
				flags:        &Flags{},
				driverConfig: &DriverConfig{Advertisement: tc.advertisement},
			}
			perGPUAllocatable := &PerGPUAllocatableDevices{
				allocatablesMap: map[PCIBusID]AllocatableDevices{pciBusID: {
					parent.CanonicalName(): {Gpu: parent},
					mig.CanonicalName():    {MigDynamic: mig},
				}},
			}
			perGPUAllocatable.ApplyDriverConfig(config.driverConfig)
			d := &driver{
				useSplitResourceSlices: tc.useSplit,
				state: &DeviceState{
					config:            config,
					perGPUAllocatable: perGPUAllocatable,
				},
			}

			slices := d.GenerateDriverResources("node-a").Pools["node-a"].Slices

			require.Len(t, slices, len(tc.want))
			for i, want := range tc.want {
				var counterSets []string
				for _, cs := range slices[i].SharedCounters {
					counterSets = append(counterSets, cs.Name)
				}
				assert.Equal(t, want.counterSets, counterSets)
				var devices []string
				for _, dev := range slices[i].Devices {
					devices = append(devices, dev.Name)
					assert.Equal(t, MigStaticDeviceType, *dev.Attributes["type"].StringValue)
					require.Len(t, dev.ConsumesCounters, 1)
					assert.Equal(t, counterSet, dev.ConsumesCounters[0].CounterSet)
				}
				assert.Equal(t, want.devices, devices)
			}
			assert.Len(t, perGPUAllocatable.GetAllDevices(), tc.wantAdvertised)
			assert.NotNil(t, perGPUAllocatable.GetDeviceForUnprepare(parent.CanonicalName()))
		})
	}
}

func TestShutdownNilReceiver(t *testing.T) {
	var d *driver
	assert.NoError(t, d.Shutdown())
}

func TestPrepareResourceClaimsEmpty(t *testing.T) {
	d := &driver{}
	results, err := d.PrepareResourceClaims(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, results)
	assert.NotNil(t, results)
}

func TestUnprepareResourceClaimsEmpty(t *testing.T) {
	d := &driver{}
	results, err := d.UnprepareResourceClaims(context.Background(), []kubeletplugin.NamespacedObject{})
	require.NoError(t, err)
	assert.Empty(t, results)
	assert.NotNil(t, results)
}

func TestWatchHealthStatusUnsupported(t *testing.T) {
	d := &driver{}
	err := d.WatchHealthStatus(context.Background(), make(chan kubeletplugin.DeviceHealthReport))
	assert.ErrorIs(t, err, kubeletplugin.ErrHealthNotSupported)
}
