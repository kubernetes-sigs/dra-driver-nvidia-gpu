/*
 * Copyright (c) 2025 NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package flock

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"
)

type Flock struct {
	path string
}

type AcquireOption func(*acquireConfig)

type acquireConfig struct {
	timeout    time.Duration
	pollPeriod time.Duration
}

func WithTimeout(timeout time.Duration) AcquireOption {
	return func(cfg *acquireConfig) {
		cfg.timeout = timeout
	}
}

func WithPollPeriod(period time.Duration) AcquireOption {
	return func(cfg *acquireConfig) {
		cfg.pollPeriod = period
	}
}

func NewFlock(path string) *Flock {
	return &Flock{
		path: path,
	}
}

// Acquire attempts to acquire an exclusive file lock, polling with the configured
// PollPeriod until whichever comes first:
//
// - lock successfully acquired
// - timeout (if provided)
// - external cancellation of context
//
// Returns a release function that must be called to unlock the file, typically
// with defer().
//
// Introduced to protect the work in nodePrepareResource() and
// nodeUnprepareResource() under a file-based lock because more than one driver
// pod may be running on a node, but at most one such function must execute at
// any given time.
func (l *Flock) Acquire(ctx context.Context, opts ...AcquireOption) (func(), error) {
	cfg := &acquireConfig{
		// Default: short period to keep lock acquisition rather responsive. If
		// this is used for fine-grained (and hence frequent) checkpoint
		// mutation locking, then it should really be a rather short period
		// (~100 ms or less).
		pollPeriod: 100 * time.Millisecond,
		// Default: timeout disabled
		timeout: 0,
	}
	for _, opt := range opts {
		opt(cfg)
	}

	f, oerr := os.OpenFile(l.path, os.O_RDWR|os.O_CREATE, 0644)
	if oerr != nil {
		return nil, fmt.Errorf("error opening lock file (%s): %w", l.path, oerr)
	}

	t0 := time.Now()
	ticker := time.NewTicker(cfg.pollPeriod)
	defer ticker.Stop()

	// Use non-blocking peek with LOCK_NB flag and polling; trade-off:
	//
	// - pro: no need for having to reliably cancel a potentially long-blocking
	//   flock() system call (can only be done with signals).
	// - con: lock acquisition time after a release is not immediate, but may
	//   take up to PollPeriod amount of time. Not an issue in this context.
	for {
		// Honor cancellation before every attempt, so that a caller whose
		// context is already done never acquires the lock (and never starts
		// the work the lock protects). Checking here, rather than only in the
		// select below, also covers the case where ctx.Done() and the ticker
		// are ready at the same time and select picks the ticker.
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, fmt.Errorf("error acquiring lock (%s): %w", l.path, err)
		}

		flerr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if flerr == nil {
			// Lock acquired. Return release function. An exclusive flock() lock
			// gets released when its file descriptor gets closed (also true
			// when the lock-holding process crashes).
			release := func() {
				f.Close()
			}
			return release, nil
		}

		if flerr != syscall.EWOULDBLOCK {
			// May be EBADF, EINTR, EINVAl, ENOLCK, and
			// in general we want an outer retry mechanism
			// to retry in view of any of those.
			f.Close()
			return nil, fmt.Errorf("error acquiring lock (%s): %w", l.path, flerr)
		}

		// Lock is currently held by other entity. Check for exit criteria;
		// otherwise retry lock acquisition upon next tick.

		if cfg.timeout > 0 && time.Since(t0) > cfg.timeout {
			f.Close()
			return nil, fmt.Errorf("timeout acquiring lock (%s)", l.path)
		}

		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("error acquiring lock (%s): %w", l.path, ctx.Err())
		case <-ticker.C:
			// Retry flock().
			continue
		}
	}
}
