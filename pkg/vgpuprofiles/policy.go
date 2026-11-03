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

	// Rules selects the profiles to advertise. A GPU is matched against
	// every rule, and the profiles of all matching rules are unioned, so a
	// policy can carry one model-specific rule per SKU plus a shared
	// catch-all rule. An empty list advertises no vGPU partitions.
	Rules []Rule `json:"rules,omitempty"`
}

// Rule advertises a set of vGPU profiles on the GPUs it selects.
type Rule struct {
	// GpuModel matches a GPU's product name as reported by NVML, e.g.
	// "NVIDIA L40S". An empty value matches every GPU on the node.
	//
	// This is the policy's only selector today. It exists from the first
	// version because adding a scoping dimension later would change the
	// meaning of every previously written policy, whereas widening the set
	// of supported matchers is additive.
	GpuModel string `json:"gpuModel,omitempty"`

	// Profiles are the vGPU profiles to advertise on matching GPUs.
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

	for i, rule := range p.Rules {
		if len(rule.Profiles) == 0 {
			return fmt.Errorf("rules[%d]: at least one profile is required", i)
		}
		// Duplicates are rejected per rule rather than silently collapsed:
		// a repeated name usually means two entries were meant to differ
		// in a field that does not exist yet.
		seen := make(map[string]bool, len(rule.Profiles))
		for j, profile := range rule.Profiles {
			if strings.TrimSpace(profile.Name) == "" {
				return fmt.Errorf("rules[%d].profiles[%d]: name is required", i, j)
			}
			if seen[profile.Name] {
				return fmt.Errorf("rules[%d].profiles[%d]: duplicate profile %q", i, j, profile.Name)
			}
			seen[profile.Name] = true
		}
	}
	return nil
}

// Empty reports whether the policy advertises nothing at all.
func (p *Policy) Empty() bool {
	for _, rule := range p.Rules {
		if len(rule.Profiles) > 0 {
			return false
		}
	}
	return true
}

// ProfilesForGPU returns the profiles the policy advertises on a GPU with the
// given NVML product name, keyed by profile name. It returns nil when the GPU
// matches no rule.
func (p *Policy) ProfilesForGPU(productName string) map[string]Profile {
	var selected map[string]Profile
	for _, rule := range p.Rules {
		if rule.GpuModel != "" && rule.GpuModel != productName {
			continue
		}
		for _, profile := range rule.Profiles {
			if selected == nil {
				selected = make(map[string]Profile)
			}
			// A model-specific rule and a catch-all rule may name the
			// same profile; first match wins so that ordering gives
			// administrators a predictable override.
			if _, ok := selected[profile.Name]; !ok {
				selected[profile.Name] = profile
			}
		}
	}
	return selected
}
