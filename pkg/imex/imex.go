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

// Package imex resolves and validates the imex.daemonLifecycle Helm value
// shared by the compute-domain-controller and compute-domain-kubelet-plugin
// binaries.
package imex

import (
	"fmt"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// DaemonLifecycle selects who runs the nvidia-imex daemons, and for how long.
type DaemonLifecycle string

const (
	// DaemonLifecyclePerComputeDomain is the default: the driver starts IMEX
	// daemons for each ComputeDomain on its nodes, and removes them with it.
	// A node runs the workloads of one ComputeDomain at a time.
	DaemonLifecyclePerComputeDomain DaemonLifecycle = "perComputeDomain"

	// DaemonLifecycleHostManaged assumes the cluster admin runs nvidia-imex
	// as a host service, shared by all ComputeDomains. The driver only
	// injects IMEX channels. Requires the HostManagedIMEXDaemon feature gate.
	DaemonLifecycleHostManaged DaemonLifecycle = "hostManaged"
)

// Deprecated imex.mode values, which v0.5 releases accept.
const (
	deprecatedModeDriverManaged = "driverManaged"
	deprecatedModeHostManaged   = "hostManaged"
)

// Config is the resolved imex.daemonLifecycle configuration.
type Config struct {
	DaemonLifecycle DaemonLifecycle
}

// ResolveConfig resolves imex.daemonLifecycle, falling back to the deprecated
// imex.mode so that values of earlier releases keep working. The deprecated
// imex.isolation is ignored, as the ComputeDomainChannelReservations feature
// gate and the ComputeDomain spec decide the channels of workloads. It returns
// a warning for each deprecated value that is set.
func ResolveConfig(lifecycle, mode, isolation string) (Config, []string, error) {
	var warnings []string
	if mode != "" {
		warnings = append(warnings, fmt.Sprintf("imex.mode is deprecated, use imex.daemonLifecycle instead (imex.mode=%q)", mode))
	}
	if isolation != "" {
		warnings = append(warnings, fmt.Sprintf("imex.isolation is deprecated and ignored, IMEX channels follow the ComputeDomainChannelReservations feature gate (imex.isolation=%q)", isolation))
	}
	switch mode {
	case "", deprecatedModeDriverManaged, deprecatedModeHostManaged:
	default:
		return Config{}, warnings, fmt.Errorf("unknown imex.mode %q: must be %q or %q", mode, deprecatedModeDriverManaged, deprecatedModeHostManaged)
	}

	config := Config{DaemonLifecycle: DaemonLifecycle(lifecycle)}
	switch {
	case lifecycle == "" && mode == deprecatedModeHostManaged:
		config.DaemonLifecycle = DaemonLifecycleHostManaged
	case lifecycle == "":
		config.DaemonLifecycle = DaemonLifecyclePerComputeDomain
	case mode == deprecatedModeHostManaged && config.DaemonLifecycle != DaemonLifecycleHostManaged:
		return Config{}, warnings, fmt.Errorf("imex.mode=%q contradicts imex.daemonLifecycle=%q, unset imex.mode", mode, lifecycle)
	}
	return config, warnings, nil
}

// EffectiveHostManaged reports whether the admin runs the IMEX daemons as a
// host service.
func (c Config) EffectiveHostManaged() bool {
	return c.DaemonLifecycle == DaemonLifecycleHostManaged
}

// SharedIMEXDaemons reports whether all ComputeDomains share the IMEX daemons
// of a node, with no per-ComputeDomain IMEX daemon. Workloads are then isolated
// by IMEX channels within the IMEX domain, not by IMEX domains.
func (c Config) SharedIMEXDaemons() bool {
	return c.DaemonLifecycle == DaemonLifecycleHostManaged
}

// String names the configuration for logs and errors.
func (c Config) String() string {
	return fmt.Sprintf("daemonLifecycle=%s", c.DaemonLifecycle)
}

// Validate checks the daemon lifecycle against the feature gates that unlock
// it.
func (c Config) Validate() error {
	switch c.DaemonLifecycle {
	case DaemonLifecyclePerComputeDomain:
	case DaemonLifecycleHostManaged:
		if !featuregates.Enabled(featuregates.HostManagedIMEXDaemon) {
			return fmt.Errorf("imex.daemonLifecycle=%q requires the HostManagedIMEXDaemon feature gate to be enabled", DaemonLifecycleHostManaged)
		}
	default:
		return fmt.Errorf("unknown imex.daemonLifecycle %q: must be %q or %q", c.DaemonLifecycle, DaemonLifecyclePerComputeDomain, DaemonLifecycleHostManaged)
	}
	return nil
}
