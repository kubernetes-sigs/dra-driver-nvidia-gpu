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
	"sort"
	"time"
)

const (
	// Detecting when a CD daemon transitions from NotReady to Ready (based on
	// the startup probe) at the moment sometimes requires an informer resync,
	// see https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/issues/742.
	informerResyncPeriod = 4 * time.Minute
	mutationCacheTTL     = time.Hour

	// Label keys for ComputeDomainClique objects.
	computeDomainLabelKey          = "resource.nvidia.com/computeDomain"
	computeDomainDaemonSetLabelKey = "resource.nvidia.com/computeDomainDaemonSet"
	computeDomainCliqueLabelKey    = "resource.nvidia.com/computeDomain.cliqueID"
)

// IPSet is a set of IP addresses.
type IPSet map[string]struct{}

// Diff compares two IP sets. It returns a list of IPs that were added and a
// list of IPs that were removed. `s` is the reference set.
func (s IPSet) Diff(cmp IPSet) ([]string, []string) {
	var added []string
	var removed []string

	// Find IPs in `s` (reference) that are not in `cmp` (removed).
	for ip := range s {
		if _, exists := cmp[ip]; !exists {
			removed = append(removed, ip)
		}
	}

	// Find IPs in `cmp` that are not in `s` (added).
	for ip := range cmp {
		if _, exists := s[ip]; !exists {
			added = append(added, ip)
		}
	}

	// Sort, for logging purposes.
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}
