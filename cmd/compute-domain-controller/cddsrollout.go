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
	"math"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

// hasOutdatedPods reports whether a clique still runs daemons of an older
// revision that are not being deleted.
func (c *staticClique) hasOutdatedPods(hash string) bool {
	return slices.ContainsFunc(c.pods, func(pod *corev1.Pod) bool {
		return !podUpdated(pod, hash) && pod.DeletionTimestamp == nil
	})
}

// availability reports whether every ready node of a clique runs a daemon
// that has been ready for minReady, or else how long until they will have
// been. Unready nodes are left out, so that a failed node does not hold up a
// rollout.
func (c *staticClique) availability(nodeReady map[string]bool, minReady time.Duration, now time.Time) (bool, time.Duration) {
	podOn := make(map[string]*corev1.Pod, len(c.pods))
	for _, pod := range c.pods {
		if existing, ok := podOn[pod.Spec.NodeName]; !ok || existing.DeletionTimestamp != nil {
			podOn[pod.Spec.NodeName] = pod
		}
	}
	var wait time.Duration
	for _, node := range c.nodes {
		if !nodeReady[node] {
			continue
		}
		pod, ok := podOn[node]
		if !ok {
			return false, 0
		}
		since, ready := podReadySince(pod)
		if !ready {
			return false, 0
		}
		if remaining := since.Add(minReady).Sub(now); remaining > wait {
			wait = remaining
		}
	}
	return wait <= 0, wait
}

// maxUnavailableCliques resolves the batch size of a rolling update. A
// percentage rounds down, to at least one clique.
func maxUnavailableCliques(ru *nvapi.ComputeDomainDaemonSetRollingUpdate, cliques int) int {
	value := intstr.FromInt32(1)
	if ru != nil && ru.MaxUnavailableCliques != nil {
		value = *ru.MaxUnavailableCliques
	}
	n, err := intstr.GetScaledValueFromIntOrPercent(&value, cliques, false)
	if err != nil {
		return 1
	}
	return max(n, 1)
}

// rolloutPlan is the next step of a rolling update.
type rolloutPlan struct {
	// cliques to restart, and their outdated daemon pods.
	cliques []string
	pods    []*corev1.Pod
	// requeueAfter is set while a clique waits for minReadySeconds.
	requeueAfter time.Duration
}

// planCliqueRollout restarts whole cliques together, at most
// maxUnavailableCliques at a time. Cliques that are updated but not yet
// available, or that were restarted recently, use up the batch. Unavailable
// outdated cliques go first, as their workloads are disrupted already.
func planCliqueRollout(cliques []*staticClique, unclustered []*corev1.Pod, hash string, ru *nvapi.ComputeDomainDaemonSetRollingUpdate, nodeReady map[string]bool, restarting map[string]bool, now time.Time) rolloutPlan {
	var plan rolloutPlan
	// Pods without a clique run no IMEX daemon, so restarting them disrupts
	// nothing.
	for _, pod := range unclustered {
		if !podUpdated(pod, hash) && pod.DeletionTimestamp == nil {
			plan.pods = append(plan.pods, pod)
		}
	}

	minReady := time.Duration(0)
	if ru != nil {
		minReady = time.Duration(ru.MinReadySeconds) * time.Second
	}

	inProgress := 0
	requeueAfter := time.Duration(math.MaxInt64)
	var unavailableCandidates, candidates []*staticClique
	for _, c := range cliques {
		available, wait := c.availability(nodeReady, minReady, now)
		if wait > 0 {
			requeueAfter = min(requeueAfter, wait)
		}
		switch {
		case c.hasOutdatedPods(hash) && restarting[c.id]:
			inProgress++
		case c.hasOutdatedPods(hash) && available:
			candidates = append(candidates, c)
		case c.hasOutdatedPods(hash):
			unavailableCandidates = append(unavailableCandidates, c)
		case !available || restarting[c.id]:
			inProgress++
		}
	}
	if requeueAfter != math.MaxInt64 {
		plan.requeueAfter = requeueAfter
	}

	if ru != nil && ru.Paused {
		return plan
	}
	budget := maxUnavailableCliques(ru, len(cliques)) - inProgress
	for _, c := range slices.Concat(unavailableCandidates, candidates) {
		if budget <= 0 {
			break
		}
		budget--
		plan.cliques = append(plan.cliques, c.id)
		for _, pod := range c.pods {
			if !podUpdated(pod, hash) && pod.DeletionTimestamp == nil {
				plan.pods = append(plan.pods, pod)
			}
		}
	}
	return plan
}
