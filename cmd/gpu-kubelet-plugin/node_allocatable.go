/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"strings"

	"github.com/urfave/cli/v2"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// Device classes that can carry node-allocatable overhead. The mig class
// covers both statically and dynamically partitioned MIG devices.
const (
	overheadClassGpu  = "gpu"
	overheadClassMig  = "mig"
	overheadClassVfio = "vfio"
)

// nodeAllocatableOverheadValues holds the raw overhead flag values for one
// device class.
type nodeAllocatableOverheadValues struct {
	memoryPerPod       string
	memoryPerContainer string
	cpuPerPod          string
	cpuPerContainer    string
}

type overheadFlagField struct {
	suffix      string
	example     string
	resource    corev1.ResourceName
	perPod      bool
	destination func(*nodeAllocatableOverheadValues) *string
}

// overheadFlagFields is the single source of truth for the per-class overhead
// flag surface: flag registration, env var names, and parsing all derive from
// it.
var overheadFlagFields = []overheadFlagField{
	{"memory-overhead-per-pod", "100Mi", corev1.ResourceMemory, true, func(v *nodeAllocatableOverheadValues) *string { return &v.memoryPerPod }},
	{"memory-overhead-per-container", "10Mi", corev1.ResourceMemory, false, func(v *nodeAllocatableOverheadValues) *string { return &v.memoryPerContainer }},
	{"cpu-overhead-per-pod", "100m", corev1.ResourceCPU, true, func(v *nodeAllocatableOverheadValues) *string { return &v.cpuPerPod }},
	{"cpu-overhead-per-container", "10m", corev1.ResourceCPU, false, func(v *nodeAllocatableOverheadValues) *string { return &v.cpuPerContainer }},
}

func overheadFlagName(class string, field overheadFlagField) string {
	return fmt.Sprintf("node-allocatable-%s-%s", class, field.suffix)
}

func overheadClassValues(flags *Flags) map[string]*nodeAllocatableOverheadValues {
	return map[string]*nodeAllocatableOverheadValues{
		overheadClassGpu:  &flags.gpuNodeAllocatableOverhead,
		overheadClassMig:  &flags.migNodeAllocatableOverhead,
		overheadClassVfio: &flags.vfioNodeAllocatableOverhead,
	}
}

func nodeAllocatableOverheadCLIFlags(flags *Flags) []cli.Flag {
	var cliFlags []cli.Flag
	for _, class := range []string{overheadClassGpu, overheadClassMig, overheadClassVfio} {
		values := overheadClassValues(flags)[class]
		for _, field := range overheadFlagFields {
			name := overheadFlagName(class, field)
			cliFlags = append(cliFlags, &cli.StringFlag{
				Name: name,
				Usage: fmt.Sprintf(
					"Node-allocatable overhead incurred by pods referencing a %s device (resource quantity, e.g. '%s'). Requires the NodeAllocatableResources feature gate.",
					class, field.example),
				Destination: field.destination(values),
				EnvVars:     []string{strings.ToUpper(strings.ReplaceAll(name, "-", "_"))},
			})
		}
	}
	return cliFlags
}

// parseNodeAllocatableOverheadFlags validates and parses the per-class
// node-allocatable overhead flags once at startup. It returns the overheads to
// publish keyed by device class, or nil when nothing is configured.
// Unparseable or negative values are rejected, as is any value set while the
// NodeAllocatableResources feature gate is disabled (matching the
// --consumable-shares contract). Zero values are treated as unset.
func parseNodeAllocatableOverheadFlags(flags *Flags) (map[string]map[corev1.ResourceName]resourceapi.NodeAllocatableResource, error) {
	overheads := map[string]map[corev1.ResourceName]resourceapi.NodeAllocatableResource{}
	anySet := false

	for class, values := range overheadClassValues(flags) {
		classOverheads := map[corev1.ResourceName]resourceapi.NodeAllocatableResource{}
		for _, field := range overheadFlagFields {
			value := strings.TrimSpace(*field.destination(values))
			if value == "" {
				continue
			}
			anySet = true
			quantity, err := resource.ParseQuantity(value)
			if err != nil {
				return nil, fmt.Errorf("invalid value for --%s: %q: %w", overheadFlagName(class, field), value, err)
			}
			if quantity.Sign() < 0 {
				return nil, fmt.Errorf("invalid value for --%s: %q (must not be negative)", overheadFlagName(class, field), value)
			}
			if quantity.Sign() == 0 {
				continue
			}

			entry := classOverheads[field.resource]
			if entry.Overhead == nil {
				entry.Overhead = &resourceapi.NodeAllocatableOverhead{}
			}
			if field.perPod {
				entry.Overhead.PerPod = &quantity
			} else {
				entry.Overhead.PerContainer = &quantity
			}
			classOverheads[field.resource] = entry
		}
		if len(classOverheads) > 0 {
			overheads[class] = classOverheads
		}
	}

	if !anySet {
		return nil, nil
	}
	if !featuregates.Enabled(featuregates.NodeAllocatableResources) {
		return nil, fmt.Errorf("node-allocatable overhead flags require feature gate %s to be enabled", featuregates.NodeAllocatableResources)
	}
	if len(overheads) == 0 {
		return nil, nil
	}
	return overheads, nil
}

// applyNodeAllocatableOverheads attaches the startup-parsed
// NodeAllocatableResources overhead entries for the given device class to a
// published device. Only the Overhead branch is ever set: none of these
// devices are node resources themselves, so there is nothing to express via
// Mapping.
func applyNodeAllocatableOverheads(dev *resourceapi.Device, config *Config, class string) {
	if config == nil {
		return
	}
	classOverheads := config.nodeAllocatableOverheads[class]
	if len(classOverheads) == 0 {
		return
	}
	dev.NodeAllocatableResources = make(map[corev1.ResourceName]resourceapi.NodeAllocatableResource, len(classOverheads))
	for name, overhead := range classOverheads {
		dev.NodeAllocatableResources[name] = *overhead.DeepCopy()
	}
}
