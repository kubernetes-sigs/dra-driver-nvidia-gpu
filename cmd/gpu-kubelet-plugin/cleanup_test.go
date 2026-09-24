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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	draclient "k8s.io/dynamic-resource-allocation/client"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flock"
)

func TestUnprepareIfStale(t *testing.T) {
	tests := []struct {
		name             string
		checkpointUID    string
		checkpointClaim  PreparedClaim
		apiClaim         *resourcev1.ResourceClaim
		apiError         error
		expectNoAPICall  bool
		expectUnprepared bool
	}{
		{
			name:          "Checkpoint claim has no name",
			checkpointUID: "claim-uid",
			checkpointClaim: PreparedClaim{
				Name:      "",
				Namespace: "default",
			},
			apiClaim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "claim-a",
					Namespace: "default",
					UID:       types.UID("claim-uid"),
				},
			},
			expectNoAPICall:  true,
			expectUnprepared: false,
		},
		{
			name:          "API Claim exists with same UID",
			checkpointUID: "claim-uid",
			checkpointClaim: PreparedClaim{
				Name:      "claim-a",
				Namespace: "default",
			},
			apiClaim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "claim-a",
					Namespace: "default",
					UID:       types.UID("claim-uid"),
				},
			},
			expectUnprepared: false,
		},
		{
			name:          "API Claim does not exist",
			checkpointUID: "claim-uid",
			checkpointClaim: PreparedClaim{
				Name:      "claim-a",
				Namespace: "default",
			},
			apiClaim:         nil,
			expectUnprepared: true,
		},
		{
			name:          "API Claim exists with different UID",
			checkpointUID: "claim-uid",
			checkpointClaim: PreparedClaim{
				Name:      "claim-a",
				Namespace: "default",
			},
			apiClaim: &resourcev1.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "claim-a",
					Namespace: "default",
					UID:       types.UID("claim-diff"),
				},
			},
			expectUnprepared: true,
		},
		{
			name:          "API server returns transient error",
			checkpointUID: "claim-uid",
			checkpointClaim: PreparedClaim{
				Name:      "claim-a",
				Namespace: "default",
			},
			apiError: apierrors.NewInternalError(
				errors.New("temporary API server failure"),
			),
			expectUnprepared: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			clientset := k8sfake.NewSimpleClientset()
			if tc.apiClaim != nil {
				clientset = k8sfake.NewSimpleClientset(tc.apiClaim)
			}
			if tc.apiError != nil {
				clientset.PrependReactor(
					"get",
					"resourceclaims",
					func(action k8stesting.Action) (bool, runtime.Object, error) {
						return true, nil, tc.apiError
					},
				)
			}

			manager := NewCheckpointCleanupManager(nil, draclient.New(clientset))

			var unprepared []kubeletplugin.NamespacedObject
			manager.unprepfunc = func(
				ctx context.Context,
				ref kubeletplugin.NamespacedObject,
			) error {
				unprepared = append(unprepared, ref)
				return nil
			}

			manager.unprepareIfStale(
				ctx,
				tc.checkpointUID,
				tc.checkpointClaim,
			)

			if tc.expectUnprepared {
				require.Len(t, unprepared, 1)

				assert.Equal(t, types.UID(tc.checkpointUID), unprepared[0].UID)
				assert.Equal(t, tc.checkpointClaim.Name, unprepared[0].Name)
				assert.Equal(t, tc.checkpointClaim.Namespace, unprepared[0].Namespace)

			} else {
				assert.Empty(t, unprepared)
			}

			if tc.expectNoAPICall {
				assert.Empty(t, clientset.Actions())
			}

			if tc.apiError != nil {
				actions := clientset.Actions()
				require.Len(t, actions, 1)
				assert.Equal(t, "get", actions[0].GetVerb())
				assert.Equal(t, "resourceclaims", actions[0].GetResource().Resource)
				assert.Equal(t, tc.checkpointClaim.Namespace, actions[0].GetNamespace())
			}
		})
	}
}

func TestCleanupOnlyProcessesPrepareStarted(t *testing.T) {
	checkpoint := &Checkpoint{
		V2: &CheckpointV2{
			PreparedClaims: PreparedClaimsByUIDV2{
				"stale-uid": {
					CheckpointState: ClaimCheckpointStatePrepareStarted,
					Name:            "stale-claim",
					Namespace:       "default",
				},
				"live-uid": {
					CheckpointState: ClaimCheckpointStatePrepareStarted,
					Name:            "live-claim",
					Namespace:       "default",
				},
				"completed-uid": {
					CheckpointState: ClaimCheckpointStatePrepareCompleted,
					Name:            "completed-claim",
					Namespace:       "default",
				},
				"unset-uid": {
					CheckpointState: ClaimCheckpointStateUnset,
					Name:            "unset-claim",
					Namespace:       "default",
				},
			},
		},
	}

	state := newCleanupTestDeviceState(t, checkpoint)

	clientset := k8sfake.NewSimpleClientset(
		&resourcev1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "stale-claim",
				Namespace: "default",
				UID:       types.UID("replacement-uid"),
			},
		},
		&resourcev1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "live-claim",
				Namespace: "default",
				UID:       types.UID("live-uid"),
			},
		},
	)

	manager := NewCheckpointCleanupManager(state, draclient.New(clientset))

	var unprepared []kubeletplugin.NamespacedObject
	manager.unprepfunc = func(ctx context.Context, ref kubeletplugin.NamespacedObject) error {
		unprepared = append(unprepared, ref)
		return nil
	}

	_, ctx := ktesting.NewTestContext(t)

	manager.cleanup(ctx)

	require.Len(t, unprepared, 1)
	assert.Equal(t, types.UID("stale-uid"), unprepared[0].UID)
	assert.Equal(t, "stale-claim", unprepared[0].Name)
	assert.Equal(t, "default", unprepared[0].Namespace)
}

func newCleanupTestDeviceState(t *testing.T, checkpoint *Checkpoint) *DeviceState {
	t.Helper()

	state, _ := newCleanupTestDeviceStateInDir(t, checkpoint)
	return state
}

// Same, and reports the directory, for a test that has to write checkpoint
// bytes of its own.
func newCleanupTestDeviceStateInDir(t *testing.T, checkpoint *Checkpoint) (*DeviceState, string) {
	t.Helper()

	checkpointDir := t.TempDir()
	cpManager, err := checkpointmanager.NewCheckpointManager(checkpointDir)
	require.NoError(t, err)

	if checkpoint != nil {
		require.NoError(t, cpManager.CreateCheckpoint(
			DriverPluginCheckpointFileBasename,
			checkpoint,
		))
	}

	return &DeviceState{
		checkpointManager: cpManager,
		cplock: flock.NewFlock(
			filepath.Join(checkpointDir, "cp.lock"),
		),
	}, checkpointDir
}

func TestEnqueueCleanup(t *testing.T) {
	manager := NewCheckpointCleanupManager(nil, nil)

	require.True(t, manager.enqueueCleanup())
	require.False(t, manager.enqueueCleanup())

	<-manager.queue

	require.True(t, manager.enqueueCleanup())
}
