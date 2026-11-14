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
	"path/filepath"
	"sort"
	"testing"
	"testing/synctest"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flock"
)

func deviceNames(devices []resourceapi.Device) []string {
	names := make([]string, 0, len(devices))
	for _, d := range devices {
		names = append(names, d.Name)
	}
	sort.Strings(names)
	return names
}

func TestComputeDomainPublishedDevices(t *testing.T) {
	allocatable := AllocatableDevices{
		"channel-0": &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: 0}},
		"channel-1": &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: 1}},
		"daemon-0":  &AllocatableDevice{Daemon: &ComputeDomainDaemonInfo{ID: 0}},
	}

	t.Run("driver-managed publishes channel 0 and the daemon device", func(t *testing.T) {
		// Non-zero channels are always excluded (advertised from the control plane).
		assert.Equal(t, []string{"channel-0", "daemon-0"}, deviceNames(computeDomainPublishedDevices(allocatable, false)))
	})

	t.Run("host-managed omits the daemon device", func(t *testing.T) {
		assert.Equal(t, []string{"channel-0"}, deviceNames(computeDomainPublishedDevices(allocatable, true)))
	})
}

func TestResourceClaimRequestsLeaveNoGoroutineBehind(t *testing.T) {
	cases := map[string]struct {
		lockHeld    bool
		cancelled   bool
		wantElapsed time.Duration
	}{
		"caller cancels before any attempt": {cancelled: true},
		// Holding the lock fails every attempt with a retryable error.
		"claim is still retried at the deadline": {lockHeld: true, wantElapsed: ErrorRetryMaxTimeout},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// synctest.Test panics if a goroutine is still durably blocked once
			// the calls return; its fake clock skips ErrorRetryMaxTimeout.
			synctest.Test(t, func(t *testing.T) {
				d := testRequestDriver(t, tc.lockHeld)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.cancelled {
					cancel()
				}

				start := time.Now()
				_, err := d.PrepareResourceClaims(ctx, []*resourceapi.ResourceClaim{testRequestClaim("a")})
				require.NoError(t, err)
				require.Equal(t, tc.wantElapsed, time.Since(start))

				start = time.Now()
				_, err = d.UnprepareResourceClaims(ctx, []kubeletplugin.NamespacedObject{testRequestClaimRef("a")})
				require.NoError(t, err)
				require.Equal(t, tc.wantElapsed, time.Since(start))
			})
		})
	}
}

func TestResourceClaimRequestsReturnOnceEveryClaimHasAResult(t *testing.T) {
	cases := map[string][]string{
		"no claims":  nil,
		"one claim":  {"a"},
		"two claims": {"a", "b"},
	}

	for name, names := range cases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := testRequestDriver(t, false)
				var claims []*resourceapi.ResourceClaim
				var claimRefs []kubeletplugin.NamespacedObject
				for _, n := range names {
					claims = append(claims, testRequestClaim(n))
					claimRefs = append(claimRefs, testRequestClaimRef(n))
				}
				start := time.Now()

				prepared, err := d.PrepareResourceClaims(t.Context(), claims)
				require.NoError(t, err)
				require.Len(t, prepared, len(names))
				unprepared, err := d.UnprepareResourceClaims(t.Context(), claimRefs)
				require.NoError(t, err)
				require.Len(t, unprepared, len(names))

				require.Less(t, time.Since(start), ErrorRetryMaxTimeout, "the requests waited out the timeout")
			})
		})
	}
}

func testRequestDriver(t *testing.T, lockHeld bool) *driver {
	t.Helper()

	lockPath := filepath.Join(t.TempDir(), DriverPrepUprepFlockFileName)
	if lockHeld {
		release, err := flock.NewFlock(lockPath).Acquire(t.Context())
		require.NoError(t, err)
		t.Cleanup(release)
	}
	return &driver{
		pulock: flock.NewFlock(lockPath),
		state:  &DeviceState{checkpointManager: &fakeCheckpointManager{checkpoint: checkpointWithClaims(nil)}},
	}
}

// testRequestClaim has no allocation, so Prepare returns a final result on its
// first attempt.
func testRequestClaim(name string) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: types.UID(name + "-uid")}}
}

func testRequestClaimRef(name string) kubeletplugin.NamespacedObject {
	return kubeletplugin.NamespacedObject{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}, UID: types.UID(name + "-uid")}
}
