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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	draclient "k8s.io/dynamic-resource-allocation/client"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

func TestCleanupDeletesExpiredPrepareAbortedEntries(t *testing.T) {
	now := time.Now()
	oldAbortedAt := metav1.NewTime(now.Add(-PrepareAbortedClaimEntryTTL - time.Second))
	freshAbortedAt := metav1.NewTime(now.Add(-PrepareAbortedClaimEntryTTL / 2))
	state := testCheckpointDeviceState(checkpointWithClaims(map[string]PreparedClaim{
		"old-aborted": {
			CheckpointState: ClaimCheckpointStatePrepareAborted,
			AbortedAt:       &oldAbortedAt,
		},
		"fresh-aborted": {
			CheckpointState: ClaimCheckpointStatePrepareAborted,
			AbortedAt:       &freshAbortedAt,
		},
	}))

	manager := NewCheckpointCleanupManager(state, (*draclient.Client)(nil))
	expiredCleanupCalled := false
	manager.expiredEntryCleanupFn = func(ctx context.Context, now time.Time, ttl time.Duration) (int, error) {
		expiredCleanupCalled = true
		assert.Equal(t, PrepareAbortedClaimEntryTTL, ttl)
		return state.deleteExpiredPrepareAbortedClaimsFromCheckpoint(now, ttl)
	}

	manager.cleanup(context.Background())

	require.True(t, expiredCleanupCalled)
	stored := requireFakeCheckpointManager(t, state).checkpoint.V2.PreparedClaims
	assert.NotContains(t, stored, "old-aborted")
	assert.Contains(t, stored, "fresh-aborted")
}

func TestCleanupPendingClaims(t *testing.T) {
	for _, tc := range []struct {
		name      string
		apiUID    string
		apiError  bool
		wantAbort bool
	}{
		{name: "deleted", wantAbort: true},
		{name: "replaced", apiUID: "replacement", wantAbort: true},
		{name: "live", apiUID: "pending"},
		{name: "API unavailable", apiError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim := claimWithResults("pending", allocationResult("request", DriverName, "channel-0", nil))
			entry := PreparedClaim{
				CheckpointState: ClaimCheckpointStatePreparePending,
				Name:            claim.Name,
				Namespace:       claim.Namespace,
				Status:          claim.Status,
			}
			state := newPendingTestState(t, checkpointWithClaims(map[string]PreparedClaim{
				"pending": entry,
				"completed": {
					CheckpointState: ClaimCheckpointStatePrepareCompleted,
					Name:            "completed",
					Namespace:       "default",
				},
			}))
			client := fake.NewClientset()
			if tc.apiUID != "" {
				apiClaim := claim.DeepCopy()
				apiClaim.UID = pendingClaimRef(tc.apiUID).UID
				_, err := client.ResourceV1().ResourceClaims(claim.Namespace).Create(context.Background(), apiClaim, metav1.CreateOptions{})
				require.NoError(t, err)
			}
			if tc.apiError {
				client.PrependReactor("get", "resourceclaims", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewServiceUnavailable("API unavailable")
				})
			}
			manager := NewCheckpointCleanupManager(state, draclient.New(client))
			var unprepared []string
			manager.unprepfunc = func(ctx context.Context, ref kubeletplugin.NamespacedObject) (bool, error) {
				unprepared = append(unprepared, string(ref.UID))
				return true, state.Unprepare(ctx, ref)
			}
			manager.expiredEntryCleanupFn = func(_ context.Context, now time.Time, ttl time.Duration) (int, error) {
				return state.deleteExpiredPrepareAbortedClaimsFromCheckpoint(now, ttl)
			}

			manager.cleanup(context.Background())

			cp, err := state.getCheckpoint()
			require.NoError(t, err)
			if tc.wantAbort {
				assert.Equal(t, []string{"pending"}, unprepared)
				assert.Equal(t, ClaimCheckpointStatePrepareAborted, cp.V2.PreparedClaims["pending"].CheckpointState)
				assert.Equal(t, claim.Status, cp.V2.PreparedClaims["pending"].Status)
				require.NotNil(t, cp.V2.PreparedClaims["pending"].AbortedAt)
				count, err := state.deleteExpiredPrepareAbortedClaimsFromCheckpoint(time.Now().Add(PrepareAbortedClaimEntryTTL+time.Second), PrepareAbortedClaimEntryTTL)
				require.NoError(t, err)
				assert.Equal(t, 1, count)
			} else {
				assert.Empty(t, unprepared)
				assert.Equal(t, entry, cp.V2.PreparedClaims["pending"])
			}
			assert.Equal(t, ClaimCheckpointStatePrepareCompleted, cp.V2.PreparedClaims["completed"].CheckpointState)
			for _, action := range client.Actions() {
				if action.GetVerb() == "get" {
					getAction, ok := action.(clienttesting.GetAction)
					require.True(t, ok)
					assert.Equal(t, "pending", getAction.GetName())
				}
			}
		})
	}
}

func TestCleanupStillIncludesStartedClaims(t *testing.T) {
	state := newPendingTestState(t, checkpointWithClaims(map[string]PreparedClaim{
		"started": {
			CheckpointState: ClaimCheckpointStatePrepareStarted,
			Name:            "started",
			Namespace:       "default",
		},
	}))
	manager := NewCheckpointCleanupManager(state, draclient.New(fake.NewClientset()))
	called := false
	manager.unprepfunc = func(_ context.Context, ref kubeletplugin.NamespacedObject) (bool, error) {
		called = true
		assert.Equal(t, pendingClaimRef("started"), ref)
		return true, nil
	}
	manager.expiredEntryCleanupFn = func(_ context.Context, now time.Time, ttl time.Duration) (int, error) {
		return state.deleteExpiredPrepareAbortedClaimsFromCheckpoint(now, ttl)
	}
	manager.cleanup(context.Background())
	assert.True(t, called)
}
