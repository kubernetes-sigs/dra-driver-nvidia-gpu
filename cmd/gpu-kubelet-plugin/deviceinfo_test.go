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
