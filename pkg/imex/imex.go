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

// Package imex resolves and validates the imex.mode / imex.scope /
// imex.isolation Helm values shared by the compute-domain-controller and
// compute-domain-kubelet-plugin binaries.
package imex

import "fmt"

// Mode selects who owns the host nvidia-imex daemon lifecycle.
type Mode string

const (
	// ModeDriverManaged is the default: the driver creates and manages the
	// nvidia-imex daemons.
	ModeDriverManaged Mode = "driverManaged"

	// ModeHostManaged assumes the cluster admin owns the host
	// nvidia-imex daemon lifecycle. The driver stops creating per-ComputeDomain
	// IMEX DaemonSets and only advertises/injects an IMEX channel device.
	// Requires the HostManagedIMEXDaemon feature gate.
	ModeHostManaged Mode = "hostManaged"
)

// Scope selects whose IMEX daemons run on a node.
type Scope string

const (
	// ScopeComputeDomain is the default under ModeDriverManaged: each
	// ComputeDomain gets its own IMEX daemons, so a node runs the workloads of
	// one ComputeDomain at a time.
	ScopeComputeDomain Scope = "computeDomain"

	// ScopeCluster shares the IMEX daemons of a node among all
	// ComputeDomains, isolated by their channels. Under ModeDriverManaged, the
	// ComputeDomainDaemonSet runs them. ModeHostManaged implies it.
	ScopeCluster Scope = "cluster"
)

// Isolation selects the IMEX isolation strategy. Meaningful under both
// ModeDriverManaged and ModeHostManaged.
type Isolation string

const (
	// IsolationIMEXDomain is the default isolation strategy, under both
	// ModeDriverManaged and ModeHostManaged: all workloads running in the
	// same IMEX domain share the same channel (0).
	IsolationIMEXDomain Isolation = "domain"

	// IsolationIMEXChannel gives each workload ResourceClaimTemplate its own
	// reserved channel within an IMEX domain. Required by ScopeCluster.
	IsolationIMEXChannel Isolation = "channel"
)

// Config is the resolved imex.mode / imex.scope / imex.isolation
// configuration.
type Config struct {
	Mode      Mode
	Scope     Scope
	Isolation Isolation
}

// Gates holds the feature gates that unlock IMEX configurations.
type Gates struct {
	HostManagedIMEXDaemon            bool
	ComputeDomainChannelReservations bool
	ComputeDomainDaemonSet           bool
}

// EffectiveHostManaged reports whether the resolved configuration puts the
// driver into host-managed IMEX mode.
func (c Config) EffectiveHostManaged() bool {
	return c.Mode == ModeHostManaged
}

// EffectiveScope resolves an unset scope: host-managed daemons are shared by
// all ComputeDomains, driver-managed ones are per ComputeDomain.
func (c Config) EffectiveScope() Scope {
	switch {
	case c.Scope != "":
		return c.Scope
	case c.EffectiveHostManaged():
		return ScopeCluster
	default:
		return ScopeComputeDomain
	}
}

// StaticDaemon reports whether the driver runs one static IMEX daemon per
// node, managed through the ComputeDomainDaemonSet, instead of one per
// ComputeDomain.
func (c Config) StaticDaemon() bool {
	return c.Mode == ModeDriverManaged && c.EffectiveScope() == ScopeCluster
}

// SharedIMEXDomain reports whether all ComputeDomains share an IMEX domain,
// isolated by their channels, with no per-ComputeDomain IMEX daemon.
func (c Config) SharedIMEXDomain() bool {
	return c.EffectiveScope() == ScopeCluster
}

// String names the configuration for logs and errors.
func (c Config) String() string {
	return fmt.Sprintf("mode=%s scope=%s isolation=%s", c.Mode, c.EffectiveScope(), c.Isolation)
}

// Validate checks the Mode/Scope/Isolation combination against the feature
// gates that unlock it.
//
// The supported combinations are:
//
//	driverManaged, computeDomain (default), domain (default)  -> OK
//	driverManaged, computeDomain, channel                     -> ComputeDomainChannelReservations
//	driverManaged, cluster, channel                           -> ComputeDomainDaemonSet (static daemons)
//	driverManaged, cluster, domain                            -> error (shared daemons need channels)
//	hostManaged, cluster (implied), domain                    -> HostManagedIMEXDaemon
//	hostManaged, cluster (implied), channel                   -> HostManagedIMEXDaemon, ComputeDomainChannelReservations
//	hostManaged, computeDomain                                -> error (the host daemon is shared)
func (c Config) Validate(gates Gates) error {
	switch c.Mode {
	case ModeDriverManaged:
		// No feature gate requirement.
	case ModeHostManaged:
		if !gates.HostManagedIMEXDaemon {
			return fmt.Errorf("imex.mode=%q requires the HostManagedIMEXDaemon feature gate to be enabled", ModeHostManaged)
		}
	default:
		return fmt.Errorf("unknown imex.mode %q", c.Mode)
	}

	switch c.Isolation {
	case "", IsolationIMEXDomain, IsolationIMEXChannel:
	default:
		return fmt.Errorf("unknown imex.isolation %q: must be %q or %q", c.Isolation, IsolationIMEXDomain, IsolationIMEXChannel)
	}
	channel := c.Isolation == IsolationIMEXChannel
	if channel && !gates.ComputeDomainChannelReservations {
		return fmt.Errorf("imex.isolation=%q requires the ComputeDomainChannelReservations feature gate to be enabled", IsolationIMEXChannel)
	}

	switch c.EffectiveScope() {
	case ScopeComputeDomain:
		if c.EffectiveHostManaged() {
			return fmt.Errorf("imex.scope=%q is not supported with imex.mode=%q: the host IMEX daemon is shared by all ComputeDomains", ScopeComputeDomain, ModeHostManaged)
		}
	case ScopeCluster:
		if c.EffectiveHostManaged() {
			return nil
		}
		if !channel {
			return fmt.Errorf("imex.scope=%q requires imex.isolation=%q, as ComputeDomains sharing IMEX daemons are isolated by their channels", ScopeCluster, IsolationIMEXChannel)
		}
		if !gates.ComputeDomainDaemonSet {
			return fmt.Errorf("imex.scope=%q requires the ComputeDomainDaemonSet feature gate to be enabled", ScopeCluster)
		}
	default:
		return fmt.Errorf("unknown imex.scope %q: must be %q or %q", c.Scope, ScopeComputeDomain, ScopeCluster)
	}
	return nil
}
