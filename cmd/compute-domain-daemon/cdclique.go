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
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	nvclique "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	nvinformers "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/informers/externalversions"
)

// ComputeDomainCliqueManager watches ComputeDomainClique objects and updates them with
// info about the ComputeDomain daemon running on this node. This is an alternative
// to ComputeDomainStatusManager that works directly with CDClique objects instead
// of writing to ComputeDomain.Status.Nodes.
type ComputeDomainCliqueManager struct {
	config        *ManagerConfig
	waitGroup     sync.WaitGroup
	cancelContext context.CancelFunc

	factory  nvinformers.SharedInformerFactory
	informer cache.SharedIndexInformer

	// Note: if `previousDaemons` is empty it means we're early in the daemon's
	// lifecycle and the IMEX daemon child process wasn't started yet.
	previousDaemons    []*nvclique.ComputeDomainDaemonInfo
	updatedDaemonsChan chan []*nvclique.ComputeDomainDaemonInfo

	podManager    *PodManager
	mutationCache cache.MutationCache
}

// NewComputeDomainCliqueManager creates a new ComputeDomainCliqueManager instance.
func NewComputeDomainCliqueManager(config *ManagerConfig) *ComputeDomainCliqueManager {
	m := &ComputeDomainCliqueManager{
		config:             config,
		previousDaemons:    []*nvclique.ComputeDomainDaemonInfo{},
		updatedDaemonsChan: make(chan []*nvclique.ComputeDomainDaemonInfo),
	}

	m.factory = nvinformers.NewSharedInformerFactoryWithOptions(
		config.clientsets.Nvidia,
		informerResyncPeriod,
		nvinformers.WithNamespace(config.podNamespace), // CDClique is in the driver namespace
		nvinformers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = fmt.Sprintf("metadata.name=%s", m.cliqueName())
		}),
	)
	m.informer = m.factory.Resource().V1beta1().ComputeDomainCliques().Informer()

	m.podManager = NewPodManager(m.config, m.updateDaemonStatus)

	return m
}

// Start starts the CDClique manager.
func (m *ComputeDomainCliqueManager) Start(ctx context.Context) (rerr error) {
	ctx, cancel := context.WithCancel(ctx)
	m.cancelContext = cancel

	defer func() {
		if rerr != nil {
			if err := m.Stop(); err != nil {
				klog.Errorf("error stopping ComputeDomainCliqueManager: %v", err)
			}
		}
	}()

	// Create mutation cache to track our own updates
	m.mutationCache = cache.NewIntegerResourceVersionMutationCache(
		klog.Background(),
		m.informer.GetStore(),
		m.informer.GetIndexer(),
		mutationCacheTTL,
		true,
	)

	// Use `WithKey` with hard-coded key, to cancel any previous update task (we
	// want to make sure that the latest CDClique update wins).
	_, err := m.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			m.config.workQueue.EnqueueWithKey(obj, "cdclique", m.onAddOrUpdate)
		},
		UpdateFunc: func(objOld, objNew any) {
			m.config.workQueue.EnqueueWithKey(objNew, "cdclique", m.onAddOrUpdate)
		},
	})
	if err != nil {
		return fmt.Errorf("error adding event handlers for ComputeDomainClique informer: %w", err)
	}

	// Enqueue clique creation to the workqueue - it will be processed with
	// retries when the workqueue starts running.
	ensureCliqueExists := func(ctx context.Context, _ any) error {
		return m.ensureCliqueExists(ctx)
	}
	m.config.workQueue.EnqueueRawWithKey(nil, "cdclique", ensureCliqueExists)

	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		m.factory.Start(ctx.Done())
	}()

	if !cache.WaitForCacheSync(ctx.Done(), m.informer.HasSynced) {
		return fmt.Errorf("informer cache sync for ComputeDomainCliques failed")
	}

	if err := m.podManager.Start(ctx); err != nil {
		return fmt.Errorf("failed to start pod manager: %w", err)
	}

	return nil
}

// Stop stops the CDClique manager.
//
//nolint:contextcheck
func (m *ComputeDomainCliqueManager) Stop() error {
	// Stop the pod manager first
	if err := m.podManager.Stop(); err != nil {
		klog.Errorf("Failed to stop pod manager: %v", err)
	}

	// Create a new context for cleanup operations since the original context might be cancelled
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cleanupCancel()

	// Attempt to remove this daemon from the CDClique before shutting down
	// Don't return error here as we still want to proceed with shutdown
	if err := m.removeDaemonInfoFromClique(cleanupCtx); err != nil {
		klog.Errorf("Failed to remove daemon info from CDClique during shutdown: %v", err)
	}

	if m.cancelContext != nil {
		m.cancelContext()
	}

	m.waitGroup.Wait()
	return nil
}

// GetDaemonInfoUpdateChan returns the channel that yields daemon info updates.
func (m *ComputeDomainCliqueManager) GetDaemonInfoUpdateChan() chan []*nvclique.ComputeDomainDaemonInfo {
	return m.updatedDaemonsChan
}

// cliqueName returns the name of the ComputeDomainClique object this daemon manages,
// constructed as "<computeDomainUUID>.<cliqueID>".
func (m *ComputeDomainCliqueManager) cliqueName() string {
	return fmt.Sprintf("%s.%s", m.config.computeDomainUUID, m.config.cliqueID)
}

// getClique gets the ComputeDomainClique from the mutation cache by namespace/name key.
func (m *ComputeDomainCliqueManager) getClique() (*nvclique.ComputeDomainClique, error) {
	key := fmt.Sprintf("%s/%s", m.config.podNamespace, m.cliqueName())
	obj, exists, err := m.mutationCache.GetByKey(key)
	if err != nil {
		return nil, fmt.Errorf("error retrieving ComputeDomainClique: %w", err)
	}
	if !exists {
		return nil, nil
	}
	clique, ok := obj.(*nvclique.ComputeDomainClique)
	if !ok {
		return nil, fmt.Errorf("unexpected object type in cache")
	}
	return clique, nil
}

// ensureCliqueExists creates the CDClique if it doesn't exist.
func (m *ComputeDomainCliqueManager) ensureCliqueExists(ctx context.Context) error {
	// Check if the clique already exists (created by us or another daemon)
	clique, err := m.getClique()
	if err != nil {
		return fmt.Errorf("failed to get CDClique '%s': %w", m.cliqueName(), err)
	}
	if clique != nil {
		klog.Infof("CDClique '%s' already exists", m.cliqueName())
		return nil
	}

	// Create the CDClique
	newClique := &nvclique.ComputeDomainClique{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.cliqueName(),
			Namespace: m.config.podNamespace,
			Labels: map[string]string{
				computeDomainLabelKey:       m.config.computeDomainUUID,
				computeDomainCliqueLabelKey: m.config.cliqueID,
			},
		},
	}
	m.ensureOwnerReference(newClique)

	createdClique, err := m.config.clientsets.Nvidia.ResourceV1beta1().ComputeDomainCliques(m.config.podNamespace).Create(ctx, newClique, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create CDClique '%s': %w", m.cliqueName(), err)
	}
	m.mutationCache.Mutation(createdClique)

	klog.Infof("Successfully created CDClique '%s'", m.cliqueName())
	return nil
}

// onAddOrUpdate handles the addition or update of a ComputeDomainClique. Here, we
// receive updates not for all CDCliques in the system, but only for the one that we
// are registered for (filtered by name). Note that the informer triggers this
// callback once upon startup for all existing objects.
func (m *ComputeDomainCliqueManager) onAddOrUpdate(ctx context.Context, obj any) error {
	// Cast the object to a ComputeDomainClique object
	o, ok := obj.(*nvclique.ComputeDomainClique)
	if !ok {
		return fmt.Errorf("failed to cast to ComputeDomainClique")
	}

	// Skip if this isn't our clique (should not happen with name filter, but be safe)
	if o.Name != m.cliqueName() {
		return nil
	}

	// Get the latest CDClique object from the mutation cache (backed by
	// the informer cache) since we plan to update it later and always *must*
	// have the latest version.
	clique, err := m.getClique()
	if err != nil {
		return fmt.Errorf("failed to get CDClique: %w", err)
	}
	if clique == nil {
		return nil
	}

	// Skip CDCliques that don't match on ComputeDomain UUID
	if clique.Labels[computeDomainLabelKey] != m.config.computeDomainUUID {
		klog.Warningf("CDClique processed with non-matching ComputeDomain UID (%v, %v)", clique.Labels[computeDomainLabelKey], m.config.computeDomainUUID)
		return nil
	}

	// Update daemon info in CDClique, if required.
	clique, err = m.syncDaemonInfoToClique(ctx, clique)
	if err != nil {
		return fmt.Errorf("CDClique update: failed to insert/update daemon info: %w", err)
	}
	m.maybePushDaemonsUpdate(clique)

	return nil
}

// syncDaemonInfoToClique makes sure that the current daemon (by node name) is
// represented in the `Daemons` field in the CDClique object, and that it
// reports the IP address of this current pod running the CD daemon. If mutation
// is needed (first insertion, or IP address update) and successful, it reflects
// the mutation in `m.mutationCache`.
func (m *ComputeDomainCliqueManager) syncDaemonInfoToClique(ctx context.Context, clique *nvclique.ComputeDomainClique) (*nvclique.ComputeDomainClique, error) {
	var myDaemon *nvclique.ComputeDomainDaemonInfo

	// Create a deep copy of the CDClique to avoid modifying the original
	newClique := clique.DeepCopy()

	// Try to find an existing entry for the current k8s node
	for _, d := range newClique.Daemons {
		if d.NodeName == m.config.nodeName {
			myDaemon = d
			break
		}
	}

	// If there is one and its IP is the same as this one, we are done
	if myDaemon != nil && myDaemon.IPAddress == m.config.podIP {
		klog.V(6).Infof("syncDaemonInfoToClique noop: pod IP unchanged (%s)", m.config.podIP)
		return newClique, nil
	}

	// If there isn't one, create one and append it to the list
	if myDaemon == nil {
		// Get the next available index for this new daemon
		nextIndex, err := m.getNextAvailableIndex(newClique.Daemons)
		if err != nil {
			return nil, fmt.Errorf("error getting next available index: %w", err)
		}

		myDaemon = &nvclique.ComputeDomainDaemonInfo{
			NodeName: m.config.nodeName,
			CliqueID: m.config.cliqueID,
			Index:    nextIndex,
			// This is going to be switched to Ready by podmanager.
			Status: nvclique.ComputeDomainStatusNotReady,
		}

		klog.Infof("CDClique does not contain node name '%s' yet, try to insert myself: %v", m.config.nodeName, myDaemon)
		newClique.Daemons = append(newClique.Daemons, myDaemon)
	}

	// Unconditionally update its IP address. Note that the myDaemon.IPAddress
	// as of now translates into a pod IP address and may therefore change
	// across pod restarts.
	myDaemon.IPAddress = m.config.podIP

	// Ensure this pod is an owner of the clique
	m.ensureOwnerReference(newClique)

	// Update the clique and (upon success) store the latest version of the object
	// (as returned by the API server) in the mutation cache.
	newClique, err := m.config.clientsets.Nvidia.ResourceV1beta1().ComputeDomainCliques(m.config.podNamespace).Update(ctx, newClique, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("error updating CDClique: %w", err)
	}
	m.mutationCache.Mutation(newClique)

	klog.Infof("Successfully inserted/updated daemon info in CDClique %s (myDaemon: %v)", m.cliqueName(), myDaemon)
	return newClique, nil
}

// The Index field in the Daemons section of the CDClique ensures a consistent
// IP-to-DNS name mapping across all machines within a given IMEX domain. Each
// daemon's index directly determines its DNS name using the format
// "compute-domain-daemon-{index}".
//
// getNextAvailableIndex finds the next available index for the current daemon by
// seeing which ones are already taken by other daemons in the CDClique. It fills
// in gaps where it can, and returns an error if no index is available within
// maxNodesPerIMEXDomain.
//
// By filling gaps in the index sequence (rather than always appending), we
// maintain stable DNS names for existing daemons even when intermediate daemons
// are removed from the clique and new ones are added.
func (m *ComputeDomainCliqueManager) getNextAvailableIndex(daemons []*nvclique.ComputeDomainDaemonInfo) (int, error) {
	// Create a map to track used indices
	usedIndices := make(map[int]bool)

	// Collect all currently used indices
	for _, d := range daemons {
		usedIndices[d.Index] = true
	}

	// Find the next available index, starting from 0 and filling gaps
	nextIndex := 0
	for usedIndices[nextIndex] {
		nextIndex++
	}

	// Ensure nextIndex is within the allowed range
	if nextIndex < 0 || nextIndex >= m.config.maxNodesPerIMEXDomain {
		return -1, fmt.Errorf("no available indices within maxNodesPerIMEXDomain (%d)", m.config.maxNodesPerIMEXDomain)
	}

	return nextIndex, nil
}

// removeDaemonInfoFromClique removes the current daemon info entry from the CDClique.
func (m *ComputeDomainCliqueManager) removeDaemonInfoFromClique(ctx context.Context) error {
	clique, err := m.getClique()
	if err != nil {
		return fmt.Errorf("failed to get CDClique: %w", err)
	}
	if clique == nil {
		// Clique doesn't exist, nothing to remove
		return nil
	}

	// Create a deep copy and filter out the daemon with the current pod's IP address
	newClique := clique.DeepCopy()
	var newDaemons []*nvclique.ComputeDomainDaemonInfo
	for _, d := range newClique.Daemons {
		if d.IPAddress != m.config.podIP {
			newDaemons = append(newDaemons, d)
		}
	}
	newClique.Daemons = newDaemons

	// Update the clique and (upon success) store the latest version of the object
	// (as returned by the API server) in the mutation cache.
	newClique, err = m.config.clientsets.Nvidia.ResourceV1beta1().ComputeDomainCliques(m.config.podNamespace).Update(ctx, newClique, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error updating CDClique: %w", err)
	}
	m.mutationCache.Mutation(newClique)

	klog.Infof("Successfully removed daemon with IP %s from CDClique %s (from ComputeDomain %s/%s)", m.config.podIP, m.cliqueName(), m.config.computeDomainNamespace, m.config.computeDomainName)
	return nil
}

// If there was actually a change compared to the previously known set of
// daemons: pass info to IMEX daemon controller.
func (m *ComputeDomainCliqueManager) maybePushDaemonsUpdate(clique *nvclique.ComputeDomainClique) {
	newIPs := m.getIPSet(clique.Daemons)
	previousIPs := m.getIPSet(m.previousDaemons)

	// Compare sets (i.e., without paying attention to order). Note: the order
	// of IP addresses written to the IMEX daemon's config file might matter (in
	// the sense that if across config files the set is equal but the order is
	// not: that may lead to an IMEX daemon startup error). Maybe we should
	// perform a stable sort of IP addresses before writing them to the nodes
	// config file.
	if !maps.Equal(newIPs, previousIPs) {
		added, removed := previousIPs.Diff(newIPs)
		klog.V(2).Infof("IP set for clique changed.\nAdded: %v\nRemoved: %v", added, removed)
		m.previousDaemons = clique.Daemons
		m.updatedDaemonsChan <- clique.Daemons
	} else {
		klog.V(6).Infof("IP set for clique did not change")
	}
}

// updateDaemonStatus updates the daemon info status based on pod readiness.
func (m *ComputeDomainCliqueManager) updateDaemonStatus(ctx context.Context, ready bool) error {
	status := nvclique.ComputeDomainStatusNotReady
	if ready {
		status = nvclique.ComputeDomainStatusReady
	}

	clique, err := m.getClique()
	if err != nil {
		return fmt.Errorf("failed to get CDClique: %w", err)
	}
	if clique == nil {
		return fmt.Errorf("CDClique '%s' not found", m.cliqueName())
	}

	// Create a deep copy to modify
	newClique := clique.DeepCopy()

	// Find the daemon
	var myDaemon *nvclique.ComputeDomainDaemonInfo
	for _, d := range newClique.Daemons {
		if d.NodeName == m.config.nodeName {
			myDaemon = d
			break
		}
	}
	if myDaemon == nil {
		return fmt.Errorf("daemon info not yet listed in CDClique (waiting for insertion)")
	}

	// If status hasn't changed, we're done
	if myDaemon.Status == status {
		klog.V(6).Infof("updateDaemonStatus noop: status not changed (%s)", status)
		return nil
	}

	// Update the status
	myDaemon.Status = status

	// Update the clique and (upon success) store the latest version of the object
	// (as returned by the API server) in the mutation cache.
	newClique, err = m.config.clientsets.Nvidia.ResourceV1beta1().ComputeDomainCliques(m.config.podNamespace).Update(ctx, newClique, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error updating CDClique: %w", err)
	}
	m.mutationCache.Mutation(newClique)

	klog.Infof("Successfully updated daemon info status in CDClique (new status: %s)", status)
	return nil
}

// ensureOwnerReference ensures this pod is listed as an owner of the clique.
func (m *ComputeDomainCliqueManager) ensureOwnerReference(clique *nvclique.ComputeDomainClique) {
	for _, ref := range clique.OwnerReferences {
		if string(ref.UID) == m.config.podUID {
			return
		}
	}
	clique.OwnerReferences = append(clique.OwnerReferences, metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "Pod",
		Name:       m.config.podName,
		UID:        types.UID(m.config.podUID),
	})
}

func (m *ComputeDomainCliqueManager) getIPSet(daemons []*nvclique.ComputeDomainDaemonInfo) IPSet {
	set := make(IPSet)
	for _, d := range daemons {
		set[d.IPAddress] = struct{}{}
	}
	return set
}
