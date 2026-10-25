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
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

// nodePartition returns the NVLink partition of a node, today its GPU clique
// as labeled by the kubelet plugin.
func nodePartition(node *corev1.Node) string {
	return node.Labels[gpuCliqueNodeLabelKey]
}

// daemonPartition returns the NVLink partition whose IMEX domain a daemon
// joined, as it labels its pod once it starts, or false before.
func daemonPartition(pod *corev1.Pod) (string, bool) {
	partition, ok := pod.Labels[computeDomainCliqueLabelKey]
	return partition, ok
}

// staticClique holds the static IMEX daemons of one NVLink clique.
type staticClique struct {
	id string
	// nodes should run a daemon.
	nodes []string
	pods  []*corev1.Pod
}

// groupByClique groups daemon pods by the clique of their node. Nodes are
// those of a clique that match the node selector. Pods on nodes without a
// clique are returned separately.
func groupByClique(nodes []*corev1.Node, pods []*corev1.Pod, nodeSelector map[string]string) ([]*staticClique, []*corev1.Pod) {
	selector := labels.SelectorFromSet(nodeSelector)
	cliqueOf := make(map[string]string, len(nodes))
	byID := make(map[string]*staticClique)
	for _, node := range nodes {
		id := nodePartition(node)
		if id == "" {
			continue
		}
		cliqueOf[node.Name] = id
		if byID[id] == nil {
			byID[id] = &staticClique{id: id}
		}
		if selector.Matches(labels.Set(node.Labels)) {
			byID[id].nodes = append(byID[id].nodes, node.Name)
		}
	}

	var unclustered []*corev1.Pod
	for _, pod := range pods {
		id, ok := cliqueOf[pod.Spec.NodeName]
		if !ok {
			unclustered = append(unclustered, pod)
			continue
		}
		byID[id].pods = append(byID[id].pods, pod)
	}

	cliques := make([]*staticClique, 0, len(byID))
	for _, c := range byID {
		if len(c.nodes) > 0 || len(c.pods) > 0 {
			cliques = append(cliques, c)
		}
	}
	slices.SortFunc(cliques, func(a, b *staticClique) int { return strings.Compare(a.id, b.id) })
	return cliques, unclustered
}

func podUpdated(pod *corev1.Pod, hash string) bool {
	return pod.Annotations[staticDaemonTemplateHashKey] == hash
}

// podReadySince returns when a running pod became ready, or false.
func podReadySince(pod *corev1.Pod) (time.Time, bool) {
	if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return time.Time{}, false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.LastTransitionTime.Time, c.Status == corev1.ConditionTrue
		}
	}
	return time.Time{}, false
}

// cliqueStatuses summarizes the daemons of each clique.
func cliqueStatuses(cliques []*staticClique, hash string) []nvapi.ComputeDomainDaemonSetCliqueStatus {
	statuses := make([]nvapi.ComputeDomainDaemonSetCliqueStatus, 0, len(cliques))
	for _, c := range cliques {
		status := nvapi.ComputeDomainDaemonSetCliqueStatus{ID: c.id, Nodes: int32(len(c.nodes))}
		for _, pod := range c.pods {
			if _, ready := podReadySince(pod); ready {
				status.ReadyNodes++
			}
			if podUpdated(pod, hash) {
				status.UpdatedNodes++
			}
		}
		statuses = append(statuses, status)
	}
	return statuses
}
