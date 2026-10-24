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
// queue and workers, and checks that every node ends up with one owner.
func TestClaimAdmissionAtScale(t *testing.T) {
	setChannelReservationsGate(t, true)
	const nodes, computeDomains, claimsPerNode = 500, 200, 4

	var cds []*nvapi.ComputeDomain
	for i := range computeDomains {
		cds = append(cds, cdInNamespace(fmt.Sprintf("cd-%d", i), fmt.Sprintf("ns-%d", i), 0))
	}
	f := newAdmissionFixture(t, imex.ModeDriverManaged, cds...)
	startAdmission(t, f)

	start := time.Now()
	var claims []*resourceapi.ResourceClaim
	for n := range nodes {
		for c := range claimsPerNode {
			// Two ComputeDomains per node, two claims each.
			cd := cds[(n+c/2*7)%computeDomains]
			claim := allocatedChannelClaim(t, fmt.Sprintf("n%d-c%d", n, c), cd.Namespace, string(cd.UID), fmt.Sprintf("node-%d", n), fmt.Sprintf("channel-%d", c%2))
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
		assert.Equal(t, domainID, f.nodeOwner(t, result.Pool))
	}
	assert.Len(t, admittedBy, nodes, "every node is used by one ComputeDomain")
}

func TestClaimAdmissionOwnedNodeNeedsNoReads(t *testing.T) {
	setChannelReservationsGate(t, true)
	f := newAdmissionFixture(t, imex.ModeDriverManaged, cdInNamespace("cd-a", "team-a", 0, 1))

	first := allocatedChannelClaim(t, "a0", "team-a", "cd-a", "node-1", "channel-0")
	f.add(t, first)
	f.admit(t, first)

	second := allocatedChannelClaim(t, "a1", "team-a", "cd-a", "node-1", "channel-1")
	f.add(t, second)
	f.client.ClearActions()
	f.admit(t, second)

	for _, action := range f.client.Actions() {
		if action.GetResource().Resource == "devicetaintrules" {
			t.Errorf("unexpected DeviceTaintRule call for an owned node: %s", action.GetVerb())
		}
	}
}

func TestDeletedNodeRuleIsRestored(t *testing.T) {
	setChannelReservationsGate(t, true)
	f := newAdmissionFixture(t, imex.ModeDriverManaged, cdInNamespace("cd-a", "team-a", 0))
	startAdmission(t, f)
	claim := allocatedChannelClaim(t, "a", "team-a", "cd-a", "node-1", "channel-0")
	_, err := f.client.ResourceV1().ResourceClaims(claim.Namespace).Create(context.Background(), claim, metav1.CreateOptions{})
	require.NoError(t, err)
	waitForDecisions(t, f, []*resourceapi.ResourceClaim{claim}, 10*time.Second)

	require.NoError(t, f.client.ResourceV1().DeviceTaintRules().Delete(context.Background(), nodeOwnershipRuleName("node-1"), metav1.DeleteOptions{}))

	require.Eventually(t, func() bool {
		_, err := f.client.ResourceV1().DeviceTaintRules().Get(context.Background(), nodeOwnershipRuleName("node-1"), metav1.GetOptions{})
		return err == nil
	}, 10*time.Second, 10*time.Millisecond, "the rule is restored without waiting for the periodic sweep")
}

func TestAssertNoClaimsUsesClaimCache(t *testing.T) {
	m, client := testReservationManagerWithClient(t)
	cached := channelClaim(t, "a", true)
	m.claimsForComputeDomain = func(string) []*resourceapi.ResourceClaim { return []*resourceapi.ResourceClaim{&cached} }

	require.Error(t, m.AssertNoClaimsReferenceComputeDomain(context.Background(), "default", "a"))
	assert.Empty(t, client.Actions(), "no ResourceClaim list against the API server")
}

// TestAssertNoClaimsWithUncachedClaim covers a ComputeDomain deleted while a
// claim exists in the API but not yet in the cache.
func TestAssertNoClaimsWithUncachedClaim(t *testing.T) {
	uncached := channelClaim(t, "a", true)
	m, client := testReservationManagerWithClient(t, &uncached)
	m.claimsForComputeDomain = func(string) []*resourceapi.ResourceClaim { return nil }

	require.Error(t, m.AssertNoClaimsReferenceComputeDomain(context.Background(), "default", "a"))
	require.Len(t, client.Actions(), 1)
	assert.Equal(t, "default", client.Actions()[0].GetNamespace(), "only the ComputeDomain's namespace is listed")
}

// TestClaimAdmissionAPIWrites pins down the API cost per claim, which bounds
// admission throughput at the controller's API rate limits.
func TestClaimAdmissionAPIWrites(t *testing.T) {
	setChannelReservationsGate(t, true)

	tests := map[string]struct {
		mode       imex.Mode
		ownedNode  bool
		wantWrites int
	}{
		"driver-managed, first claim on a node": {mode: imex.ModeDriverManaged, wantWrites: 2},
		"driver-managed, owned node":            {mode: imex.ModeDriverManaged, ownedNode: true, wantWrites: 1},
		"host-managed":                          {mode: imex.ModeHostManaged, wantWrites: 1},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			f := newAdmissionFixture(t, tc.mode, cdInNamespace("cd-a", "team-a", 0, 1))
			if tc.ownedNode {
				first := allocatedChannelClaim(t, "first", "team-a", "cd-a", "node-1", "channel-0")
				f.add(t, first)
				f.admit(t, first)
			}
			claim := allocatedChannelClaim(t, "claim", "team-a", "cd-a", "node-1", "channel-1")
			f.add(t, claim)
			f.client.ClearActions()

			f.admit(t, claim)

			writes := 0
			for _, action := range f.client.Actions() {
				switch action.GetVerb() {
				case "create", "update", "patch", "delete":
					writes++
				case "get", "list":
					if action.GetResource().Resource != "resourceclaims" {
						t.Errorf("unexpected %s of %s", action.GetVerb(), action.GetResource().Resource)
					}
				}
			}
			assert.Equal(t, tc.wantWrites, writes)
		})
	}
}

// BenchmarkNodeSweep syncs 10,000 owned nodes with 100,000 cached claims,
// the work of one periodic sweep.
func BenchmarkNodeSweep(b *testing.B) {
	setChannelReservationsGate(b, true)
	const nodes, claimsPerNode = 10000, 10
	f := newAdmissionFixture(b, imex.ModeDriverManaged)
	for n := range nodes {
		node := fmt.Sprintf("node-%d", n)
		owner := fmt.Sprintf("cd-%d", n%500)
		f.m.setNodeOwner(node, owner)
		require.NoError(b, f.m.ruleInformer.GetStore().Add(nodeOwnershipRule(node, owner)))
		for c := range claimsPerNode {
			claim := allocatedChannelClaim(b, fmt.Sprintf("n%d-c%d", n, c), "ns", owner, node, fmt.Sprintf("channel-%d", c))
			require.NoError(b, f.m.informer.GetStore().Add(claim))
		}
	}
	ctx := context.Background()

	b.ResetTimer()
	for b.Loop() {
		for n := range nodes {
			require.NoError(b, f.m.syncNode(ctx, fmt.Sprintf("node-%d", n)))
		}
	}
	assert.Empty(b, f.client.Actions(), "owned nodes in use need no API calls")
}
