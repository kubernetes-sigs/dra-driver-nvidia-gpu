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

// Package main implements a Kubernetes Device Resource Allocation (DRA) driver controller
package main

import (
	"context"
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/workqueue"
)

// ManagerConfig defines the common configuration options shared across all managers.
// It contains essential fields for driver identification, Kubernetes client access,
// and work queue management.
type ManagerConfig struct {
	// driverName is the unique identifier for this DRA driver
	driverName string

	// driverNamespace is the Kubernetes namespace where the driver operates
	driverNamespace string

	// imageName is the full image name to use when rendering templates
	imageName string

	// maxNodesPerIMEXDomain is the maximum number of nodes per IMEX domain to allocate
	maxNodesPerIMEXDomain int

	// clientsets provides access to various Kubernetes API client interfaces
	clientsets flags.ClientSets

	// workQueue manages the asynchronous processing of tasks
	workQueue *workqueue.WorkQueue

	// additionalNamespaces is a list of additional namespaces
	// where the driver can manage resources
	additionalNamespaces []string

	// logVerbosityCDDaemon controls the log verbosity for dynamically launched
	// ComputeDomain daemons.
	logVerbosityCDDaemon int

	// httpEndpoint is the TCP network address where the HTTP server for diagnostics
	// (including pprof and metrics) will listen
	httpEndpoint string

	// metricsPath is the HTTP path for Prometheus metrics
	metricsPath string

	// imagePullSecretNames are the names of the image pull secrets to apply to dynamically rendered compute-domain-daemon
	imagePullSecretNames []string
}

// Controller manages the lifecycle of the DRA driver and its components.
type Controller struct {
	// config holds the controller's configuration settings
	config *Config
}

// NewController creates and initializes a new Controller instance with the provided configuration.
func NewController(config *Config) *Controller {
	return &Controller{config: config}
}

// Run starts the controller's main loop and manages the lifecycle of its components.
// It initializes the work queue, starts the ComputeDomain manager, and handles
// graceful shutdown when the context is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	workQueue := workqueue.New(workqueue.DefaultControllerRateLimiter())

	managerConfig := &ManagerConfig{
		driverName:            c.config.driverName,
		driverNamespace:       c.config.flags.namespace,
		additionalNamespaces:  c.config.flags.additionalNamespaces.Value(),
		imageName:             c.config.flags.imageName,
		maxNodesPerIMEXDomain: c.config.flags.maxNodesPerIMEXDomain,
		clientsets:            c.config.clientsets,
		workQueue:             workQueue,
		logVerbosityCDDaemon:  c.config.flags.logVerbosityCDDaemon,
		httpEndpoint:          c.config.flags.httpEndpoint,
		metricsPath:           c.config.flags.metricsPath,
		imagePullSecretNames:  c.config.imagePullSecretNames,
	}

	// TODO: log full, nested cliFlags structure.
	klog.Infof("controller manager config: %+v", managerConfig)

	if featuregates.Enabled(featuregates.SharedDaemonResourceClaim) {
		if err := ensureDriverNamespaceAdminAccessLabel(ctx, c.config.clientsets, managerConfig.driverNamespace); err != nil {
			return fmt.Errorf("error ensuring driver namespace admin-access label: %w", err)
		}
	}

	cdManager := NewComputeDomainManager(managerConfig)

	if err := cdManager.Start(ctx); err != nil {
		return fmt.Errorf("error starting ComputeDomain manager: %w", err)
	}

	workQueue.Run(ctx)

	if err := cdManager.Stop(); err != nil {
		return fmt.Errorf("error stopping ComputeDomain manager: %w", err)
	}

	return nil
}

// ensureDriverNamespaceAdminAccessLabel patches the driver namespace with
// resource.kubernetes.io/admin-access=true so that the apiserver's
// DRAAdminAccess gate permits per-CD daemon ResourceClaims (in this
// namespace) to set adminAccess: true on their request. Idempotent: a
// strategic-merge patch is a no-op when the label is already present.
func ensureDriverNamespaceAdminAccessLabel(ctx context.Context, c flags.ClientSets, namespace string) error {
	patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:"true"}}}`, resourceapi.DRAAdminNamespaceLabelKey))
	if _, err := c.Core.CoreV1().Namespaces().Patch(ctx, namespace, types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patching namespace %q with %s=true: %w", namespace, resourceapi.DRAAdminNamespaceLabelKey, err)
	}
	return nil
}
