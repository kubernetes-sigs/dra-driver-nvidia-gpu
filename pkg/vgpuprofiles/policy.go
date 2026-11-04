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

// Package vgpuprofiles implements the administrator-facing policy that selects
// which NVIDIA vGPU profiles the GPU kubelet plugin advertises as vGPU
// partition devices.
//
// The policy is node-local configuration (a file, typically projected from a
// ConfigMap), not a Kubernetes API object. It nevertheless carries an
// apiVersion/kind envelope and is strict-decoded so that the on-disk format is
// a versioned contract: new knobs can be added as optional fields, and the
// policy can be promoted to a CRD later without renaming anything an
// administrator already wrote.
package vgpuprofiles

import (
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

const (
	// APIVersion is the only apiVersion accepted in a policy document. It
	// intentionally matches the driver's API group so that a future
	// promotion of this configuration to a CRD is not a breaking change for
	// documents already written by administrators.
	APIVersion = "resource.nvidia.com/v1beta1"

	// Kind is the only kind accepted in a policy document.
	Kind = "VgpuProfilePolicy"
)

// Policy is the root of a policy document.
type Policy struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`

	// Rules selects the profiles to advertise. The selection applies to
	// every GPU on the node; profiles the host cannot create are skipped
	// at discovery time. An empty value advertises no vGPU partitions.
	Rules Rules `json:"rules,omitempty"`
}

// Rules advertises a set of vGPU profiles.
type Rules struct {
	// Profiles are the vGPU profiles to advertise.
	Profiles []Profile `json:"profiles"`
}

// Profile selects a single vGPU profile (type).
//
// This is an object rather than a bare profile name so that per-profile knobs
// (instance caps, MIG placement policy, enable/disable) can be added later as
// optional fields instead of being encoded into a name string.
type Profile struct {
	// Name is the full vGPU type name as reported by NVML, e.g.
	// "NVIDIA L40S-12Q". It is the profile's identity: profile IDs vary
	// across vGPU Manager versions, names do not.
	Name string `json:"name"`
}

// Parse decodes and validates a policy document.
//
// Decoding is strict: an unknown field is an error rather than a silently
// ignored typo, which is what makes the format safe to extend.
func Parse(data []byte) (*Policy, error) {
	var c Policy
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return nil, fmt.Errorf("error decoding vGPU profile policy: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// ParseFile reads and parses the policy document at path.
func ParseFile(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("error reading vGPU profile policy: %w", err)
	}
	return Parse(data)
}

// Validate reports whether the policy is well-formed.
func (p *Policy) Validate() error {
	if p.APIVersion != APIVersion {
		return fmt.Errorf("unsupported apiVersion %q in vGPU profile policy, expected %q", p.APIVersion, APIVersion)
	}
	if p.Kind != Kind {
		return fmt.Errorf("unsupported kind %q in vGPU profile policy, expected %q", p.Kind, Kind)
	}

	// Duplicates are rejected rather than silently collapsed: a repeated
	// name usually means two entries were meant to differ in a field that
	// does not exist yet.
	seen := make(map[string]bool, len(p.Rules.Profiles))
	for j, profile := range p.Rules.Profiles {
		if strings.TrimSpace(profile.Name) == "" {
			return fmt.Errorf("rules.profiles[%d]: name is required", j)
		}
		if seen[profile.Name] {
			return fmt.Errorf("rules.profiles[%d]: duplicate profile %q", j, profile.Name)
		}
		seen[profile.Name] = true
	}
	return nil
}

// Empty reports whether the policy advertises nothing at all.
func (p *Policy) Empty() bool {
	return len(p.Rules.Profiles) == 0
}

// SelectedProfiles returns the profiles the policy advertises, keyed by
// profile name. It returns nil when the policy selects nothing.
func (p *Policy) SelectedProfiles() map[string]Profile {
	var selected map[string]Profile
	for _, profile := range p.Rules.Profiles {
		if selected == nil {
			selected = make(map[string]Profile)
		}
		selected[profile.Name] = profile
	}
	return selected
}
