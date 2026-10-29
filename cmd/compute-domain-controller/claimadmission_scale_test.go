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
	"context"
	"fmt"
	"testing"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
)

// startAdmission runs the admission manager with its workers against the
// fixture's fake API server.
func startAdmission(t testing.TB, f *admissionFixture) {
	t.Helper()
	bufferWatches(f.client)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, f.m.Start(ctx))
	t.Cleanup(func() {
		cancel()
		require.NoError(t, f.m.Stop())
	})
}

// bufferWatches lets the fake API server deliver thousands of watch events:
// its own watchers panic once 100 events are pending.
func bufferWatches(client *fake.Clientset) {
	client.PrependWatchReactor("*", func(action k8stesting.Action) (bool, watch.Interface, error) {
		var opts metav1.ListOptions
		if wa, ok := action.(k8stesting.WatchActionImpl); ok {
			opts = wa.ListOptions
		}
		w, err := client.Tracker().Watch(action.GetResource(), action.GetNamespace(), opts)
		if err != nil {
			return true, nil, err
		}
		events := make(chan watch.Event, 100000)
		proxy := watch.NewProxyWatcher(events)
		go func() {
			defer close(events)
			for {
				select {
				case e, ok := <-w.ResultChan():
					if !ok {
						return
					}
					events <- e
				case <-proxy.StopChan():
					w.Stop()
					return
				}
			}
		}()
		return true, proxy, nil
	})
}

// waitForDecisions waits until every claim has an admission decision.
func waitForDecisions(t testing.TB, f *admissionFixture, claims []*resourceapi.ResourceClaim, timeout time.Duration) map[string]string {
	t.Helper()
	decisions := make(map[string]string, len(claims))
	deadline := time.Now().Add(timeout)
	for len(decisions) < len(claims) && time.Now().Before(deadline) {
		for _, claim := range claims {
			if _, done := decisions[claim.Name]; done {
				continue
			}
			got, err := f.client.ResourceV1().ResourceClaims(claim.Namespace).Get(context.Background(), claim.Name, metav1.GetOptions{})
			require.NoError(t, err)
			for _, status := range got.Status.Devices {
				for _, c := range []string{imex.ChannelAdmittedCondition, imex.ChannelRejectedCondition} {
					if meta.IsStatusConditionTrue(status.Conditions, c) {
						decisions[claim.Name] = c
					}
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.Len(t, decisions, len(claims), "not all claims were decided in time")
	return decisions
}

// TestClaimAdmissionAtScale has ComputeDomains contend for nodes through the
// queue and workers, and checks that every node ends up with one user.
func TestClaimAdmissionAtScale(t *testing.T) {
	setChannelReservationsGate(t, true)
	const nodes, computeDomains, claimsPerNode = 500, 200, 4

	var cds []*nvapi.ComputeDomain
	for i := range computeDomains {
		cds = append(cds, cdInNamespace(fmt.Sprintf("cd-%d", i), fmt.Sprintf("ns-%d", i)))
	}
	f := newAdmissionFixture(t, imex.ModeDriverManaged, cds...)
	for n := range nodes {
		f.node(t, fmt.Sprintf("node-%d", n))
	}
	startAdmission(t, f)

	start := time.Now()
	var claims []*resourceapi.ResourceClaim
	for n := range nodes {
		for c := range claimsPerNode {
			// Two ComputeDomains per node, two claims each.
			cd := cds[(n+c/2*7)%computeDomains]
			claim := allocatedChannelClaim(t, fmt.Sprintf("n%d-c%d", n, c), cd.Namespace, string(cd.UID), fmt.Sprintf("node-%d", n), "job")
			_, err := f.client.ResourceV1().ResourceClaims(claim.Namespace).Create(context.Background(), claim, metav1.CreateOptions{})
			require.NoError(t, err)
			claims = append(claims, claim)
		}
		// The fake API server panics once a watcher falls 100 events behind.
		if len(claims)%50 == 0 {
			require.Eventually(t, func() bool { return len(f.m.informer.GetStore().List()) >= len(claims) }, 10*time.Second, time.Millisecond)
		}
	}

	decisions := waitForDecisions(t, f, claims, 2*time.Minute)
	t.Logf("decided %d claims on %d nodes in %v", len(claims), nodes, time.Since(start))

	admittedBy := make(map[string]string)
	for _, claim := range claims {
		result := claim.Status.Allocation.Devices.Results[0]
		domainID := channelDomainID(claim, result.Request)
		if decisions[claim.Name] != imex.ChannelAdmittedCondition {
			continue
		}
		if owner, ok := admittedBy[result.Pool]; ok {
			require.Equal(t, owner, domainID, "node %s admitted claims of two ComputeDomains", result.Pool)
		}
		admittedBy[result.Pool] = domainID
	}
	assert.Len(t, admittedBy, nodes, "every node is used by one ComputeDomain")
	for node, owner := range admittedBy {
		assert.Equal(t, owner, f.owner(t, node), "node %s is labeled for the ComputeDomain admitted there", node)
	}
}

// TestClaimAdmissionAPIWrites pins down the API cost per claim, which bounds
// admission throughput at the controller's API rate limits: one claim status
// update, plus a node read and label for the first claim of a ComputeDomain
// on a node.
func TestClaimAdmissionAPIWrites(t *testing.T) {
	setChannelReservationsGate(t, true)

	tests := map[string]struct {
		mode       imex.Mode
		usedNode   bool
		wantWrites int
		wantReads  int
	}{
		"driver-managed, first claim on a node": {mode: imex.ModeDriverManaged, wantWrites: 2, wantReads: 1},
		"driver-managed, node in use":           {mode: imex.ModeDriverManaged, usedNode: true, wantWrites: 1},
		"host-managed":                          {mode: imex.ModeHostManaged, wantWrites: 1},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			f := newAdmissionFixture(t, tc.mode, cdInNamespace("cd-a", "team-a"))
			if tc.usedNode {
				first := allocatedChannelClaim(t, "first", "team-a", "cd-a", "node-1", "job")
				f.add(t, first)
				f.admit(t, first)
				f.sync(t, first)
				f.syncNode(t, "node-1")
			}
			claim := allocatedChannelClaim(t, "claim", "team-a", "cd-a", "node-1", "job")
			f.add(t, claim)
			f.client.ClearActions()

			require.NoError(t, f.m.admitKey(context.Background(), "team-a/claim"))

			writes, reads := 0, 0
			for _, action := range f.client.Actions() {
				switch {
				case action.GetVerb() == "update":
					writes++
				case action.GetVerb() == "get" && action.GetResource().Resource == "nodes":
					reads++
				default:
					t.Errorf("unexpected %s of %s", action.GetVerb(), action.GetResource().Resource)
				}
			}
			assert.Equal(t, tc.wantWrites, writes)
			assert.Equal(t, tc.wantReads, reads)
		})
	}
}

// BenchmarkAdmissionOnBusyNode decides claims on a node holding many claims
// of its user, the cost of deriving node ownership from the cache.
func BenchmarkAdmissionOnBusyNode(b *testing.B) {
	setChannelReservationsGate(b, true)
	const claimsOnNode = 1000
	cd := cdInNamespace("cd-a", "team-a")
	f := newAdmissionFixture(b, imex.ModeDriverManaged, cd)
	for i := range claimsOnNode {
		claim := allocatedChannelClaim(b, fmt.Sprintf("c%d", i), "team-a", "cd-a", "node-1", "job")
		result := claim.Status.Allocation.Devices.Results[0]
		setChannelAdmissionCondition(claim, &result, admitDecision())
		require.NoError(b, f.m.informer.GetStore().Add(claim))
	}

	b.ResetTimer()
	for b.Loop() {
		if other := f.m.otherNodeUser("node-1", "cd-b", "new"); other != "cd-a" {
			b.Fatalf("expected cd-a to use the node, got %q", other)
		}
	}
}
