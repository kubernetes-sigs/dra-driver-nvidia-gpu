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

// Package imex resolves and validates the imex.mode / imex.isolation Helm
// values shared by the compute-domain-controller and
// compute-domain-kubelet-plugin binaries.
package imex

import "fmt"

// Mode selects who owns the host nvidia-imex daemon lifecycle.
type Mode string

const (
	// ModeDriverManaged is the default: the driver creates and manages a
	// per-ComputeDomain nvidia-imex DaemonSet.
	ModeDriverManaged Mode = "driverManaged"

	// ModeHostManaged assumes the cluster admin owns the host
	// nvidia-imex daemon lifecycle. The driver stops creating per-ComputeDomain
	// IMEX DaemonSets and only advertises/injects an IMEX channel device.
	// Requires the HostManagedIMEXDaemon feature gate.
	ModeHostManaged Mode = "hostManaged"
)

// Isolation selects the IMEX isolation strategy. Meaningful under both
// ModeDriverManaged and ModeHostManaged.
type Isolation string

const (
	// IsolationIMEXDomain is the default isolation strategy, under both
	// ModeDriverManaged and ModeHostManaged: workers of a ComputeDomain share
	// one channel. Driver-managed mode uses channel zero in its own domain;
	// host-managed mode reserves a distinct channel in the shared host domain.
	IsolationIMEXDomain Isolation = "domain"

	// IsolationIMEXChannel would give each workload a unique channel within
	// an IMEX domain. Not implemented/supported yet: currently rejected as
	// an error.
	IsolationIMEXChannel Isolation = "channel"
)

// Config is the resolved imex.mode / imex.isolation configuration.
type Config struct {
	Mode      Mode
	Isolation Isolation
}

// EffectiveHostManaged reports whether the resolved configuration puts the
// driver into host-managed IMEX mode.
func (c Config) EffectiveHostManaged() bool {
	return c.Mode == ModeHostManaged
}

// Validate checks the Mode/Isolation combination against the
// HostManagedIMEXDaemon feature gate state. hostManagedGateEnabled must
// reflect whether the HostManagedIMEXDaemon feature gate is currently
// enabled.
//
// The supported combinations are:
//
//	ModeDriverManaged, IsolationIMEXDomain (default)  -> OK
//	ModeDriverManaged, IsolationIMEXChannel           -> error (not implemented yet)
//	ModeHostManaged, !hostManagedGateEnabled          -> error (gate required)
//	ModeHostManaged, IsolationIMEXDomain (default)    -> OK
//	ModeHostManaged, IsolationIMEXChannel             -> error (not implemented yet)
func (c Config) Validate(hostManagedGateEnabled bool) error {
	switch c.Mode {
	case ModeDriverManaged:
		// No feature gate requirement.
	case ModeHostManaged:
		if !hostManagedGateEnabled {
			return fmt.Errorf("imex.mode=%q requires the HostManagedIMEXDaemon feature gate to be enabled", ModeHostManaged)
		}
	default:
		return fmt.Errorf("unknown imex.mode %q", c.Mode)
	}

	switch c.Isolation {
	case "", IsolationIMEXDomain:
		return nil
	case IsolationIMEXChannel:
		return fmt.Errorf("imex.isolation=%q is not supported yet: per-workload IMEX channel allocation is not implemented", IsolationIMEXChannel)
	default:
		return fmt.Errorf("unknown imex.isolation %q: must be %q or %q", c.Isolation, IsolationIMEXDomain, IsolationIMEXChannel)
	}
}
