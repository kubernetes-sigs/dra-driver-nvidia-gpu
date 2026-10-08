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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
	nvfake "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/clientset/versioned/fake"
	nvinformers "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/informers/externalversions"
)

func TestPrepareOverlapRetryIsAbortedByUnprepare(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    imex.Mode
		restart bool
	}{
		{name: "driver managed", mode: imex.ModeDriverManaged},
		{name: "driver managed checkpoint reload", mode: imex.ModeDriverManaged, restart: true},
		{name: "host managed", mode: imex.ModeHostManaged},
		{name: "host managed checkpoint reload", mode: imex.ModeHostManaged, restart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			claim := claimWithResults("waiting-claim", allocationResult("request", DriverName, "channel-0", nil))
			owner := preparedClaim(ClaimCheckpointStatePrepareCompleted, allocationResult("request", DriverName, "channel-0", nil))
			state := newPendingTestState(t, checkpointWithClaims(map[string]PreparedClaim{"owner": owner}))
			state.config.imexConfig = imex.Config{Mode: tc.mode, Isolation: imex.IsolationIMEXDomain}

			devices, err := state.Prepare(ctx, claim)
			require.ErrorContains(t, err, "already allocated to different claim owner")
			require.False(t, isPermanentError(err))
			require.Nil(t, devices)

			if tc.restart {
				state = &DeviceState{checkpointManager: state.checkpointManager, config: state.config}
			}
			// Device teardown and CDI are deliberately unavailable: a waiting
			// claim must not touch resources owned by the completed claim.
			require.NoError(t, state.Unprepare(ctx, pendingClaimRef(claim.Name)))
			cp, err := state.getCheckpoint()
			require.NoError(t, err)
			require.Equal(t, ClaimCheckpointStatePrepareAborted, cp.V2.PreparedClaims[string(claim.UID)].CheckpointState)
			assert.Equal(t, claim.Status, cp.V2.PreparedClaims[string(claim.UID)].Status)
			assert.Equal(t, owner, cp.V2.PreparedClaims["owner"])
			require.NoError(t, state.Unprepare(ctx, pendingClaimRef(claim.Name)))

			// Model the previous owner's release without requiring GPU hardware.
			require.NoError(t, state.deleteClaimFromCheckpoint(pendingClaimRef("owner")))
			if tc.restart {
				state = &DeviceState{checkpointManager: state.checkpointManager, config: state.config}
			}
			devices, err = state.Prepare(ctx, claim)
			require.ErrorContains(t, err, "claim prepare was already aborted")
			assert.True(t, isPermanentError(err))
			assert.Nil(t, devices)
		})
	}
}

func TestPendingPrepareCompletesAfterOwnerReleased(t *testing.T) {
	ctx := context.Background()
	claim := claimWithResults("waiting-claim", allocationResult("request", DriverName, "channel-0", nil))
	state := newPendingTestState(t, checkpointWithClaims(map[string]PreparedClaim{
		"owner": preparedClaim(ClaimCheckpointStatePrepareCompleted, allocationResult("request", DriverName, "channel-0", nil)),
	}))
	enablePendingTestChannelPreparation(t, state, claim)

	for range 2 {
		_, err := state.Prepare(ctx, claim)
		require.ErrorContains(t, err, "already allocated to different claim owner")
		require.False(t, isPermanentError(err))
		cp, err := state.getCheckpoint()
		require.NoError(t, err)
		pending := cp.V2.PreparedClaims[string(claim.UID)]
		assert.Equal(t, ClaimCheckpointStatePreparePending, pending.CheckpointState)
		assert.Equal(t, claim.Name, pending.Name)
		assert.Equal(t, claim.Namespace, pending.Namespace)
		assert.Equal(t, claim.Status, pending.Status)
		assert.Nil(t, pending.PreparedDevices)
	}

	require.NoError(t, state.deleteClaimFromCheckpoint(pendingClaimRef("owner")))
	devices, err := state.Prepare(ctx, claim)
	require.NoError(t, err)
	require.Len(t, devices, 1)
	assert.Equal(t, "channel-0", devices[0].DeviceName)
	cp, err := state.getCheckpoint()
	require.NoError(t, err)
	assert.Equal(t, ClaimCheckpointStatePrepareCompleted, cp.V2.PreparedClaims[string(claim.UID)].CheckpointState)

	repeated, err := state.Prepare(ctx, claim)
	require.NoError(t, err)
	assert.Equal(t, devices, repeated)
}

func TestAbortedPendingClaimAllowsNewReservation(t *testing.T) {
	ctx := context.Background()
	claim := claimWithResults("reused-claim", allocationResult("request", DriverName, "channel-0", nil))
	claim.Status.ReservedFor = []resourceapi.ResourceClaimConsumerReference{{Resource: "pods", Name: "old-pod", UID: "old-pod-uid"}}
	state := newPendingTestState(t, checkpointWithClaims(map[string]PreparedClaim{
		"owner": preparedClaim(ClaimCheckpointStatePrepareCompleted, allocationResult("request", DriverName, "channel-0", nil)),
	}))
	enablePendingTestChannelPreparation(t, state, claim)
	_, err := state.Prepare(ctx, claim)
	require.ErrorContains(t, err, "already allocated to different claim owner")
	require.NoError(t, state.Unprepare(ctx, pendingClaimRef(claim.Name)))

	_, err = state.Prepare(ctx, claim)
	require.True(t, isPermanentError(err))

	reused := claim.DeepCopy()
	reused.Status.ReservedFor = []resourceapi.ResourceClaimConsumerReference{{Resource: "pods", Name: "new-pod", UID: "new-pod-uid"}}
	_, err = state.Prepare(ctx, reused)
	require.ErrorContains(t, err, "already allocated to different claim owner")
	require.False(t, isPermanentError(err))
	cp, err := state.getCheckpoint()
	require.NoError(t, err)
	pending := cp.V2.PreparedClaims[string(reused.UID)]
	assert.Equal(t, ClaimCheckpointStatePreparePending, pending.CheckpointState)
	assert.Equal(t, reused.Status, pending.Status)
	assert.Nil(t, pending.AbortedAt)

	require.NoError(t, state.deleteClaimFromCheckpoint(pendingClaimRef("owner")))
	devices, err := state.Prepare(ctx, reused)
	require.NoError(t, err)
	require.Len(t, devices, 1)
}

func TestPreparePreservesStartedStateOnOverlap(t *testing.T) {
	claim := claimWithResults("started-claim", allocationResult("request", DriverName, "channel-0", nil))
	started := preparedClaim(ClaimCheckpointStatePrepareStarted, claim.Status.Allocation.Devices.Results...)
	state := newPendingTestState(t, checkpointWithClaims(map[string]PreparedClaim{
		string(claim.UID): started,
		"owner":           preparedClaim(ClaimCheckpointStatePrepareCompleted, claim.Status.Allocation.Devices.Results...),
	}))

	_, err := state.Prepare(context.Background(), claim)
	require.ErrorContains(t, err, "already allocated to different claim owner")
	cp, err := state.getCheckpoint()
	require.NoError(t, err)
	assert.Equal(t, started, cp.V2.PreparedClaims[string(claim.UID)])
}

func TestPendingPrepareTracksChangedStatus(t *testing.T) {
	claim := claimWithResults("waiting-claim", allocationResult("request", DriverName, "channel-0", nil))
	state := newPendingTestState(t, checkpointWithClaims(map[string]PreparedClaim{
		"owner": preparedClaim(ClaimCheckpointStatePrepareCompleted, claim.Status.Allocation.Devices.Results...),
	}))
	manager := &failingPendingCheckpointManager{CheckpointManager: state.checkpointManager}
	state.checkpointManager = manager
	ctx := context.Background()

	for range 2 {
		_, err := state.Prepare(ctx, claim)
		require.ErrorContains(t, err, "already allocated to different claim owner")
	}
	assert.Equal(t, 1, manager.writes, "unchanged retries should not rewrite pending state")

	updated := claim.DeepCopy()
	updated.Status.ReservedFor = []resourceapi.ResourceClaimConsumerReference{{Resource: "pods", Name: "new-pod", UID: "new-pod-uid"}}
	_, err := state.Prepare(ctx, updated)
	require.ErrorContains(t, err, "already allocated to different claim owner")
	assert.Equal(t, 2, manager.writes)
	cp, err := state.getCheckpoint()
	require.NoError(t, err)
	assert.Equal(t, updated.Status, cp.V2.PreparedClaims[string(updated.UID)].Status)

	require.NoError(t, state.Unprepare(ctx, pendingClaimRef(updated.Name)))
	_, err = state.Prepare(ctx, updated)
	assert.True(t, isPermanentError(err))
}

func TestPreparePendingCheckpointWriteFailure(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		name := "pending write"
		if failAt == 2 {
			name = "started write"
		}
		t.Run(name, func(t *testing.T) {
			state := newPendingTestState(t, checkpointWithClaims(nil))
			writeErr := errors.New("checkpoint storage unavailable")
			state.checkpointManager = &failingPendingCheckpointManager{
				CheckpointManager: state.checkpointManager,
				failAt:            failAt,
				err:               writeErr,
			}
			claim := claimWithResults("claim", allocationResult("request", DriverName, "channel-0", nil))

			// No device or CDI dependencies: preparation must stop at the
			// failed durable transition, before touching any device.
			devices, err := state.Prepare(context.Background(), claim)
			require.ErrorIs(t, err, writeErr)
			assert.Nil(t, devices)
			assert.False(t, isPermanentError(err))
			cp, err := state.getCheckpoint()
			require.NoError(t, err)
			if failAt == 1 {
				assert.NotContains(t, cp.V2.PreparedClaims, string(claim.UID))
			} else {
				assert.Equal(t, ClaimCheckpointStatePreparePending, cp.V2.PreparedClaims[string(claim.UID)].CheckpointState)
			}
		})
	}
}

func TestUnpreparePendingPropagatesCheckpointFailure(t *testing.T) {
	claim := claimWithResults("claim", allocationResult("request", DriverName, "channel-0", nil))
	state := newPendingTestState(t, checkpointWithClaims(map[string]PreparedClaim{
		string(claim.UID): preparedClaim(ClaimCheckpointStatePreparePending, claim.Status.Allocation.Devices.Results...),
	}))
	writeErr := errors.New("checkpoint storage unavailable")
	manager := state.checkpointManager
	state.checkpointManager = &failingPendingCheckpointManager{
		CheckpointManager: manager,
		failAt:            1,
		err:               writeErr,
	}

	err := state.Unprepare(context.Background(), pendingClaimRef(claim.Name))
	require.ErrorIs(t, err, writeErr)
	cp, err := state.getCheckpoint()
	require.NoError(t, err)
	assert.Equal(t, ClaimCheckpointStatePreparePending, cp.V2.PreparedClaims[string(claim.UID)].CheckpointState)

	state.checkpointManager = manager
	require.NoError(t, state.Unprepare(context.Background(), pendingClaimRef(claim.Name)))
	cp, err = state.getCheckpoint()
	require.NoError(t, err)
	assert.Equal(t, ClaimCheckpointStatePrepareAborted, cp.V2.PreparedClaims[string(claim.UID)].CheckpointState)
}

func TestPendingCheckpointRoundTrip(t *testing.T) {
	pending := preparedClaim(ClaimCheckpointStatePreparePending, allocationResult("request", DriverName, "channel-0", nil))
	pending.Name, pending.Namespace = "pending", "default"
	started := preparedClaim(ClaimCheckpointStatePrepareStarted, allocationResult("request", DriverName, "daemon-0", nil))
	completed := preparedClaim(ClaimCheckpointStatePrepareCompleted, allocationResult("request", DriverName, "channel-1", nil))
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	aborted := preparedClaim(ClaimCheckpointStatePrepareAborted, allocationResult("request", DriverName, "channel-2", nil))
	aborted.AbortedAt = &now
	cp := checkpointWithClaims(map[string]PreparedClaim{
		"pending": pending, "started": started, "completed": completed, "aborted": aborted,
	})

	data, err := cp.MarshalCheckpoint()
	require.NoError(t, err)
	restored := &Checkpoint{}
	require.NoError(t, restored.UnmarshalCheckpoint(data))
	require.NoError(t, restored.VerifyChecksum())
	assert.Equal(t, cp.V2.PreparedClaims, restored.ToLatestVersion().V2.PreparedClaims)
	assert.Len(t, restored.V1.PreparedClaims, 1)
	assert.Contains(t, restored.V1.PreparedClaims, "completed")

	legacy := &Checkpoint{V1: &CheckpointV1{PreparedClaims: PreparedClaimsByUIDV1{
		"completed": {Status: completed.Status},
	}}}
	require.NoError(t, legacy.SetChecksumV1())
	assert.Equal(t, ClaimCheckpointStatePrepareCompleted, legacy.ToLatestVersion().V2.PreparedClaims["completed"].CheckpointState)
}

type failingPendingCheckpointManager struct {
	checkpointmanager.CheckpointManager
	failAt int
	writes int
	err    error
}

func (m *failingPendingCheckpointManager) CreateCheckpoint(name string, cp checkpointmanager.Checkpoint) error {
	m.writes++
	if m.writes == m.failAt {
		return m.err
	}
	return m.CheckpointManager.CreateCheckpoint(name, cp)
}

func enablePendingTestChannelPreparation(t *testing.T, state *DeviceState, claim *resourceapi.ResourceClaim) {
	t.Helper()
	domainID := "123e4567-e89b-12d3-a456-426614174000"
	domain := &configapi.ComputeDomain{
		ObjectMeta: metav1.ObjectMeta{Namespace: claim.Namespace, Name: "domain", UID: types.UID(domainID)},
	}
	factory := nvinformers.NewSharedInformerFactory(nvfake.NewSimpleClientset(), 0)
	informer := factory.Resource().V1beta1().ComputeDomains().Informer()
	require.NoError(t, informer.AddIndexers(cache.Indexers{"computeDomainUID": uidIndexer[*configapi.ComputeDomain]}))
	require.NoError(t, informer.GetIndexer().Add(domain))
	state.computeDomainManager = &ComputeDomainManager{informer: informer}
	state.config = hostManagedConfig()
	state.config.flags.nodeName = "test-node"
	state.allocatable = testDeviceState().allocatable
	state.cdi = &CDIHandler{}
	claim.Status.Allocation.Devices.Config = []resourceapi.DeviceAllocationConfiguration{
		opaqueConfig(t, resourceapi.AllocationConfigSourceClaim, DriverName, nil, channelConfig(domainID)),
	}
}

func newPendingTestState(t *testing.T, cp *Checkpoint) *DeviceState {
	t.Helper()
	manager, err := checkpointmanager.NewCheckpointManager(t.TempDir())
	require.NoError(t, err)
	state := testCheckpointDeviceState(cp)
	state.checkpointManager = manager
	require.NoError(t, state.createCheckpoint(cp))
	return state
}

func pendingClaimRef(name string) kubeletplugin.NamespacedObject {
	return kubeletplugin.NamespacedObject{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: name},
		UID:            types.UID(name),
	}
}
