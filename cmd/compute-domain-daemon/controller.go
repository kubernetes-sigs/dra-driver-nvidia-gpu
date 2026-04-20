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

	nvcd "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/workqueue"
)

// DaemonInfoManager is an interface for managing daemon info updates.
// It is implemented by ComputeDomainStatusManager and ComputeDomainCliqueManager.
type DaemonInfoManager interface {
	Start(ctx context.Context) error
	Stop() error
	GetDaemonInfoUpdateChan() chan []*nvcd.ComputeDomainDaemonInfo
}

// ManagerConfig holds the configuration for the compute domain manager.
type ManagerConfig struct {
	httpEndpoint           string
	metricsPath            string
	workQueue              *workqueue.WorkQueue
	clientsets             flags.ClientSets
	nodeName               string
	computeDomainUUID      string
	computeDomainName      string
	computeDomainNamespace string
	cliqueID               string
	podIP                  string
	podUID                 string
	podName                string
	podNamespace           string
	maxNodesPerIMEXDomain  int
}

// ControllerConfig holds the configuration for the controller.
type ControllerConfig struct {
	httpEndpoint           string
	metricsPath            string
	clientsets             flags.ClientSets
	nodeName               string
	computeDomainUUID      string
	computeDomainName      string
	computeDomainNamespace string
	cliqueID               string
	podIP                  string
	podUID                 string
	podName                string
	podNamespace           string
	maxNodesPerIMEXDomain  int
}

// Controller manages the lifecycle of compute domain operations.
type Controller struct {
	daemonInfoManager DaemonInfoManager
	workQueue         *workqueue.WorkQueue
}

// NewController creates and initializes a new Controller instance.
func NewController(config *ControllerConfig) (*Controller, error) {
	workQueue := workqueue.New(workqueue.DefaultCDDaemonRateLimiter())

	mc := &ManagerConfig{
		workQueue:              workQueue,
		httpEndpoint:           config.httpEndpoint,
		metricsPath:            config.metricsPath,
		clientsets:             config.clientsets,
		nodeName:               config.nodeName,
		computeDomainUUID:      config.computeDomainUUID,
		computeDomainName:      config.computeDomainName,
		computeDomainNamespace: config.computeDomainNamespace,
		cliqueID:               config.cliqueID,
		podIP:                  config.podIP,
		podUID:                 config.podUID,
		podName:                config.podName,
		podNamespace:           config.podNamespace,
		maxNodesPerIMEXDomain:  config.maxNodesPerIMEXDomain,
	}

	// Choose the appropriate daemon info manager based on the feature gate
	var daemonInfoManager DaemonInfoManager
	if featuregates.Enabled(featuregates.ComputeDomainCliques) {
		daemonInfoManager = NewComputeDomainCliqueManager(mc)
	} else {
		daemonInfoManager = NewComputeDomainStatusManager(mc)
	}

	controller := &Controller{
		daemonInfoManager: daemonInfoManager,
		workQueue:         workQueue,
	}

	return controller, nil
}

// Run starts the controller's main loop and manages the lifecycle of its components.
// It initializes the work queue and handles graceful shutdown when the context is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	// Start the daemon info manager
	if err := c.daemonInfoManager.Start(ctx); err != nil {
		return fmt.Errorf("failed to start daemon info manager: %v", err)
	}

	// Start processing the workqueue
	c.workQueue.Run(ctx)

	// Stop the daemon info manager
	if err := c.daemonInfoManager.Stop(); err != nil {
		return fmt.Errorf("failed to stop daemon info manager: %v", err)
	}

	return nil
}

// GetDaemonInfoUpdateChan returns a channel that yields updates for the daemons
// currently present in the CD status or CDClique. This is only a complete set of
// daemons (size `numNodes`) if IMEXDaemonsWithDNSNames=false.
func (c *Controller) GetDaemonInfoUpdateChan() chan []*nvcd.ComputeDomainDaemonInfo {
	return c.daemonInfoManager.GetDaemonInfoUpdateChan()
}
