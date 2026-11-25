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

package metrics

import (
	"fmt"
	"sync"
	"testing"

	"k8s.io/component-base/metrics/legacyregistry"
)

func resetComputeDomainClusterMetricsForTest() {
	legacyregistry.Reset()
	computeDomainClusterMetricsOnce = sync.Once{}
	computeDomainLastStatus = nil
}

// TestObserveComputeDomainStatusConcurrent syncs multiple ComputeDomains in parallel
// goroutines, each of which calls ObserveComputeDomainStatus for its own UID.
func TestObserveComputeDomainStatusConcurrent(t *testing.T) {
	resetComputeDomainClusterMetricsForTest()

	const numCDs = 50
	const numUpdatesPerCD = 20
	statuses := []string{"NotReady", "Ready", "Error"}

	var wg sync.WaitGroup
	for i := 0; i < numCDs; i++ {
		uid := fmt.Sprintf("cd-uid-%d", i)
		wg.Add(1)
		go func(uid string) {
			defer wg.Done()
			for j := 0; j < numUpdatesPerCD; j++ {
				ObserveComputeDomainStatus(uid, statuses[j%len(statuses)])
			}
		}(uid)
	}
	wg.Wait()

	// Concurrently forget some of the same UIDs while others are still being observed,
	// exercising ForgetComputeDomain and ObserveComputeDomainStatus against the shared
	// map at the same time.
	var wg2 sync.WaitGroup
	for i := 0; i < numCDs; i++ {
		uid := fmt.Sprintf("cd-uid-%d", i)
		wg2.Add(2)
		go func(uid string) {
			defer wg2.Done()
			ObserveComputeDomainStatus(uid, "Ready")
		}(uid)
		go func(uid string) {
			defer wg2.Done()
			ForgetComputeDomain(uid)
		}(uid)
	}
	wg2.Wait()
}
