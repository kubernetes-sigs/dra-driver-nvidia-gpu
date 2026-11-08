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

package flock

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testPollPeriod keeps lock-contention tests fast; the production default is 100ms.
const testPollPeriod = 20 * time.Millisecond

// lockPath returns a lock-file path inside a per-test temp dir,
// which Go deletes automatically when the test ends.
func lockPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.lock")
}

// mustAcquire acquires the lock at path or fails the test.
func mustAcquire(t *testing.T, path string) func() {
	t.Helper()
	release, err := NewFlock(path).Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() failed: %v", err)
	}
	return release
}

// Acquire on a free lock succeeds; after release, it can be acquired again.
func TestAcquireRelease(t *testing.T) {
	l := NewFlock(lockPath(t))

	release, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first Acquire() failed: %v", err)
	}
	release()

	release2, err := l.Acquire(context.Background(), WithTimeout(time.Second))
	if err != nil {
		t.Fatalf("Acquire() after release failed: %v", err)
	}
	release2()
}

// While the lock is held, a second Acquire with a timeout gives up with a
// timeout error, and not before the timeout has elapsed.
func TestAcquireTimeout(t *testing.T) {
	path := lockPath(t)
	release := mustAcquire(t, path)
	defer release()

	timeout := 300 * time.Millisecond
	start := time.Now()
	_, err := NewFlock(path).Acquire(context.Background(),
		WithTimeout(timeout), WithPollPeriod(testPollPeriod))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Acquire() on a held lock succeeded, want timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("Acquire() error = %v, want timeout error", err)
	}
	if elapsed < timeout {
		t.Fatalf("Acquire() returned after %v, want at least %v", elapsed, timeout)
	}
}

// While the lock is held, a second Acquire returns once its context is done,
// and the returned error wraps the context error.
func TestAcquireContextCanceled(t *testing.T) {
	path := lockPath(t)
	release := mustAcquire(t, path)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := NewFlock(path).Acquire(ctx, WithPollPeriod(testPollPeriod))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire() error = %v, want wrapped %v", err, context.DeadlineExceeded)
	}
}

// A waiter blocked on a held lock acquires it once the holder releases.
func TestAcquireSucceedsAfterRelease(t *testing.T) {
	path := lockPath(t)
	release := mustAcquire(t, path)

	done := make(chan error, 1)
	go func() {
		r, err := NewFlock(path).Acquire(context.Background(),
			WithTimeout(2*time.Second), WithPollPeriod(testPollPeriod))
		if err == nil {
			r()
		}
		done <- err
	}()

	// Give the waiter time to observe the held lock before releasing it.
	time.Sleep(100 * time.Millisecond)
	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waiter Acquire() failed after release: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiter did not acquire the lock after release")
	}
}

// Acquire fails if the lock file cannot be opened.
func TestAcquireOpenError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist", "test.lock")

	_, err := NewFlock(path).Acquire(context.Background())
	if err == nil || !strings.Contains(err.Error(), "opening lock file") {
		t.Fatalf("Acquire() error = %v, want error opening lock file", err)
	}
}

// Documents current behavior: the lock is attempted before the context is
// checked, so a free lock is acquired even if the context is already done.
func TestAcquireFreeLockWithCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	release, err := NewFlock(lockPath(t)).Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire() on free lock with canceled context failed: %v", err)
	}
	release()
}
