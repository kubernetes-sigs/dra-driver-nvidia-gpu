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

	appsv1 "k8s.io/api/apps/v1"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta2"
)

// MultiNamespaceDaemonSetManager manages DaemonSets across multiple namespaces.
type MultiNamespaceDaemonSetManager struct {
	config   *ManagerConfig
	managers map[string]*DaemonSetManager
}

// NewMultiNamespaceDaemonSetManager creates a new multi-namespace DaemonSet manager
// It creates individual DaemonSet managers for the driver namespace and all additional namespaces.
func NewMultiNamespaceDaemonSetManager(config *ManagerConfig, getComputeDomain GetComputeDomainFunc, listComputeDomains ListComputeDomainsFunc, updateComputeDomainStatus UpdateComputeDomainStatusFunc) *MultiNamespaceDaemonSetManager {
	m := &MultiNamespaceDaemonSetManager{
		config:   config,
		managers: make(map[string]*DaemonSetManager),
	}

	// Use a map to deduplicate namespaces (driver namespace + additional namespaces)
	uniqueNamespaces := make(map[string]struct{})
	uniqueNamespaces[config.driverNamespace] = struct{}{}
	for _, ns := range config.additionalNamespaces {
		uniqueNamespaces[ns] = struct{}{}
	}

	// Create managers for unique namespaces
	for ns := range uniqueNamespaces {
		configNew := *config
		configNew.driverNamespace = ns
		configNew.additionalNamespaces = nil
		m.managers[ns] = NewDaemonSetManager(&configNew, getComputeDomain, listComputeDomains, updateComputeDomainStatus)
	}

	return m
}

// Start starts all DaemonSet managers for all namespaces.
func (m *MultiNamespaceDaemonSetManager) Start(ctx context.Context) error {
	for ns, manager := range m.managers {
		if err := manager.Start(ctx); err != nil {
			return fmt.Errorf("failed to start DaemonSet manager for namespace %s: %w", ns, err)
		}
	}
	return nil
}

// Stop stops all DaemonSet managers for all namespaces.
func (m *MultiNamespaceDaemonSetManager) Stop() error {
	for ns, manager := range m.managers {
		if err := manager.Stop(); err != nil {
			return fmt.Errorf("failed to stop DaemonSet manager for namespace %s: %w", ns, err)
		}
	}
	return nil
}

// Create creates a DaemonSet in the provided namespace.
func (m *MultiNamespaceDaemonSetManager) Create(ctx context.Context, cd *nvapi.ComputeDomain) (*appsv1.DaemonSet, error) {
	for ns, manager := range m.managers {
		ds, err := manager.Get(ctx, string(cd.UID))
		if err != nil {
			return nil, fmt.Errorf("failed to get DaemonSet in namespace %s: %w", ns, err)
		}
		if ds != nil {
			return ds, nil
		}
	}
	manager, exists := m.managers[m.config.driverNamespace]
	if !exists {
		return nil, fmt.Errorf("no DaemonSet manager found for namespace %s", m.config.driverNamespace)
	}
	return manager.Create(ctx, cd)
}

// Delete deletes DaemonSets across all namespaces for the given ComputeDomain UID.
func (m *MultiNamespaceDaemonSetManager) Delete(ctx context.Context, cdUID string) error {
	for ns, manager := range m.managers {
		if err := manager.Delete(ctx, cdUID); err != nil {
			return fmt.Errorf("failed to delete DaemonSet in namespace %s: %w", ns, err)
		}
	}
	return nil
}

// RemoveFinalizer removes finalizers from DaemonSets across all namespaces.
func (m *MultiNamespaceDaemonSetManager) RemoveFinalizer(ctx context.Context, cdUID string) error {
	for ns, manager := range m.managers {
		if err := manager.RemoveFinalizer(ctx, cdUID); err != nil {
			return fmt.Errorf("failed to remove finalizer from DaemonSet in namespace %s: %w", ns, err)
		}
	}
	return nil
}

// AssertRemoved asserts that DaemonSets are removed across all namespaces.
func (m *MultiNamespaceDaemonSetManager) AssertRemoved(ctx context.Context, cdUID string) error {
	for ns, manager := range m.managers {
		if err := manager.AssertRemoved(ctx, cdUID); err != nil {
			return fmt.Errorf("failed to assert DaemonSet removal in namespace %s: %w", ns, err)
		}
	}
	return nil
}
