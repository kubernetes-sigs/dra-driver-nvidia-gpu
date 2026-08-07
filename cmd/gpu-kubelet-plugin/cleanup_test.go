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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	draclient "k8s.io/dynamic-resource-allocation/client"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

func TestUnprepareIfStale(t *testing.T) {
	tests := []struct {
		name             string
		checkpointUID    string
		checkpointClaim  PreparedClaim
		apiClaim         *resourcev1.ResourceClaim
		expectUnprepared bool
	}{
		{
			name:          "Checkpoint Claim no name",
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
			expectUnprepared: false,
		},
		{
			name:          "API Claim is existed, and UID same",
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
			name:          "API Claim is not existed",
			checkpointUID: "claim-uid",
			checkpointClaim: PreparedClaim{
				Name:      "claim-a",
				Namespace: "default",
			},
			apiClaim: nil,
			expectUnprepared: true,
		},
		{
			name:          "API Claim is existed, but UID is different",
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
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clientset := k8sfake.NewSimpleClientset()
			if tc.apiClaim != nil {
				clientset = k8sfake.NewSimpleClientset(tc.apiClaim)
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
				context.Background(),
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
		})
	}
}
