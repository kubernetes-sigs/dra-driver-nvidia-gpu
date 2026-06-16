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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	draclient "k8s.io/dynamic-resource-allocation/client"
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
