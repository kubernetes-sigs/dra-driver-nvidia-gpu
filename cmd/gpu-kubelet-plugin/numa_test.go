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
	"os"
	"path/filepath"
	"testing"

	"k8s.io/dynamic-resource-allocation/deviceattribute"

	"github.com/stretchr/testify/require"
)

const testPCIBusID = "0000:02:00.0"

func TestGetNUMANodeAttributeByPCIBusIDScalar(t *testing.T) {
	root := t.TempDir()
	writeSysfsFile(t, root, "bus", "pci", "devices", testPCIBusID, "numa_node", "4\n")

	attr, err := getNUMANodeAttributeByPCIBusID(testPCIBusID, deviceattribute.ScalarAttribute, root)

	require.NoError(t, err)
	require.Equal(t, deviceattribute.StandardDeviceAttributeNUMANode, attr.Name)
	require.NotNil(t, attr.Value.IntValue)
	require.Equal(t, int64(4), *attr.Value.IntValue)
	require.Empty(t, attr.Value.IntValues)
}

func TestConfiguredSysfsRootUsesEnvOverride(t *testing.T) {
	t.Setenv(sysfsRootEnvvar, "/mock/sys")

	require.Equal(t, "/mock/sys", configuredSysfsRoot())
}

func TestGetNUMANodeAttributeByPCIBusIDListWithSameSocketFilter(t *testing.T) {
	root := t.TempDir()
	writeSysfsFile(t, root, "bus", "pci", "devices", testPCIBusID, "numa_node", "0\n")
	writeSysfsFile(t, root, "devices", "system", "node", "node0", "distance", "10 12 12 20\n")
	writeSysfsFile(t, root, "devices", "system", "node", "node0", "cpulist", "0\n")
	writeSysfsFile(t, root, "devices", "system", "node", "node1", "cpulist", "1\n")
	writeSysfsFile(t, root, "devices", "system", "node", "node2", "cpulist", "2\n")
	writeSysfsFile(t, root, "devices", "system", "cpu", "cpu0", "topology", "physical_package_id", "0\n")
	writeSysfsFile(t, root, "devices", "system", "cpu", "cpu1", "topology", "physical_package_id", "0\n")
	writeSysfsFile(t, root, "devices", "system", "cpu", "cpu2", "topology", "physical_package_id", "1\n")

	attr, err := getNUMANodeAttributeByPCIBusID(testPCIBusID, deviceattribute.ListAttribute, root)

	require.NoError(t, err)
	require.Equal(t, deviceattribute.StandardDeviceAttributeNUMANode, attr.Name)
	require.Nil(t, attr.Value.IntValue)
	require.Equal(t, []int64{0, 1}, attr.Value.IntValues)
}

func TestGetNUMANodeAttributeByPCIBusIDListWithoutSocketDataIncludesMinimumDistanceNodes(t *testing.T) {
	root := t.TempDir()
	writeSysfsFile(t, root, "bus", "pci", "devices", testPCIBusID, "numa_node", "1\n")
	writeSysfsFile(t, root, "devices", "system", "node", "node1", "distance", "12 10 12\n")

	attr, err := getNUMANodeAttributeByPCIBusID(testPCIBusID, deviceattribute.ListAttribute, root)

	require.NoError(t, err)
	require.Equal(t, []int64{1, 0, 2}, attr.Value.IntValues)
}

func TestGetNUMANodeAttributeByPCIBusIDListFallsBackWithoutDistance(t *testing.T) {
	root := t.TempDir()
	writeSysfsFile(t, root, "bus", "pci", "devices", testPCIBusID, "numa_node", "2\n")

	attr, err := getNUMANodeAttributeByPCIBusID(testPCIBusID, deviceattribute.ListAttribute, root)

	require.NoError(t, err)
	require.Equal(t, []int64{2}, attr.Value.IntValues)
}

func TestGetNUMANodeAttributeByPCIBusIDOmitsNoAffinityDevice(t *testing.T) {
	root := t.TempDir()
	writeSysfsFile(t, root, "bus", "pci", "devices", testPCIBusID, "numa_node", "-1\n")

	_, err := getNUMANodeAttributeByPCIBusID(testPCIBusID, deviceattribute.ListAttribute, root)

	require.Error(t, err)
	require.Contains(t, err.Error(), "no NUMA affinity")
}

func writeSysfsFile(t *testing.T, root string, elems ...string) {
	t.Helper()
	require.GreaterOrEqual(t, len(elems), 2)

	content := elems[len(elems)-1]
	path := filepath.Join(append([]string{root}, elems[:len(elems)-1]...)...)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}
