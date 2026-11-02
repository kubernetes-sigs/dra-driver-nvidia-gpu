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
	"context"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
)

// nodeReleaseDelay lets the caches catch up before a node whose claims went
// away is released.
const nodeReleaseDelay = 10 * time.Second

// claimNode makes the ComputeDomain the owner of the node, through the node
// label that also places its IMEX daemon there. The kubelet plugin publishes
// the owner on its channel device, so the scheduler skips the node for other
// ComputeDomains from then on, rather than only after their claims are
// rejected. It returns another owner the cache did not show yet.
func (m *ClaimAdmissionManager) claimNode(ctx context.Context, name, domainID string) (string, error) {
	if m.cachedNodeOwner(name) == domainID {
		return "", nil
	}
	node, err := m.config.clientsets.Core.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("error getting Node %s: %w", name, err)
	}
	switch owner := node.Labels[computeDomainLabelKey]; owner {
	case domainID:
		return "", nil
	case "":
	default:
		return owner, nil
	}
	node = node.DeepCopy()
	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}
	node.Labels[computeDomainLabelKey] = domainID
	if _, err := m.config.clientsets.Core.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		return "", fmt.Errorf("error labeling Node %s for ComputeDomain %s: %w", name, domainID, err)
	}
	klog.V(2).Infof("Node %s is owned by ComputeDomain %s", name, domainID)
	return "", nil
}

func (m *ClaimAdmissionManager) cachedNodeOwner(name string) string {
	obj, exists, err := m.nodeInformer.GetStore().GetByKey(name)
	if err != nil || !exists {
		return ""
	}
	node, ok := obj.(*corev1.Node)
	if !ok {
		return ""
	}
	return node.Labels[computeDomainLabelKey]
}

// enqueueReleases queues the nodes a claim no longer holds a channel on.
func (m *ClaimAdmissionManager) enqueueReleases(oldObj, newObj any) {
	if !m.ownsNodes() {
		return
	}
	oldNodes, _ := indexClaimByNode(oldObj)
	var newNodes []string
	if newObj != nil {
		newNodes, _ = indexClaimByNode(newObj)
	}
	for _, node := range oldNodes {
		if !slices.Contains(newNodes, node) {
			m.releaseQueue.AddAfter(node, m.releaseDelay)
		}
	}
}

// releaseNode removes the owner of a node that no claim of its ComputeDomain
// uses. Normally the kubelet plugin removes it after the last claim is
// unprepared. This covers claims admitted but never prepared, for example of
// pods deleted before they started.
func (m *ClaimAdmissionManager) releaseNode(ctx context.Context, name string) error {
	unlock := m.lockNode(name)
	defer unlock()

	owner := m.cachedNodeOwner(name)
	if owner == "" || m.domainUsesNode(name, owner) {
		return nil
	}
	node, err := m.config.clientsets.Core.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting Node %s: %w", name, err)
	}
	if node.Labels[computeDomainLabelKey] != owner {
		return nil
	}
	node = node.DeepCopy()
	delete(node.Labels, computeDomainLabelKey)
	if _, err := m.config.clientsets.Core.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("error releasing Node %s from ComputeDomain %s: %w", name, owner, err)
	}
	klog.V(2).Infof("Released Node %s from ComputeDomain %s, no claims use it", name, owner)
	return nil
}

// domainUsesNode reports whether a claim of the ComputeDomain holds a
// channel on the node that was not rejected, or is being admitted there.
func (m *ClaimAdmissionManager) domainUsesNode(node, domainID string) bool {
	m.inflightMu.Lock()
	for _, other := range m.inflight[node] {
		if other == domainID {
			m.inflightMu.Unlock()
			return true
		}
	}
	m.inflightMu.Unlock()

	for _, claim := range m.claimsByIndex(claimNodeIndex, node) {
		for _, result := range channelResults(claim) {
			if result.Pool != node || channelDomainID(claim, result.Request) != domainID {
				continue
			}
			status := deviceStatus(claim, &result)
			if status == nil || !meta.IsStatusConditionTrue(status.Conditions, imex.ChannelRejectedCondition) {
				return true
			}
		}
	}
	return false
}

func (m *ClaimAdmissionManager) processNextRelease(ctx context.Context) bool {
	node, shutdown := m.releaseQueue.Get()
	if shutdown {
		return false
	}
	defer m.releaseQueue.Done(node)

	if err := m.releaseNode(ctx, node); err != nil {
		klog.V(1).Infof("Release of Node %s: %v (attempt %d)", node, err, m.releaseQueue.NumRequeues(node))
		m.releaseQueue.AddRateLimited(node)
		return true
	}
	m.releaseQueue.Forget(node)
	return true
}
