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

package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

func TestBuildNodesFromPodsAssignsUniqueIndices(t *testing.T) {
	m := &ComputeDomainStatusManager{}
	pods := []*corev1.Pod{
		nonFabricPod("node-b", "10.0.0.2", false),
		nonFabricPod("node-a", "10.0.0.1", true),
	}

	nodes := m.buildNodesFromPods(pods, nil)

	require.Equal(t, []*nvapi.ComputeDomainNode{
		{Name: "node-a", IPAddress: "10.0.0.1", CliqueID: "", Index: 0, Status: nvapi.ComputeDomainStatusReady},
		{Name: "node-b", IPAddress: "10.0.0.2", CliqueID: "", Index: 1, Status: nvapi.ComputeDomainStatusNotReady},
	}, nodes)
}

func TestBuildNodesFromPodsPreservesExistingIndices(t *testing.T) {
	m := &ComputeDomainStatusManager{}
	existingNodes := []*nvapi.ComputeDomainNode{
		{Name: "node-a", CliqueID: "", Index: 4},
		{Name: "node-b", CliqueID: "", Index: 7},
		{Name: "stale-node", CliqueID: "", Index: 0},
	}
	pods := []*corev1.Pod{
		nonFabricPod("node-c", "10.0.0.3", false),
		nonFabricPod("node-b", "10.0.1.2", false),
		nonFabricPod("node-a", "10.0.1.1", false),
	}

	nodes := m.buildNodesFromPods(pods, existingNodes)

	require.Equal(t, 4, nodes[0].Index)
	require.Equal(t, 7, nodes[1].Index)
	require.Equal(t, 0, nodes[2].Index)
}

func TestBuildNodesFromPodsRepairsInvalidIndices(t *testing.T) {
	m := &ComputeDomainStatusManager{}
	existingNodes := []*nvapi.ComputeDomainNode{
		{Name: "node-a", CliqueID: "", Index: -1},
		{Name: "node-b", CliqueID: "", Index: 2},
		{Name: "node-c", CliqueID: "", Index: 2},
		{Name: "node-d", CliqueID: "", Index: 5},
	}
	pods := []*corev1.Pod{
		nonFabricPod("node-d", "10.0.0.4", false),
		nonFabricPod("node-c", "10.0.0.3", false),
		nonFabricPod("node-b", "10.0.0.2", false),
		nonFabricPod("node-a", "10.0.0.1", false),
	}

	nodes := m.buildNodesFromPods(pods, existingNodes)

	require.Equal(t, []int{0, 1, 2, 5}, []int{nodes[0].Index, nodes[1].Index, nodes[2].Index, nodes[3].Index})
}

func TestBuildNodesFromPodsFiltersInvalidPodsAndDuplicateNodes(t *testing.T) {
	m := &ComputeDomainStatusManager{}
	pods := []*corev1.Pod{
		nonFabricPod("", "10.0.0.1", false),
		nonFabricPod("node-no-ip", "", false),
		nonFabricPod("node-a", "10.0.0.2", false),
		nonFabricPod("node-a", "10.0.0.3", true),
	}

	nodes := m.buildNodesFromPods(pods, nil)

	require.Len(t, nodes, 1)
	require.Equal(t, "node-a", nodes[0].Name)
	require.Equal(t, "10.0.0.3", nodes[0].IPAddress)
	require.Equal(t, 0, nodes[0].Index)
	require.Equal(t, nvapi.ComputeDomainStatusReady, nodes[0].Status)
}

func TestSyncCDMigratesLegacyIndicesOnce(t *testing.T) {
	cd := &nvapi.ComputeDomain{}
	cd.Status.Nodes = []*nvapi.ComputeDomainNode{
		{Name: "fabric-node", IPAddress: "10.0.0.10", CliqueID: "clique-a", Index: 3},
		{Name: "node-a", IPAddress: "10.0.0.1", CliqueID: "", Index: -1},
		{Name: "node-b", IPAddress: "10.0.0.2", CliqueID: "", Index: -1},
	}
	fabricPods := []*corev1.Pod{nonFabricPod("fabric-node", "10.0.0.10", true)}
	nonFabricPods := []*corev1.Pod{
		nonFabricPod("node-b", "10.0.0.2", true),
		nonFabricPod("node-a", "10.0.0.1", true),
	}

	var updated *nvapi.ComputeDomain
	m := &ComputeDomainStatusManager{
		updateComputeDomainStatus: func(_ context.Context, newCD *nvapi.ComputeDomain) (*nvapi.ComputeDomain, error) {
			updated = newCD
			return newCD, nil
		},
	}

	m.syncCD(context.Background(), cd, nil, fabricPods, nonFabricPods)

	require.NotNil(t, updated)
	require.Equal(t, []*nvapi.ComputeDomainNode{
		{Name: "fabric-node", IPAddress: "10.0.0.10", CliqueID: "clique-a", Index: 3},
		{Name: "node-a", IPAddress: "10.0.0.1", CliqueID: "", Index: 0, Status: nvapi.ComputeDomainStatusReady},
		{Name: "node-b", IPAddress: "10.0.0.2", CliqueID: "", Index: 1, Status: nvapi.ComputeDomainStatusReady},
	}, updated.Status.Nodes)

	cd = updated
	updated = nil
	m.syncCD(context.Background(), cd, nil, fabricPods, nonFabricPods)
	require.Nil(t, updated)
}

func nonFabricPod(nodeName, podIP string, ready bool) *corev1.Pod {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{NodeName: nodeName},
		Status: corev1.PodStatus{
			PodIP: podIP,
		},
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{
			Type:   corev1.PodReady,
			Status: corev1.ConditionTrue,
		}}
	}
	return pod
}
