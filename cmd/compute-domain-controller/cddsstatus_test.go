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
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stretchr/testify/assert"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

var rolloutNow = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

func cliqueNode(name, clique string, ready bool) *corev1.Node {
	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionFalse
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}},
	}
	if clique != "" {
		node.Labels[gpuCliqueNodeLabelKey] = clique
	}
	return node
}

// daemonPod returns a static IMEX daemon pod of a template hash, ready for
// readyFor, or not ready if readyFor is negative.
func daemonPod(node, hash string, readyFor time.Duration) *corev1.Pod {
	ready := corev1.ConditionTrue
	if readyFor < 0 {
		ready = corev1.ConditionFalse
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "imex-" + node,
			Namespace:   "driver",
			UID:         types.UID("uid-imex-" + node),
			Labels:      map[string]string{computeDomainDaemonSetLabelKey: "cdds-uid"},
			Annotations: map[string]string{staticDaemonTemplateHashKey: hash},
		},
		Spec: corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: ready, LastTransitionTime: metav1.NewTime(rolloutNow.Add(-readyFor)),
			}},
		},
	}
}

// testCliques builds cliques c0..cN-1 of nodesPerClique nodes, all ready and
// running the given hash for an hour.
func testCliques(cliques, nodesPerClique int, hash string) ([]*corev1.Node, []*corev1.Pod) {
	var nodes []*corev1.Node
	var pods []*corev1.Pod
	for c := range cliques {
		for n := range nodesPerClique {
			name := fmt.Sprintf("c%d-n%d", c, n)
			nodes = append(nodes, cliqueNode(name, fmt.Sprintf("c%d", c), true))
			pods = append(pods, daemonPod(name, hash, time.Hour))
		}
	}
	return nodes, pods
}

func podNames(pods []*corev1.Pod) []string {
	var names []string
	for _, pod := range pods {
		names = append(names, pod.Name)
	}
	return names
}

func TestGroupByCliqueAppliesNodeSelector(t *testing.T) {
	nodes, pods := testCliques(1, 2, "h")
	nodes[0].Labels["pool"] = "a"

	cliques, _ := groupByClique(nodes, pods[:1], map[string]string{"pool": "a"})

	assert.Equal(t, []string{"c0-n0"}, cliques[0].nodes, "only selected nodes run a daemon")
	statuses := cliqueStatuses(cliques, "h")
	assert.Equal(t, []nvapi.ComputeDomainDaemonSetCliqueStatus{{ID: "c0", Nodes: 1, ReadyNodes: 1, UpdatedNodes: 1}}, statuses)
}
