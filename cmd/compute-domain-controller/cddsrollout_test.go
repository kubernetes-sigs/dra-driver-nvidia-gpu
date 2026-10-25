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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/stretchr/testify/assert"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

func TestPlanCliqueRollout(t *testing.T) {
	rollingUpdate := func(maxUnavailable intstr.IntOrString, minReadySeconds int32) *nvapi.ComputeDomainDaemonSetRollingUpdate {
		return &nvapi.ComputeDomainDaemonSetRollingUpdate{MaxUnavailableCliques: &maxUnavailable, MinReadySeconds: minReadySeconds}
	}

	tests := map[string]struct {
		cliques      int
		mutate       func(nodes []*corev1.Node, pods []*corev1.Pod)
		ru           *nvapi.ComputeDomainDaemonSetRollingUpdate
		restarting   map[string]bool
		wantCliques  []string
		wantRequeue  bool
		wantDeletion int
	}{
		"one clique at a time by default": {
			cliques:      3,
			wantCliques:  []string{"c0"},
			wantDeletion: 2,
		},
		"a number of cliques at a time": {
			cliques:      3,
			ru:           rollingUpdate(intstr.FromInt32(2), 0),
			wantCliques:  []string{"c0", "c1"},
			wantDeletion: 4,
		},
		"a percentage of cliques rounds down": {
			cliques:      5,
			ru:           rollingUpdate(intstr.FromString("50%"), 0),
			wantCliques:  []string{"c0", "c1"},
			wantDeletion: 4,
		},
		"a small percentage still updates one clique": {
			cliques:      3,
			ru:           rollingUpdate(intstr.FromString("10%"), 0),
			wantCliques:  []string{"c0"},
			wantDeletion: 2,
		},
		"an updated clique that is not ready yet uses up the batch": {
			cliques: 3,
			mutate: func(_ []*corev1.Node, pods []*corev1.Pod) {
				pods[0].Annotations[staticDaemonTemplateHashKey] = "new"
				pods[1].Annotations[staticDaemonTemplateHashKey] = "new"
				pods[1].Status.Conditions[0].Status = corev1.ConditionFalse
			},
		},
		"an updated clique waits for minReadySeconds": {
			cliques: 2,
			ru:      rollingUpdate(intstr.FromInt32(1), 30),
			mutate: func(_ []*corev1.Node, pods []*corev1.Pod) {
				for _, pod := range pods[:2] {
					pod.Annotations[staticDaemonTemplateHashKey] = "new"
					pod.Status.Conditions[0].LastTransitionTime = metav1.NewTime(rolloutNow.Add(-10 * time.Second))
				}
			},
			wantRequeue: true,
		},
		"a clique restarted recently is not restarted twice": {
			cliques:    2,
			restarting: map[string]bool{"c0": true},
		},
		"an unavailable outdated clique goes first": {
			cliques: 3,
			mutate: func(_ []*corev1.Node, pods []*corev1.Pod) {
				pods[5].Status.Conditions[0].Status = corev1.ConditionFalse
			},
			wantCliques:  []string{"c2"},
			wantDeletion: 2,
		},
		"a daemon of a failed node does not hold up the rollout": {
			cliques: 2,
			mutate: func(nodes []*corev1.Node, pods []*corev1.Pod) {
				pods[0].Annotations[staticDaemonTemplateHashKey] = "new"
				pods[1].Annotations[staticDaemonTemplateHashKey] = "new"
				nodes[1].Status.Conditions[0].Status = corev1.ConditionFalse
				pods[1].Status.Conditions[0].Status = corev1.ConditionFalse
			},
			wantCliques:  []string{"c1"},
			wantDeletion: 2,
		},
		"a clique whose daemons are being recreated uses up the batch": {
			cliques: 2,
			mutate: func(_ []*corev1.Node, pods []*corev1.Pod) {
				// c0-n1 has no daemon yet.
				pods[0].Annotations[staticDaemonTemplateHashKey] = "new"
				pods[1].Annotations[staticDaemonTemplateHashKey] = "new"
				pods[1].Spec.NodeName = "c0-n0"
				pods[1].DeletionTimestamp = &metav1.Time{}
			},
		},
		"paused": {
			cliques: 2,
			ru:      &nvapi.ComputeDomainDaemonSetRollingUpdate{Paused: true},
		},
		"daemons without a clique restart right away": {
			cliques: 1,
			ru:      &nvapi.ComputeDomainDaemonSetRollingUpdate{Paused: true},
			mutate: func(nodes []*corev1.Node, _ []*corev1.Pod) {
				delete(nodes[0].Labels, gpuCliqueNodeLabelKey)
			},
			wantDeletion: 1,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			nodes, pods := testCliques(tc.cliques, 2, "old")
			if tc.mutate != nil {
				tc.mutate(nodes, pods)
			}
			cliques, unclustered := groupByClique(nodes, pods, nil)

			plan := planCliqueRollout(cliques, unclustered, "new", tc.ru, nodeReadiness(nodes), tc.restarting, rolloutNow)

			assert.Equal(t, tc.wantCliques, plan.cliques)
			assert.Len(t, plan.pods, tc.wantDeletion, podNames(plan.pods))
			assert.Equal(t, tc.wantRequeue, plan.requeueAfter > 0)
		})
	}
}

func TestPlanCliqueRolloutRestartsWholeCliques(t *testing.T) {
	nodes, pods := testCliques(2, 18, "old")
	cliques, unclustered := groupByClique(nodes, pods, nil)

	plan := planCliqueRollout(cliques, unclustered, "new", nil, nodeReadiness(nodes), nil, rolloutNow)

	assert.Equal(t, []string{"c0"}, plan.cliques)
	assert.Len(t, plan.pods, 18, "all daemons of the clique restart together, so its IMEX domain is disrupted once")
	for _, pod := range plan.pods {
		assert.Contains(t, pod.Spec.NodeName, "c0-")
	}
}

func TestMaxUnavailableCliques(t *testing.T) {
	assert.Equal(t, 1, maxUnavailableCliques(nil, 10))
	assert.Equal(t, 3, maxUnavailableCliques(&nvapi.ComputeDomainDaemonSetRollingUpdate{MaxUnavailableCliques: ptr.To(intstr.FromInt32(3))}, 10))
	assert.Equal(t, 2, maxUnavailableCliques(&nvapi.ComputeDomainDaemonSetRollingUpdate{MaxUnavailableCliques: ptr.To(intstr.FromString("25%"))}, 10))
	assert.Equal(t, 1, maxUnavailableCliques(&nvapi.ComputeDomainDaemonSetRollingUpdate{MaxUnavailableCliques: ptr.To(intstr.FromInt32(0))}, 10))
}
