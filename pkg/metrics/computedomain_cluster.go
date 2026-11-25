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
	"sync"

	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

var (
	computeDomainClusterMetricsOnce sync.Once
	computeDomains                  *metrics.GaugeVec

	computeDomainLastStatusMu sync.Mutex
	computeDomainLastStatus   map[string]string // ComputeDomain UID -> last published status label
)

func initComputeDomainClusterMetrics() {
	computeDomains = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Namespace: "nvidia_dra",
			Name:      "compute_domain_info",
			Help:      "Current number of ComputeDomain custom resources in the cluster as seen by compute-domain-controller, partitioned by compute domain status.",
		},
		[]string{"status"},
	)
}

// RegisterComputeDomainClusterMetrics registers cluster-wide ComputeDomain gauges on the
// Kubernetes legacy metrics registry (included in the controller /metrics endpoint).
func registerComputeDomainClusterMetrics() {
	computeDomainClusterMetricsOnce.Do(func() {
		initComputeDomainClusterMetrics()
		legacyregistry.MustRegister(computeDomains)
	})
}

// ObserveComputeDomainStatus updates gauges for a single ComputeDomain when its global
// status label changes. It is safe to call concurrently from multiple goroutines (CDs are
// synced in parallel), including repeatedly with the same uid and statusLabel (no-op).
// uid must be non-empty.
func ObserveComputeDomainStatus(uid, statusLabel string) {
	if uid == "" {
		return
	}
	registerComputeDomainClusterMetrics()

	computeDomainLastStatusMu.Lock()
	defer computeDomainLastStatusMu.Unlock()

	if computeDomainLastStatus == nil {
		computeDomainLastStatus = make(map[string]string)
	}

	if prev, ok := computeDomainLastStatus[uid]; ok { // previous observation for this UID
		if prev == statusLabel {
			return
		}
		computeDomains.WithLabelValues(prev).Add(-1)
		computeDomains.WithLabelValues(statusLabel).Add(1)
		computeDomainLastStatus[uid] = statusLabel
	} else { // first observation for this UID
		computeDomains.WithLabelValues(statusLabel).Add(1)
		computeDomainLastStatus[uid] = statusLabel
	}
}

// ForgetComputeDomain removes a ComputeDomain UID from metrics (e.g. on informer Delete).
// It is safe to call concurrently with ObserveComputeDomainStatus.
func ForgetComputeDomain(uid string) {
	if uid == "" {
		return
	}
	registerComputeDomainClusterMetrics()

	computeDomainLastStatusMu.Lock()
	defer computeDomainLastStatusMu.Unlock()

	if computeDomainLastStatus == nil {
		return
	}
	prev, ok := computeDomainLastStatus[uid]
	if !ok {
		return
	}
	computeDomains.WithLabelValues(prev).Add(-1)
	delete(computeDomainLastStatus, uid)
}
