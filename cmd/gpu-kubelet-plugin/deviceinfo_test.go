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

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/utils/ptr"

	"github.com/stretchr/testify/require"
)

func newTestGpuInfo(numaNodeAttr *deviceattribute.DeviceAttribute) *GpuInfo {
	return &GpuInfo{
		UUID:                  "GPU-test",
		minor:                 0,
		productName:           "NVIDIA Test GPU",
		brand:                 "NVIDIA",
		architecture:          "Test",
		cudaComputeCapability: "9.0",
		driverVersion:         "580.0.0",
		cudaDriverVersion:     "13.0",
		numaNodeAttr:          numaNodeAttr,
	}
}

func newScalarNumaNodeAttribute(numaNode int64) *deviceattribute.DeviceAttribute {
	return &deviceattribute.DeviceAttribute{
		Name: deviceattribute.StandardDeviceAttributeNUMANode,
		Value: resourceapi.DeviceAttribute{
			IntValue: ptr.To(numaNode),
		},
	}
}

func newListNumaNodeAttribute(numaNodes ...int64) *deviceattribute.DeviceAttribute {
	return &deviceattribute.DeviceAttribute{
		Name: deviceattribute.StandardDeviceAttributeNUMANode,
		Value: resourceapi.DeviceAttribute{
			IntValues: numaNodes,
		},
	}
}

func requireNumaNodeAttribute(t *testing.T, attrs map[resourceapi.QualifiedName]resourceapi.DeviceAttribute, expected int64) {
	t.Helper()

	attr, ok := attrs[deviceattribute.StandardDeviceAttributeNUMANode]
	require.True(t, ok)
	require.NotNil(t, attr.IntValue)
	require.Equal(t, expected, *attr.IntValue)
}

func requireNumaNodeListAttribute(t *testing.T, attrs map[resourceapi.QualifiedName]resourceapi.DeviceAttribute, expected []int64) {
	t.Helper()

	attr, ok := attrs[deviceattribute.StandardDeviceAttributeNUMANode]
	require.True(t, ok)
	require.Nil(t, attr.IntValue)
	require.Equal(t, expected, attr.IntValues)
}

func TestGpuInfoAttributesIncludeStandardNumaNode(t *testing.T) {
	gpu := newTestGpuInfo(newScalarNumaNodeAttribute(1))

	requireNumaNodeAttribute(t, gpu.Attributes(), 1)
}

func TestGpuInfoAttributesIncludeStandardNumaNodeList(t *testing.T) {
	gpu := newTestGpuInfo(newListNumaNodeAttribute(1, 2))

	requireNumaNodeListAttribute(t, gpu.Attributes(), []int64{1, 2})
}

func TestCommonMigAttributesIncludeStandardNumaNode(t *testing.T) {
	parent := newTestGpuInfo(newScalarNumaNodeAttribute(2))

	requireNumaNodeAttribute(t, CommonAttributesMig(parent, "1g.10gb"), 2)
}

func TestVfioDeviceIncludesStandardNumaNode(t *testing.T) {
	vfio := &VfioDeviceInfo{
		UUID:                   "vfio-test",
		deviceID:               "0x1234",
		vendorID:               "0x10de",
		index:                  0,
		productName:            "NVIDIA Test GPU",
		numaNodeAttr:           newScalarNumaNodeAttribute(3),
		addressableMemoryBytes: 1024,
	}

	requireNumaNodeAttribute(t, vfio.GetDevice().Attributes, 3)
}

func TestGpuInfoCanonicalName(t *testing.T) {
	tests := map[string]struct {
		minor int
		want  DeviceName
	}{
		"minor zero":      {minor: 0, want: "gpu-0"},
		"single digit":    {minor: 7, want: "gpu-7"},
		"multiple digits": {minor: 42, want: "gpu-42"},
	}

	for description, tc := range tests {
		t.Run(description, func(t *testing.T) {
			gpu := &GpuInfo{minor: tc.minor}
			require.Equal(t, tc.want, gpu.CanonicalName())
		})
	}
}

func TestGpuInfoString(t *testing.T) {
	// String() is used in log messages and combines the canonical name (for
	// recognizability) with the UUID (for precision).
	gpu := &GpuInfo{minor: 3, UUID: "GPU-abc123"}
	require.Equal(t, "gpu-3-GPU-abc123", gpu.String())
}

func TestVfioDeviceInfoCanonicalName(t *testing.T) {
	tests := map[string]struct {
		index int
		want  string
	}{
		"index zero":    {index: 0, want: "gpu-vfio-0"},
		"index nonzero": {index: 5, want: "gpu-vfio-5"},
	}

	for description, tc := range tests {
		t.Run(description, func(t *testing.T) {
			vfio := &VfioDeviceInfo{index: tc.index}
			require.Equal(t, tc.want, vfio.CanonicalName())
		})
	}
}

func TestMigDeviceInfoSpecTuple(t *testing.T) {
	mig := &MigDeviceInfo{
		ParentMinor:    2,
		GiProfileID:    19,
		PlacementStart: 4,
	}

	require.Equal(t, &MigSpecTuple{
		ParentMinor:    2,
		ProfileID:      19,
		PlacementStart: 4,
	}, mig.SpecTuple())
}

func TestMigDeviceInfoLiveTuple(t *testing.T) {
	mig := &MigDeviceInfo{
		UUID:        "MIG-live",
		ParentUUID:  "GPU-parent",
		ParentMinor: 1,
		GIID:        6,
		CIID:        0,
	}

	require.Equal(t, &MigLiveTuple{
		ParentMinor: 1,
		ParentUUID:  "GPU-parent",
		GIID:        6,
		CIID:        0,
		MigUUID:     "MIG-live",
	}, mig.LiveTuple())
}

func TestAddDeviceAttribute(t *testing.T) {
	t.Run("nil attribute is a no-op", func(t *testing.T) {
		attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{}
		addDeviceAttribute(attrs, nil)
		require.Empty(t, attrs)
	})

	t.Run("non-nil attribute is added under its name", func(t *testing.T) {
		attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{}
		attr := &deviceattribute.DeviceAttribute{
			Name: resourceapi.QualifiedName("pciBusID"),
			Value: resourceapi.DeviceAttribute{
				StringValue: ptr.To("0000:65:00.0"),
			},
		}

		addDeviceAttribute(attrs, attr)

		got, ok := attrs["pciBusID"]
		require.True(t, ok)
		require.NotNil(t, got.StringValue)
		require.Equal(t, "0000:65:00.0", *got.StringValue)
	})
}

func TestGpuInfoAttributesCoreValues(t *testing.T) {
	gpu := newTestGpuInfo(nil)

	attrs := gpu.Attributes()

	// String-valued attributes are copied through verbatim.
	stringAttrs := map[resourceapi.QualifiedName]string{
		"type":         GpuDeviceType,
		"uuid":         "GPU-test",
		"productName":  "NVIDIA Test GPU",
		"brand":        "NVIDIA",
		"architecture": "Test",
	}
	for name, want := range stringAttrs {
		attr, ok := attrs[name]
		require.True(t, ok, "expected attribute %q", name)
		require.NotNil(t, attr.StringValue, "attribute %q should be string-valued", name)
		require.Equal(t, want, *attr.StringValue)
	}

	// Version-valued attributes are normalized to full semver (e.g. "9.0" ->
	// "9.0.0") by semver.MustParse(...).String().
	versionAttrs := map[resourceapi.QualifiedName]string{
		"cudaComputeCapability": "9.0.0",
		"driverVersion":         "580.0.0",
		"cudaDriverVersion":     "13.0.0",
	}
	for name, want := range versionAttrs {
		attr, ok := attrs[name]
		require.True(t, ok, "expected attribute %q", name)
		require.NotNil(t, attr.VersionValue, "attribute %q should be version-valued", name)
		require.Equal(t, want, *attr.VersionValue)
	}
}

func TestGpuInfoAttributesAddressingMode(t *testing.T) {
	t.Run("omitted when unset", func(t *testing.T) {
		gpu := newTestGpuInfo(nil)
		_, ok := gpu.Attributes()["addressingMode"]
		require.False(t, ok)
	})

	t.Run("included when set", func(t *testing.T) {
		gpu := newTestGpuInfo(nil)
		gpu.addressingMode = ptr.To("hmm")

		attr, ok := gpu.Attributes()["addressingMode"]
		require.True(t, ok)
		require.NotNil(t, attr.StringValue)
		require.Equal(t, "hmm", *attr.StringValue)
	})
}
