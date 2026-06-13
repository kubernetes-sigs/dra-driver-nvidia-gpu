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
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	nvinformers "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/informers/externalversions"
	nvlisters "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/listers/resource/v1beta1"
)

// ComputeDomainCliqueManager manages ComputeDomainClique objects, providing
// Get/List/Update methods for clique access.
type ComputeDomainCliqueManager struct {
	config        *ManagerConfig
	waitGroup     sync.WaitGroup
	cancelContext context.CancelFunc

	factory       nvinformers.SharedInformerFactory
	informer      cache.SharedIndexInformer
	lister        nvlisters.ComputeDomainCliqueLister
	mutationCache cache.MutationCache
}

// NewComputeDomainCliqueManager creates a new ComputeDomainCliqueManager.
func NewComputeDomainCliqueManager(config *ManagerConfig) *ComputeDomainCliqueManager {
	factory := nvinformers.NewSharedInformerFactoryWithOptions(
		config.clientsets.Nvidia,
		informerResyncPeriod,
		nvinformers.WithNamespace(config.driverNamespace),
	)
	informer := factory.Resource().V1beta1().ComputeDomainCliques().Informer()
	lister := nvlisters.NewComputeDomainCliqueLister(informer.GetIndexer())

	m := &ComputeDomainCliqueManager{
		config:   config,
		factory:  factory,
		informer: informer,
		lister:   lister,
	}

	return m
}

// Start starts the ComputeDomainCliqueManager.
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

	// Create mutation cache to track updates
	m.mutationCache = cache.NewIntegerResourceVersionMutationCache(
		klog.Background(),
		m.informer.GetStore(),
		m.informer.GetIndexer(),
		mutationCacheTTL,
		true,
	)

	m.waitGroup.Add(1)
	go func() {
		defer m.waitGroup.Done()
		m.factory.Start(ctx.Done())
	}()

	if !cache.WaitForCacheSync(ctx.Done(), m.informer.HasSynced) {
		return fmt.Errorf("informer cache sync for ComputeDomainCliques failed")
	}

	return nil
}

// Stop stops the ComputeDomainCliqueManager.
func (m *ComputeDomainCliqueManager) Stop() error {
	if m.cancelContext != nil {
		m.cancelContext()
	}
	m.waitGroup.Wait()
	return nil
}

// Get returns the ComputeDomainClique for the given ComputeDomain UID and clique ID.
// The clique name is "<computeDomainUID>.<cliqueID>".
func (m *ComputeDomainCliqueManager) Get(cdUID, cliqueID string) *nvapi.ComputeDomainClique {
	key := fmt.Sprintf("%s/%s.%s", m.config.driverNamespace, cdUID, cliqueID)
	obj, exists, err := m.mutationCache.GetByKey(key)
	if err != nil || !exists {
		return nil
	}
	clique, ok := obj.(*nvapi.ComputeDomainClique)
	if !ok {
		return nil
	}
	return clique
}

// List returns all ComputeDomainCliques from the informer cache.
func (m *ComputeDomainCliqueManager) List() ([]*nvapi.ComputeDomainClique, error) {
	return m.lister.ComputeDomainCliques(m.config.driverNamespace).List(labels.Everything())
}

// GetLive returns the ComputeDomainClique from the API server (not the informer cache).
// Callers that perform read-modify-write updates should use this before Update to reduce
// stale resourceVersion conflicts.
func (m *ComputeDomainCliqueManager) GetLive(ctx context.Context, name string) (*nvapi.ComputeDomainClique, error) {
	return m.config.clientsets.Nvidia.ResourceV1beta1().ComputeDomainCliques(m.config.driverNamespace).Get(ctx, name, metav1.GetOptions{})
}

// Update updates a ComputeDomainClique and caches the result in the mutation cache.
func (m *ComputeDomainCliqueManager) Update(ctx context.Context, clique *nvapi.ComputeDomainClique) (*nvapi.ComputeDomainClique, error) {
	updatedClique, err := m.config.clientsets.Nvidia.ResourceV1beta1().ComputeDomainCliques(clique.Namespace).Update(ctx, clique, metav1.UpdateOptions{})
	if err != nil {
		return nil, err
	}
	m.mutationCache.Mutation(updatedClique)
	return updatedClique, nil
}
