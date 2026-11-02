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
	"math"
	"slices"
	"strings"
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

// podsByNode returns the daemon pod of each node of a clique, preferring a
// pod that is not being deleted.
func (c *staticClique) podsByNode() map[string]*corev1.Pod {
	podOn := make(map[string]*corev1.Pod, len(c.pods))
	for _, pod := range c.pods {
		if existing, ok := podOn[pod.Spec.NodeName]; !ok || existing.DeletionTimestamp != nil {
			podOn[pod.Spec.NodeName] = pod
		}
	}
	return podOn
}

// availability reports whether every node of a clique that holds up a rollout
// runs a daemon that has been ready for minReady, or else how long until they
// will have been. Unready and cordoned nodes are left out, so that a failed
// node does not hold up a rollout.
func (c *staticClique) availability(nodeReady map[string]bool, minReady time.Duration, now time.Time) (bool, time.Duration) {
	podOn := c.podsByNode()
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

// lastChange returns when the daemons of a clique last changed, as their pods
// were created or deleted. It is derived from the pods, so that it survives a
// restart of the controller.
func (c *staticClique) lastChange() time.Time {
	var last time.Time
	for _, pod := range c.pods {
		if t := pod.CreationTimestamp.Time; t.After(last) {
			last = t
		}
		if pod.DeletionTimestamp != nil && pod.DeletionTimestamp.After(last) {
			last = pod.DeletionTimestamp.Time
		}
	}
	return last
}

// maxStalledNodes bounds the nodes a stalled reason lists, as a broken
// revision usually fails all nodes alike.
const maxStalledNodes = 3

// unavailableReason tells which nodes keep a clique unavailable, and why, so
// that admins can act on them. It is empty when the daemons are ready.
func (c *staticClique) unavailableReason(nodeReady map[string]bool) string {
	podOn := c.podsByNode()
	var reasons []string
	blocked := 0
	for _, node := range c.nodes {
		if !nodeReady[node] {
			continue
		}
		reason := "no IMEX daemon pod"
		if pod, ok := podOn[node]; ok {
			reason = podUnavailableReason(pod)
		}
		if reason == "" {
			continue
		}
		blocked++
		if len(reasons) < maxStalledNodes {
			reasons = append(reasons, fmt.Sprintf("node %s: %s", node, reason))
		}
	}
	if blocked > len(reasons) {
		reasons = append(reasons, fmt.Sprintf("%d more nodes", blocked-len(reasons)))
	}
	return strings.Join(reasons, ", ")
}

// podUnavailableReason tells why a daemon pod is not ready, from the most
// specific state the kubelet reports, or is empty when it is ready. It leaves
// out counters such as restarts, so that the status does not change on every
// sync of a large cluster.
func podUnavailableReason(pod *corev1.Pod) string {
	if pod.DeletionTimestamp != nil {
		return fmt.Sprintf("pod %s terminating since %s", pod.Name, pod.DeletionTimestamp.UTC().Format(time.RFC3339))
	}
	if _, ready := podReadySince(pod); ready {
		return ""
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
			return fmt.Sprintf("pod %s not scheduled: %s", pod.Name, cond.Message)
		}
	}
	for _, status := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
		if waiting := status.State.Waiting; waiting != nil && waiting.Reason != "" {
			return fmt.Sprintf("pod %s container %s %s", pod.Name, status.Name, waiting.Reason)
		}
		if terminated := status.State.Terminated; terminated != nil && terminated.ExitCode != 0 {
			return fmt.Sprintf("pod %s container %s exited with code %d", pod.Name, status.Name, terminated.ExitCode)
		}
	}
	return fmt.Sprintf("pod %s not ready in phase %s", pod.Name, pod.Status.Phase)
}

const (
	defaultMinReadySeconds         = 30
	defaultProgressDeadlineSeconds = 1200
)

// progressDeadline resolves the progress deadline of a rolling update. The API
// defaults it within rollingUpdate, which may be omitted.
func progressDeadline(ru *nvapi.ComputeDomainDaemonSetRollingUpdate) time.Duration {
	if ru == nil || ru.ProgressDeadlineSeconds <= 0 {
		return defaultProgressDeadlineSeconds * time.Second
	}
	return time.Duration(ru.ProgressDeadlineSeconds) * time.Second
}

// rolloutPlan is the next step of a rolling update.
type rolloutPlan struct {
	// cliques to restart, and their outdated daemon pods.
	cliques []string
	pods    []*corev1.Pod
	// requeueAfter is set while a clique waits for minReadySeconds.
	requeueAfter time.Duration
	// topology is the value of rollingUpdate.topologyKey being updated.
	topology string
	// stalled holds the cliques unavailable past the progress deadline.
	stalled []*staticClique
}

// planCliqueRollout restarts whole cliques together, at most
// maxUnavailableCliques at a time. Cliques that are updated but not yet
// available, or that were restarted recently, use up the batch. Unavailable
// outdated cliques go first, as their workloads are disrupted already. With a
// topology key, only the cliques of the first topology that is not yet updated
// and available are restarted. topologyOf maps nodes to their topology, and is
// nil without a topology key.
func planCliqueRollout(cliques []*staticClique, unclustered []*corev1.Pod, hash string, ru *nvapi.ComputeDomainDaemonSetRollingUpdate, nodeReady map[string]bool, topologyOf map[string]string, restarting map[string]bool, now time.Time) rolloutPlan {
	var plan rolloutPlan
	var outdatedUnclustered []*corev1.Pod
	for _, pod := range unclustered {
		if !podUpdated(pod, hash) && pod.DeletionTimestamp == nil {
			outdatedUnclustered = append(outdatedUnclustered, pod)
		}
	}

	// The API defaults minReadySeconds within rollingUpdate, which may be
	// omitted.
	minReady := defaultMinReadySeconds * time.Second
	if ru != nil {
		minReady = time.Duration(ru.MinReadySeconds) * time.Second
	}

	inProgress := 0
	requeueAfter := time.Duration(math.MaxInt64)
	var unavailableCandidates, candidates []*staticClique
	// pending holds the topologies with cliques that are not yet updated and
	// available.
	var pending []string
	for _, pod := range outdatedUnclustered {
		pending = append(pending, topologyOf[pod.Spec.NodeName])
	}
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
			// A stalled clique keeps its place in the batch, so that a broken
			// revision stops the rollout instead of spreading.
			if !restarting[c.id] && wait == 0 && !c.lastChange().IsZero() && now.Sub(c.lastChange()) > progressDeadline(ru) {
				if c.stalled = c.unavailableReason(nodeReady); c.stalled != "" {
					plan.stalled = append(plan.stalled, c)
				}
			}
		default:
			continue
		}
		pending = append(pending, c.topology)
	}
	if requeueAfter != math.MaxInt64 {
		plan.requeueAfter = requeueAfter
	}

	if topologyOf != nil && len(pending) > 0 {
		plan.topology = slices.MinFunc(pending, compareTopology)
		inTopology := func(c *staticClique) bool { return c.topology == plan.topology }
		unavailableCandidates = slices.DeleteFunc(unavailableCandidates, func(c *staticClique) bool { return !inTopology(c) })
		candidates = slices.DeleteFunc(candidates, func(c *staticClique) bool { return !inTopology(c) })
		outdatedUnclustered = slices.DeleteFunc(outdatedUnclustered, func(pod *corev1.Pod) bool {
			return topologyOf[pod.Spec.NodeName] != plan.topology
		})
	}
	// Pods without a clique run no IMEX daemon, so restarting them disrupts
	// nothing.
	plan.pods = outdatedUnclustered

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
