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
	"testing"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// expectedOverhead describes the expected parsed overhead for one resource;
// an empty string means the corresponding pointer must be nil.
type expectedOverhead struct {
	perPod       string
	perContainer string
}

func requireOverheadQuantity(t *testing.T, expected string, actual *resource.Quantity) {
	t.Helper()

	if expected == "" {
		require.Nil(t, actual)
		return
	}
	require.NotNil(t, actual)
	require.True(t, resource.MustParse(expected).Equal(*actual))
}

func requireOverheads(t *testing.T, expected map[corev1.ResourceName]expectedOverhead, actual map[corev1.ResourceName]resourceapi.NodeAllocatableResource) {
	t.Helper()

	if expected == nil {
		require.Nil(t, actual)
		return
	}
	require.Len(t, actual, len(expected))
	for name, want := range expected {
		entry, ok := actual[name]
		require.True(t, ok, "expected a NodeAllocatableResources entry for %q", name)
		require.Nil(t, entry.Mapping)
		require.NotNil(t, entry.Overhead)
		requireOverheadQuantity(t, want.perPod, entry.Overhead.PerPod)
		requireOverheadQuantity(t, want.perContainer, entry.Overhead.PerContainer)
	}
}

func TestParseNodeAllocatableOverheadFlags(t *testing.T) {
	tests := []struct {
		name              string
		featureGate       bool
		gpu               nodeAllocatableOverheadValues
		mig               nodeAllocatableOverheadValues
		vfio              nodeAllocatableOverheadValues
		expectedOverheads map[string]map[corev1.ResourceName]expectedOverhead
		expectedErr       string
	}{
		{
			name:        "nothing configured",
			featureGate: false,
		},
		{
			name:        "feature gate enabled but nothing configured",
			featureGate: true,
		},
		{
			name:        "values set with feature gate disabled are rejected",
			featureGate: false,
			gpu:         nodeAllocatableOverheadValues{memoryPerPod: "100Mi"},
			expectedErr: "node-allocatable overhead flags require feature gate NodeAllocatableResources to be enabled",
		},
		{
			name:        "gpu memory per-pod only",
			featureGate: true,
			gpu:         nodeAllocatableOverheadValues{memoryPerPod: "100Mi"},
			expectedOverheads: map[string]map[corev1.ResourceName]expectedOverhead{
				overheadClassGpu: {corev1.ResourceMemory: {perPod: "100Mi"}},
			},
		},
		{
			name:        "distinct values per class",
			featureGate: true,
			gpu:         nodeAllocatableOverheadValues{memoryPerPod: "1Gi", cpuPerPod: "500m"},
			mig:         nodeAllocatableOverheadValues{memoryPerPod: "128Mi", memoryPerContainer: "32Mi"},
			vfio:        nodeAllocatableOverheadValues{cpuPerContainer: "50m"},
			expectedOverheads: map[string]map[corev1.ResourceName]expectedOverhead{
				overheadClassGpu: {
					corev1.ResourceMemory: {perPod: "1Gi"},
					corev1.ResourceCPU:    {perPod: "500m"},
				},
				overheadClassMig: {
					corev1.ResourceMemory: {perPod: "128Mi", perContainer: "32Mi"},
				},
				overheadClassVfio: {
					corev1.ResourceCPU: {perContainer: "50m"},
				},
			},
		},
		{
			name:        "class with only zero values publishes nothing",
			featureGate: true,
			gpu:         nodeAllocatableOverheadValues{memoryPerPod: "100Mi"},
			mig:         nodeAllocatableOverheadValues{memoryPerPod: "0", cpuPerPod: "0m"},
			expectedOverheads: map[string]map[corev1.ResourceName]expectedOverhead{
				overheadClassGpu: {corev1.ResourceMemory: {perPod: "100Mi"}},
			},
		},
		{
			name:        "all-zero values yield no overheads",
			featureGate: true,
			gpu:         nodeAllocatableOverheadValues{memoryPerPod: "0", cpuPerPod: "0"},
		},
		{
			name:        "whitespace-only value is treated as unset",
			featureGate: true,
			mig:         nodeAllocatableOverheadValues{cpuPerContainer: "   "},
		},
		{
			name:        "negative value is rejected with class-scoped flag name",
			featureGate: true,
			mig:         nodeAllocatableOverheadValues{memoryPerPod: "-100Mi"},
			expectedErr: "invalid value for --node-allocatable-mig-memory-overhead-per-pod: \"-100Mi\" (must not be negative)",
		},
		{
			name:        "unparseable value is rejected with class-scoped flag name",
			featureGate: true,
			vfio:        nodeAllocatableOverheadValues{cpuPerPod: "lots"},
			expectedErr: "invalid value for --node-allocatable-vfio-cpu-overhead-per-pod: \"lots\"",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
				string(featuregates.NodeAllocatableResources): tc.featureGate,
			}))

			flags := &Flags{
				gpuNodeAllocatableOverhead:  tc.gpu,
				migNodeAllocatableOverhead:  tc.mig,
				vfioNodeAllocatableOverhead: tc.vfio,
			}

			overheads, err := parseNodeAllocatableOverheadFlags(flags)

			if tc.expectedErr != "" {
				require.ErrorContains(t, err, tc.expectedErr)
				require.Nil(t, overheads)
				return
			}
			require.NoError(t, err)
			if tc.expectedOverheads == nil {
				require.Nil(t, overheads)
				return
			}
			require.Len(t, overheads, len(tc.expectedOverheads))
			for class, expected := range tc.expectedOverheads {
				requireOverheads(t, expected, overheads[class])
			}
		})
	}
}

func TestNodeAllocatableOverheadCLIFlags(t *testing.T) {
	flags := &Flags{}
	cliFlags := nodeAllocatableOverheadCLIFlags(flags)
	require.Len(t, cliFlags, 12)

	names := map[string]bool{}
	for _, f := range cliFlags {
		for _, name := range f.Names() {
			names[name] = true
		}
	}
	for _, expected := range []string{
		"node-allocatable-gpu-memory-overhead-per-pod",
		"node-allocatable-gpu-cpu-overhead-per-container",
		"node-allocatable-mig-memory-overhead-per-container",
		"node-allocatable-mig-cpu-overhead-per-pod",
		"node-allocatable-vfio-memory-overhead-per-pod",
		"node-allocatable-vfio-cpu-overhead-per-container",
	} {
		require.True(t, names[expected], "missing flag %s", expected)
	}
}

func newOverheadTestConfig(t *testing.T, gpu, mig, vfio nodeAllocatableOverheadValues) *Config {
	t.Helper()

	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.NodeAllocatableResources): true,
	}))
	overheads, err := parseNodeAllocatableOverheadFlags(&Flags{
		gpuNodeAllocatableOverhead:  gpu,
		migNodeAllocatableOverhead:  mig,
		vfioNodeAllocatableOverhead: vfio,
	})
	require.NoError(t, err)
	return &Config{
		flags:                    &Flags{},
		nodeAllocatableOverheads: overheads,
	}
}

func TestApplyNodeAllocatableOverheadsPerClassIsolation(t *testing.T) {
	config := newOverheadTestConfig(t,
		nodeAllocatableOverheadValues{memoryPerPod: "100Mi", cpuPerPod: "250m"},
		nodeAllocatableOverheadValues{memoryPerPod: "32Mi"},
		nodeAllocatableOverheadValues{},
	)

	gpuDev := resourceapi.Device{Name: "gpu-0"}
	migDev := resourceapi.Device{Name: "gpu-0-mig-1g5gb-0"}
	vfioDev := resourceapi.Device{Name: "gpu-0-vfio"}
	applyNodeAllocatableOverheads(&gpuDev, config, overheadClassGpu)
	applyNodeAllocatableOverheads(&migDev, config, overheadClassMig)
	applyNodeAllocatableOverheads(&vfioDev, config, overheadClassVfio)

	requireOverheads(t, map[corev1.ResourceName]expectedOverhead{
		corev1.ResourceMemory: {perPod: "100Mi"},
		corev1.ResourceCPU:    {perPod: "250m"},
	}, gpuDev.NodeAllocatableResources)
	requireOverheads(t, map[corev1.ResourceName]expectedOverhead{
		corev1.ResourceMemory: {perPod: "32Mi"},
	}, migDev.NodeAllocatableResources)
	require.Nil(t, vfioDev.NodeAllocatableResources)
}

func TestApplyNodeAllocatableOverheadsNoSharedState(t *testing.T) {
	config := newOverheadTestConfig(t,
		nodeAllocatableOverheadValues{memoryPerPod: "100Mi"},
		nodeAllocatableOverheadValues{},
		nodeAllocatableOverheadValues{},
	)

	expected := map[corev1.ResourceName]expectedOverhead{
		corev1.ResourceMemory: {perPod: "100Mi"},
	}

	devA := resourceapi.Device{Name: "gpu-0"}
	devB := resourceapi.Device{Name: "gpu-1"}
	applyNodeAllocatableOverheads(&devA, config, overheadClassGpu)
	applyNodeAllocatableOverheads(&devB, config, overheadClassGpu)

	requireOverheads(t, expected, devA.NodeAllocatableResources)
	requireOverheads(t, expected, devB.NodeAllocatableResources)

	// Devices must not share mutable state with each other or with the config.
	require.NotSame(t,
		devA.NodeAllocatableResources[corev1.ResourceMemory].Overhead,
		devB.NodeAllocatableResources[corev1.ResourceMemory].Overhead)
	devA.NodeAllocatableResources[corev1.ResourceMemory].Overhead.PerPod.Add(resource.MustParse("1Mi"))
	requireOverheads(t, expected, devB.NodeAllocatableResources)
	requireOverheads(t, expected, config.nodeAllocatableOverheads[overheadClassGpu])
}

func TestApplyNodeAllocatableOverheadsNoConfig(t *testing.T) {
	dev := resourceapi.Device{Name: "gpu-0"}

	require.NotPanics(t, func() {
		applyNodeAllocatableOverheads(&dev, nil, overheadClassGpu)
	})
	require.Nil(t, dev.NodeAllocatableResources)

	require.NotPanics(t, func() {
		applyNodeAllocatableOverheads(&dev, &Config{}, overheadClassGpu)
	})
	require.Nil(t, dev.NodeAllocatableResources)
}

func newTestMigDeviceInfo(parent *GpuInfo) *MigDeviceInfo {
	return &MigDeviceInfo{
		UUID:           "MIG-test",
		Profile:        "1g.5gb",
		ParentUUID:     parent.UUID,
		PlacementStart: 0,
		PlacementSize:  1,
		parent:         parent,
		giProfileInfo:  &nvml.GpuInstanceProfileInfo{MemorySizeMB: 4864},
	}
}

func newTestMigSpec(parent *GpuInfo) *MigSpec {
	return &MigSpec{
		Parent:        parent,
		Profile:       &nvdev.MigProfileInfo{C: 1, G: 1, GB: 5},
		GIProfileInfo: nvml.GpuInstanceProfileInfo{MemorySizeMB: 4864},
		Placement:     nvml.GpuInstancePlacement{Start: 0, Size: 1},
	}
}

func TestDevicePublishPathsCarryPerClassOverheads(t *testing.T) {
	config := newOverheadTestConfig(t,
		nodeAllocatableOverheadValues{memoryPerPod: "100Mi"},
		nodeAllocatableOverheadValues{memoryPerPod: "32Mi"},
		nodeAllocatableOverheadValues{memoryPerPod: "16Mi"},
	)

	parent := newTestGpuInfo(nil)
	gpuDevice := AllocatableDevice{Gpu: parent}
	migStaticDevice := AllocatableDevice{MigStatic: newTestMigDeviceInfo(parent)}
	migDynamicDevice := AllocatableDevice{MigDynamic: newTestMigSpec(parent)}
	vfioDevice := AllocatableDevice{
		Vfio: &VfioDeviceInfo{
			UUID:                   "vfio-test",
			deviceID:               "0x1234",
			vendorID:               "0x10de",
			index:                  0,
			productName:            "NVIDIA Test GPU",
			numaNodeAttr:           newScalarNumaNodeAttribute(0),
			addressableMemoryBytes: 1024,
		},
	}

	for name, tc := range map[string]struct {
		dev            resourceapi.Device
		expectedPerPod string
	}{
		"gpu GetDevice":         {gpuDevice.GetDevice(config), "100Mi"},
		"gpu PartGetDevice":     {gpuDevice.PartGetDevice(config), "100Mi"},
		"mig static GetDevice":  {migStaticDevice.GetDevice(config), "32Mi"},
		"mig dyn PartGetDevice": {migDynamicDevice.PartGetDevice(config), "32Mi"},
		"vfio GetDevice":        {vfioDevice.GetDevice(config), "16Mi"},
	} {
		t.Run(name, func(t *testing.T) {
			requireOverheads(t, map[corev1.ResourceName]expectedOverhead{
				corev1.ResourceMemory: {perPod: tc.expectedPerPod},
			}, tc.dev.NodeAllocatableResources)
		})
	}
}

func TestDevicePublishPathsUnconfiguredClassStaysNil(t *testing.T) {
	// Only the gpu class is configured: MIG and VFIO devices must stay nil.
	config := newOverheadTestConfig(t,
		nodeAllocatableOverheadValues{memoryPerPod: "100Mi"},
		nodeAllocatableOverheadValues{},
		nodeAllocatableOverheadValues{},
	)

	parent := newTestGpuInfo(nil)
	migStaticDevice := AllocatableDevice{MigStatic: newTestMigDeviceInfo(parent)}
	migDynamicDevice := AllocatableDevice{MigDynamic: newTestMigSpec(parent)}
	vfioDevice := AllocatableDevice{
		Vfio: &VfioDeviceInfo{
			UUID:                   "vfio-test",
			deviceID:               "0x1234",
			vendorID:               "0x10de",
			index:                  0,
			productName:            "NVIDIA Test GPU",
			numaNodeAttr:           newScalarNumaNodeAttribute(0),
			addressableMemoryBytes: 1024,
		},
	}

	require.Nil(t, migStaticDevice.GetDevice(config).NodeAllocatableResources)
	require.Nil(t, migDynamicDevice.PartGetDevice(config).NodeAllocatableResources)
	require.Nil(t, vfioDevice.GetDevice(config).NodeAllocatableResources)
}
