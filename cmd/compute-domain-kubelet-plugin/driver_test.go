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
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
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
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
)

func deviceNames(devices []resourceapi.Device) []string {
	names := make([]string, 0, len(devices))
	for _, d := range devices {
		names = append(names, d.Name)
	}
	sort.Strings(names)
	return names
}

func driverManagedConfig() *Config {
	return &Config{
		flags:      &Flags{},
		imexConfig: imex.Config{DaemonLifecycle: imex.DaemonLifecyclePerComputeDomain},
	}
}

func TestComputeDomainPublishedDevices(t *testing.T) {
	allocatable := AllocatableDevices{
		"channel-0": &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: 0}},
		"channel-1": &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: 1}},
		"daemon-0":  &AllocatableDevice{Daemon: &ComputeDomainDaemonInfo{ID: 0}},
	}

	t.Run("driver-managed publishes channel 0 and the daemon device", func(t *testing.T) {
		// Non-zero channels are always excluded (advertised from the control plane).
		assert.Equal(t, []string{"channel-0", "daemon-0"}, deviceNames(computeDomainPublishedDevices(allocatable, driverManagedConfig(), channelDeviceState{})))
	})

	t.Run("host-managed omits the daemon device", func(t *testing.T) {
		assert.Equal(t, []string{"channel-0"}, deviceNames(computeDomainPublishedDevices(allocatable, hostManagedConfig(), channelDeviceState{})))
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
		"claim is still retried at the deadline": {lockHeld: true, wantElapsed: RetryMaxTimeout},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// synctest.Test panics if a goroutine is still durably blocked once
			// the calls return; its fake clock skips RetryMaxTimeout.
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

// kubeletCallTimeout is the kubelet's timeout; later responses are lost.
const kubeletCallTimeout = 45 * time.Second

func TestResourceClaimRequestsReturnTheCauseBeforeTheKubeletGivesUp(t *testing.T) {
	cases := map[string]struct {
		claims int
		// wantErr is the error of at least one claim.
		wantErr string
	}{
		// The last attempt ends at the deadline, after an earlier lock timeout.
		"one claim": {claims: 1, wantErr: "timeout acquiring lock"},
		// Each attempt waits 10s for the lock, so some claims get none.
		"more claims than attempts": {claims: 6, wantErr: "not attempted before the retries stopped"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := testRequestDriver(t, true)
				var claims []*resourceapi.ResourceClaim
				var claimRefs []kubeletplugin.NamespacedObject
				for i := range tc.claims {
					claims = append(claims, testRequestClaim(string(rune('a'+i))))
					claimRefs = append(claimRefs, testRequestClaimRef(string(rune('a'+i))))
				}
				hasWantErr := func(err error) bool { return err != nil && strings.Contains(err.Error(), tc.wantErr) }

				ctx, cancel := context.WithTimeout(t.Context(), kubeletCallTimeout)
				defer cancel()
				prepared, err := d.PrepareResourceClaims(ctx, claims)
				require.NoError(t, err)
				require.NoError(t, ctx.Err(), "the kubelet gave up first")
				var errs []error
				for _, claim := range claims {
					require.Error(t, prepared[claim.UID].Err, claim.Name)
					errs = append(errs, prepared[claim.UID].Err)
				}
				require.True(t, slices.ContainsFunc(errs, hasWantErr), "%v", errs)

				ctx, cancel = context.WithTimeout(t.Context(), kubeletCallTimeout)
				defer cancel()
				unprepared, err := d.UnprepareResourceClaims(ctx, claimRefs)
				require.NoError(t, err)
				require.NoError(t, ctx.Err(), "the kubelet gave up first")
				errs = nil
				for _, ref := range claimRefs {
					require.Error(t, unprepared[ref.UID], ref.Name)
					errs = append(errs, unprepared[ref.UID])
				}
				require.True(t, slices.ContainsFunc(errs, hasWantErr), "%v", errs)
			})
		})
	}
}

func TestResourceClaimRequestsReturnTheResultOfTheLastAttempt(t *testing.T) {
	t.Run("a later attempt succeeds", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := testRequestDriver(t, false)
			claim := testRequestClaim("a")
			// The first attempt times out on the lock, a later one gets it.
			holdLock := func() {
				release, err := d.pulock.Acquire(t.Context())
				require.NoError(t, err)
				time.AfterFunc(15*time.Second, release)
			}

			holdLock()
			prepared, err := d.PrepareResourceClaims(t.Context(), []*resourceapi.ResourceClaim{claim})
			require.NoError(t, err)
			require.ErrorContains(t, prepared[claim.UID].Err, "no allocation set")

			holdLock()
			unprepared, err := d.UnprepareResourceClaims(t.Context(), []kubeletplugin.NamespacedObject{testRequestClaimRef("a")})
			require.NoError(t, err)
			require.Contains(t, unprepared, claim.UID)
			require.NoError(t, unprepared[claim.UID])
		})
	})

	t.Run("a later attempt fails with another error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := testRequestDriver(t, false)
			claim := testRequestClaim("a")
			dir := filepath.Join(t.TempDir(), "lock")
			lockPath := filepath.Join(dir, DriverPrepUprepFlockFileName)
			d.pulock = flock.NewFlock(lockPath)
			// Opening the lock file fails until 5s, then the lock is held.
			holdLockLater := func() (release func()) {
				held := make(chan func(), 1)
				time.AfterFunc(5*time.Second, func() {
					assert.NoError(t, os.Mkdir(dir, 0o755))
					release, err := flock.NewFlock(lockPath).Acquire(context.Background())
					assert.NoError(t, err)
					held <- release
				})
				return func() {
					(<-held)()
					require.NoError(t, os.RemoveAll(dir))
				}
			}

			release := holdLockLater()
			prepared, err := d.PrepareResourceClaims(t.Context(), []*resourceapi.ResourceClaim{claim})
			require.NoError(t, err)
			require.ErrorContains(t, prepared[claim.UID].Err, "timeout acquiring lock")
			release()

			release = holdLockLater()
			unprepared, err := d.UnprepareResourceClaims(t.Context(), []kubeletplugin.NamespacedObject{testRequestClaimRef("a")})
			require.NoError(t, err)
			require.ErrorContains(t, unprepared[claim.UID], "timeout acquiring lock")
			release()
		})
	})

	t.Run("an attempt ends after the retries stop", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := testRequestDriver(t, false)
			claim := testRequestClaim("a")
			claim.Status.Allocation = &resourceapi.AllocationResult{}
			// The attempt that gets the lock at 30s reads the checkpoint until
			// 50s, after the retries stop at 40s.
			slowCheckpoint := func(claims map[string]PreparedClaim) {
				d.state.checkpointManager = &fakeCheckpointManager{checkpoint: checkpointWithClaims(claims), getDelay: 20 * time.Second}
			}
			holdLock := func() {
				release, err := d.pulock.Acquire(t.Context())
				require.NoError(t, err)
				time.AfterFunc(30*time.Second, release)
			}

			slowCheckpoint(map[string]PreparedClaim{string(claim.UID): {CheckpointState: ClaimCheckpointStatePrepareCompleted}})
			holdLock()
			prepared, err := d.PrepareResourceClaims(t.Context(), []*resourceapi.ResourceClaim{claim})
			require.NoError(t, err)
			require.Contains(t, prepared, claim.UID)
			require.NoError(t, prepared[claim.UID].Err)

			slowCheckpoint(nil)
			holdLock()
			unprepared, err := d.UnprepareResourceClaims(t.Context(), []kubeletplugin.NamespacedObject{testRequestClaimRef("a")})
			require.NoError(t, err)
			require.Contains(t, unprepared, claim.UID)
			require.NoError(t, unprepared[claim.UID])
		})
	})
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
				for _, n := range names {
					uid := types.UID(n + "-uid")
					require.ErrorContains(t, prepared[uid].Err, "no allocation set")
					require.NoError(t, unprepared[uid])
				}

				require.Less(t, time.Since(start), RetryMaxTimeout, "the requests waited out the timeout")
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
