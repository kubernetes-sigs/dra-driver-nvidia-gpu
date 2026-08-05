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
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

const (
	// cdStatusSyncInterval is how often to sync node info to CD status and clean up stale clique entries.
	cdStatusSyncInterval = 2 * time.Second
)

// ComputeDomainStatusManager synchronizes node information to ComputeDomain status from
// both CDCliques (fabric-attached nodes) and daemon pods (non-fabric-attached nodes).
type ComputeDomainStatusManager struct {
	config        *ManagerConfig
	waitGroup     sync.WaitGroup
	cancelContext context.CancelFunc

	cliqueManager *ComputeDomainCliqueManager
	podManager    *DaemonSetPodManager

	getComputeDomain          GetComputeDomainFunc
	listComputeDomains        ListComputeDomainsFunc
	updateComputeDomainStatus UpdateComputeDomainStatusFunc
}

// NewComputeDomainStatusManager creates a new ComputeDomainStatusManager.
func NewComputeDomainStatusManager(config *ManagerConfig, getComputeDomain GetComputeDomainFunc, listComputeDomains ListComputeDomainsFunc, updateComputeDomainStatus UpdateComputeDomainStatusFunc) *ComputeDomainStatusManager {
	// Create cliqueManager if feature gate is enabled
	var cliqueManager *ComputeDomainCliqueManager
	if featuregates.Enabled(featuregates.ComputeDomainCliques) {
		cliqueManager = NewComputeDomainCliqueManager(config)
	}

	// Create podManager
	podManager := NewDaemonSetPodManager(config)

	return &ComputeDomainStatusManager{
		config:                    config,
		cliqueManager:             cliqueManager,
		podManager:                podManager,
		getComputeDomain:          getComputeDomain,
		listComputeDomains:        listComputeDomains,
		updateComputeDomainStatus: updateComputeDomainStatus,
	}
}

// Start starts the ComputeDomainStatusManager.
func (m *ComputeDomainStatusManager) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	m.cancelContext = cancel

	// Start cliqueManager if it exists
	if m.cliqueManager != nil {
		if err := m.cliqueManager.Start(ctx); err != nil {
			return fmt.Errorf("error starting ComputeDomainClique manager: %w", err)
		}
	}

	// Start podManager
	if err := m.podManager.Start(ctx); err != nil {
		return fmt.Errorf("error starting DaemonSetPod manager: %w", err)
	}

	klog.Info("ComputeDomainStatusManager: starting periodic sync")

	// Start periodic sync loop (also handles clique cleanup when feature gate is enabled)
	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		m.startPeriodicSync(ctx)
	}()

	return nil
}

// Stop stops the ComputeDomainStatusManager.
func (m *ComputeDomainStatusManager) Stop() error {
	if err := m.podManager.Stop(); err != nil {
		klog.Errorf("error stopping DaemonSetPod manager: %v", err)
	}
	if m.cliqueManager != nil {
		if err := m.cliqueManager.Stop(); err != nil {
			klog.Errorf("error stopping ComputeDomainClique manager: %v", err)
		}
	}
	if m.cancelContext != nil {
		m.cancelContext()
	}
	m.waitGroup.Wait()
	return nil
}

// startPeriodicSync runs the sync every cdStatusSyncInterval.
func (m *ComputeDomainStatusManager) startPeriodicSync(ctx context.Context) {
	ticker := time.NewTicker(cdStatusSyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.sync(ctx)
		}
	}
}

// sync synchronizes node information to all ComputeDomain statuses.
func (m *ComputeDomainStatusManager) sync(ctx context.Context) {
	// Get all ComputeDomains
	cds, err := m.listComputeDomains()
	if err != nil {
		klog.Errorf("CDStatusSync: error listing ComputeDomains: %v", err)
		return
	}

	// Get all daemon pods
	pods, err := m.podManager.List()
	if err != nil {
		klog.Errorf("CDStatusSync: error listing pods: %v", err)
		return
	}

	// Get fabric-attached nodes from cliques (if feature gate is enabled)
	var cliques []*nvapi.ComputeDomainClique
	if m.cliqueManager != nil {
		cliques, err = m.cliqueManager.List()
		if err != nil {
			klog.Errorf("CDStatusSync: error listing cliques: %v", err)
			return
		}
	}

	// Group cliques by CD UID (used for status sync and per-clique daemon cleanup)
	cliquesByCD := make(map[string][]*nvapi.ComputeDomainClique)
	for _, clique := range cliques {
		cdUID := clique.Labels[computeDomainLabelKey]
		if cdUID == "" {
			continue
		}
		cliquesByCD[cdUID] = append(cliquesByCD[cdUID], clique)
	}

	if m.cliqueManager != nil {
		// Clean up stale entries from cliques in parallel (pods scoped per clique)
		for _, clique := range cliques {
			cdUID := clique.Labels[computeDomainLabelKey]
			if cdUID == "" {
				continue
			}
			go m.cleanupClique(ctx, clique, pods, len(cliquesByCD[cdUID]))
		}
	}

	// Group pods by CD UID and type (fabric-attached vs non-fabric-attached)
	fabricPodsByCD := make(map[string][]*corev1.Pod)
	nonFabricPodsByCD := make(map[string][]*corev1.Pod)
	for _, pod := range pods {
		cdUID := pod.Labels[computeDomainLabelKey]
		if cdUID == "" {
			continue
		}

		// Separate pods based on cliqueID label
		cliqueID, exists := pod.Labels[computeDomainCliqueLabelKey]
		if !exists || cliqueID != "" {
			// Unlabeled or fabric-attached: treat as fabric pods
			fabricPodsByCD[cdUID] = append(fabricPodsByCD[cdUID], pod)
		} else {
			// Explicitly empty cliqueID: non-fabric pods
			nonFabricPodsByCD[cdUID] = append(nonFabricPodsByCD[cdUID], pod)
		}
	}

	// Sync each CD in parallel
	var wg sync.WaitGroup
	for _, cd := range cds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.syncCD(ctx, cd, cliquesByCD[string(cd.UID)], fabricPodsByCD[string(cd.UID)], nonFabricPodsByCD[string(cd.UID)])
		}()
	}
	wg.Wait()
}

// syncCD synchronizes node information to a single ComputeDomain's status.
func (m *ComputeDomainStatusManager) syncCD(ctx context.Context, cd *nvapi.ComputeDomain, cliques []*nvapi.ComputeDomainClique, fabricPods []*corev1.Pod, nonFabricPods []*corev1.Pod) {
	latestCD, err := m.getComputeDomain(string(cd.UID))
	if err != nil {
		klog.Errorf("CDStatusSync: error getting ComputeDomain %s: %v", cd.Name, err)
		return
	}
	if latestCD == nil {
		return
	}

	var fabricNodes, nonFabricNodes, newNodes []*nvapi.ComputeDomainNode

	if m.cliqueManager != nil {
		// Feature gate enabled: build from cliques + non-fabric pods
		fabricNodes = m.buildNodesFromCliques(cliques)
		nonFabricNodes = m.buildNodesFromPods(nonFabricPods)
		newNodes = slices.Concat(fabricNodes, nonFabricNodes)
	} else {
		// Feature gate disabled: filter stale fabric nodes + rebuild non-fabric nodes
		fabricNodes = m.getNonStaleFabricNodes(latestCD.Status.Nodes, fabricPods)
		nonFabricNodes = m.buildNodesFromPods(nonFabricPods)
		newNodes = slices.Concat(fabricNodes, nonFabricNodes)
	}

	if m.nodesEqual(latestCD.Status.Nodes, newNodes) {
		return
	}

	klog.V(6).Infof("CDStatusSync: syncing ComputeDomain %s/%s: fabric=%d non-fabric=%d", latestCD.Namespace, latestCD.Name, len(fabricNodes), len(nonFabricNodes))

	// Update status (use latest object for resourceVersion)
	newCD := latestCD.DeepCopy()
	newCD.Status.Nodes = newNodes
	if _, err := m.updateComputeDomainStatus(ctx, newCD); err != nil {
		klog.Errorf("CDStatusSync: error updating ComputeDomain %s status: %v", latestCD.Name, err)
		return
	}

	klog.V(4).Infof("CDStatusSync: updated ComputeDomain %s/%s: total nodes=%d", latestCD.Namespace, latestCD.Name, len(newNodes))
}

// buildNodesFromCliques builds a nodes list from fabric-attached cliques.
func (m *ComputeDomainStatusManager) buildNodesFromCliques(cliques []*nvapi.ComputeDomainClique) []*nvapi.ComputeDomainNode {
	var result []*nvapi.ComputeDomainNode
	for _, clique := range cliques {
		for _, daemon := range clique.Daemons {
			result = append(result, &nvapi.ComputeDomainNode{
				Name:      daemon.NodeName,
				IPAddress: daemon.IPAddress,
				CliqueID:  daemon.CliqueID,
				Index:     daemon.Index,
				Status:    daemon.Status,
			})
		}
	}
	return result
}

// buildNodesFromPods builds ComputeDomainNode entries from non-fabric-attached pods.
func (m *ComputeDomainStatusManager) buildNodesFromPods(pods []*corev1.Pod) []*nvapi.ComputeDomainNode {
	var nodes []*nvapi.ComputeDomainNode
	for _, pod := range pods {
		if pod.Spec.NodeName == "" || pod.Status.PodIP == "" {
			continue
		}

		status := nvapi.ComputeDomainStatusNotReady
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				status = nvapi.ComputeDomainStatusReady
				break
			}
		}

		nodes = append(nodes, &nvapi.ComputeDomainNode{
			Name:      pod.Spec.NodeName,
			IPAddress: pod.Status.PodIP,
			CliqueID:  "",
			Index:     -1,
			Status:    status,
		})
	}
	return nodes
}

// fabricCliqueIDFromClique returns the fabric clique ID from object labels, or from
// metadata name "<computeDomainUID>.<cliqueID>" when the clique ID label is unset.
func fabricCliqueIDFromClique(clique *nvapi.ComputeDomainClique) string {
	if clique == nil {
		return ""
	}
	if id := clique.Labels[computeDomainCliqueLabelKey]; id != "" {
		return id
	}
	cdUID := clique.Labels[computeDomainLabelKey]
	if cdUID == "" {
		return ""
	}
	prefix := cdUID + "."
	if strings.HasPrefix(clique.Name, prefix) {
		return strings.TrimPrefix(clique.Name, prefix)
	}
	return ""
}

// podCountsForCliqueFabricDaemon is true when this pod is the fabric-attached daemon
// for the same ComputeDomain and fabric clique as clique. Non-fabric pods (explicit
// empty clique label) are excluded. Pods without a clique label only match when the
// CD has a single fabric clique so attribution is unambiguous.
func podCountsForCliqueFabricDaemon(pod *corev1.Pod, clique *nvapi.ComputeDomainClique, fabricCliqueCountForCD int) bool {
	if pod == nil || clique == nil {
		return false
	}
	cdUID := clique.Labels[computeDomainLabelKey]
	if cdUID == "" || pod.Labels[computeDomainLabelKey] != cdUID {
		return false
	}
	podCliqueID, podHasCliqueLabel := pod.Labels[computeDomainCliqueLabelKey]
	if podHasCliqueLabel && podCliqueID == "" {
		return false
	}
	expected := fabricCliqueIDFromClique(clique)
	if expected == "" {
		return false
	}
	if podHasCliqueLabel && podCliqueID != "" {
		return podCliqueID == expected
	}
	return fabricCliqueCountForCD == 1
}

// cleanupClique removes stale daemon entries from a single clique.
func (m *ComputeDomainStatusManager) cleanupClique(ctx context.Context, clique *nvapi.ComputeDomainClique, pods []*corev1.Pod, fabricCliqueCountForCD int) {
	cdUID := clique.Labels[computeDomainLabelKey]
	cliqueID := fabricCliqueIDFromClique(clique)
	if cdUID == "" || cliqueID == "" {
		return
	}

	ns, name := clique.Namespace, clique.Name
	if ns == "" || name == "" {
		return
	}

	// Quick exit if cache already matches desired state (avoids live Get on every tick).
	if cached := m.cliqueManager.Get(cdUID, cliqueID); cached != nil {
		if running := runningFabricNodesForClique(pods, cached, fabricCliqueCountForCD); daemonsEqual(cached.Daemons, filterDaemonsByRunningNodes(cached.Daemons, running)) {
			return
		}
	}

	var removedLogged bool
	var lastRemoved []string
	var updateSucceeded bool
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		live, err := m.cliqueManager.GetLive(ctx, name)
		if err != nil {
			return err
		}
		runningNodes := runningFabricNodesForClique(pods, live, fabricCliqueCountForCD)
		updatedDaemons := filterDaemonsByRunningNodes(live.Daemons, runningNodes)
		if daemonsEqual(live.Daemons, updatedDaemons) {
			return nil
		}
		var removedNodes []string
		for _, daemon := range live.Daemons {
			if _, exists := runningNodes[daemon.NodeName]; !exists {
				removedNodes = append(removedNodes, daemon.NodeName)
			}
		}
		if !removedLogged {
			klog.Infof("CliqueCleanup: removing stale daemon entries from clique %s/%s: %v", ns, name, removedNodes)
			removedLogged = true
		}
		lastRemoved = removedNodes

		newClique := live.DeepCopy()
		newClique.Daemons = updatedDaemons
		_, err = m.cliqueManager.Update(ctx, newClique)
		if err == nil {
			updateSucceeded = true
		}
		return err
	})
	if err != nil {
		klog.Errorf("CliqueCleanup: error updating ComputeDomainClique %s/%s: %v", ns, name, err)
		return
	}
	if updateSucceeded {
		klog.Infof("CliqueCleanup: successfully removed %d stale daemon entries from clique %s/%s", len(lastRemoved), ns, name)
	}
}

func runningFabricNodesForClique(pods []*corev1.Pod, clique *nvapi.ComputeDomainClique, fabricCliqueCountForCD int) map[string]struct{} {
	runningNodes := make(map[string]struct{})
	for _, pod := range pods {
		if pod.Spec.NodeName == "" {
			continue
		}
		if podCountsForCliqueFabricDaemon(pod, clique, fabricCliqueCountForCD) {
			runningNodes[pod.Spec.NodeName] = struct{}{}
		}
	}
	return runningNodes
}

func filterDaemonsByRunningNodes(daemons []*nvapi.ComputeDomainDaemonInfo, runningNodes map[string]struct{}) []*nvapi.ComputeDomainDaemonInfo {
	var out []*nvapi.ComputeDomainDaemonInfo
	for _, daemon := range daemons {
		if _, exists := runningNodes[daemon.NodeName]; exists {
			out = append(out, daemon)
		}
	}
	return out
}

// filterStaleNodes removes nodes from CD status if their pod no longer exists.
// It filters the existing nodes list to only keep those with a corresponding pod in the pods list.
// getNonStaleFabricNodes returns fabric-attached nodes from existingNodes that still have running pods.
// Non-fabric nodes are filtered out (they'll be rebuilt from nonFabricPods).
func (m *ComputeDomainStatusManager) getNonStaleFabricNodes(existingNodes []*nvapi.ComputeDomainNode, fabricPods []*corev1.Pod) []*nvapi.ComputeDomainNode {
	// Build set of fabric pod IPs
	fabricPodIPs := make(map[string]struct{})
	for _, pod := range fabricPods {
		if pod.Status.PodIP != "" {
			fabricPodIPs[pod.Status.PodIP] = struct{}{}
		}
	}

	// Keep only fabric nodes (CliqueID != "") that still have pods
	var result []*nvapi.ComputeDomainNode
	for _, node := range existingNodes {
		// Skip non-fabric nodes (they're rebuilt fresh)
		if node.CliqueID == "" {
			continue
		}
		// Keep fabric node if its pod still exists
		if _, exists := fabricPodIPs[node.IPAddress]; exists {
			result = append(result, node)
		}
	}

	return result
}

// daemonsEqual checks if two daemon slices are semantically equal (per nodeName key).
func daemonsEqual(a, b []*nvapi.ComputeDomainDaemonInfo) bool {
	aMap := make(map[string]nvapi.ComputeDomainDaemonInfo)
	for _, d := range a {
		if d != nil {
			aMap[d.NodeName] = *d
		}
	}
	bMap := make(map[string]nvapi.ComputeDomainDaemonInfo)
	for _, d := range b {
		if d != nil {
			bMap[d.NodeName] = *d
		}
	}
	return maps.Equal(aMap, bMap)
}

// nodesEqual checks if two slices of ComputeDomainNode are equal.
func (m *ComputeDomainStatusManager) nodesEqual(a, b []*nvapi.ComputeDomainNode) bool {
	aMap := make(map[string]nvapi.ComputeDomainNode)
	for _, node := range a {
		aMap[node.Name] = *node
	}
	bMap := make(map[string]nvapi.ComputeDomainNode)
	for _, node := range b {
		bMap[node.Name] = *node
	}
	return maps.Equal(aMap, bMap)
}
