/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

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
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"k8s.io/klog/v2"
)

type ProcessManager struct {
	sync.Mutex
	handle      *exec.Cmd
	cmd         []string
	waitResChan chan error
}

func NewProcessManager(cmd []string) *ProcessManager {
	m := &ProcessManager{
		handle:      nil,
		cmd:         cmd,
		waitResChan: make(chan error, 1),
	}
	return m
}

// Restart starts or restarts the process.
func (m *ProcessManager) Restart() error {
	m.Lock()
	defer m.Unlock()

	if m.handle != nil {
		if err := m.stopLocked(); err != nil {
			return fmt.Errorf("restart: stop failed: %w", err)
		}
	}
	return m.startLocked()
}

// EnsureStarted starts the process if it is not already running. If the process
// is already started, this is a no-op. The boolean return value indicates
// `new`, i.e. it is `true` if the process was _newly_ started. It must be
// ignored when the returned error is non-nil.
func (m *ProcessManager) EnsureStarted() (bool, error) {
	m.Lock()
	defer m.Unlock()

	if m.handle != nil {
		return false, nil
	}
	return true, m.startLocked()
}

// Signal() attempts to send the provided signal to the managed child process.
// Any error is emitted to the caller and must be handled there.
func (m *ProcessManager) Signal(s os.Signal) error {
	m.Lock()
	defer m.Unlock()

	if m.handle == nil {
		return fmt.Errorf("pm: sending signal %s failed: not started", s)
	}
	return m.handle.Process.Signal(s)
}

// Start starts the process. Returns an error if the process is already running.
func (m *ProcessManager) Start() error {
	m.Lock()
	defer m.Unlock()
	return m.startLocked()
}

// Stop gracefully stops the running process by sending SIGTERM and waiting for
// it to exit. Returns an error if the process is not running.
func (m *ProcessManager) Stop() error {
	m.Lock()
	defer m.Unlock()
	return m.stopLocked()
}

// startLocked starts the process. The caller must hold m.Lock().
func (m *ProcessManager) startLocked() error {
	if m.handle != nil {
		return fmt.Errorf("pm: start failed: already started")
	}

	klog.Infof("Start: %s", strings.Join(m.cmd, " "))

	// Child inherits stdout/err: output of child will interleave with output of
	// parent. In practice, individual log lines typically stay intact (data
	// written in one write() syscall typically is written as an atomic unit).
	cmd := exec.Command(m.cmd[0], m.cmd[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Start()

	// For pre-start problems like invalid path or permission error.
	if err != nil {
		return fmt.Errorf("failed to start process: %w", err)
	}
	m.handle = cmd

	// Start blocking wait() system call reaping this process once it exits. The
	// result is written to a buffered channel (that can be peeked into; used to
	// detect unexpected child termination in Watchdog()). It's OK to give up
	// control over this goroutine; but it's critical that `m.waitLocked()` is
	// called at some point during each child's lifecycle to read from the channel.
	go func() {
		m.waitResChan <- m.handle.Wait()
	}()

	klog.Infof("Started process with pid %d", cmd.Process.Pid)
	return nil
}

// waitLocked waits for the Wait() syscall result to appear on the channel,
// logs the exit code, and resets m.handle. The caller must hold m.Lock().
// If the wait() system call that feeds the channel returns an error: fatal.
func (m *ProcessManager) waitLocked() error {
	werr := <-m.waitResChan
	if werr == nil {
		klog.Infof("Child exited with code 0")
	} else {
		if exitError, ok := werr.(*exec.ExitError); ok {
			klog.Warningf("Child exited with code %d", exitError.ExitCode())
		} else {
			return fmt.Errorf("pm: wait() failed: %w", werr)
		}
	}

	// Status reaped, we can drop reference to the child.
	m.handle = nil
	return nil
}

// stopLocked sends SIGTERM to the child and waits for it to exit. The caller
// must hold m.Lock().
func (m *ProcessManager) stopLocked() error {
	if m.handle == nil {
		return fmt.Errorf("pm: stop failed: not started")
	}

	klog.Infof("Stop: send SIGTERM to pid %d", m.handle.Process.Pid)
	err := m.handle.Process.Signal(syscall.SIGTERM)
	if err != nil {
		return fmt.Errorf("pm: stop: could not send SIGTERM to child: %w", err)
	}

	// Wait for process to gracefully shut down. TODO: apply timeout, send
	// SIGKILL upon timeout, wait again. Update: it's reasonable to leave this
	// to the k8s orchestration layer.
	klog.Infof("Wait() for child")
	if err := m.waitLocked(); err != nil {
		return fmt.Errorf("pm: stop: wait failed: %w", err)
	}

	return nil
}

// Watchdog() supervises the process: unexpected termination is handled by
// logging a corresponding message, and by restarting the process. Canceling the
// injected context is the intended way to gracefully stop the child (and to
// also terminate the watchdog).
func (m *ProcessManager) Watchdog(ctx context.Context) error {
	// Maybe use SIGCHLD handler instead to make this ticker-less
	ticker := time.NewTicker(1000 * time.Millisecond)
	defer ticker.Stop()

	klog.Infof("Start watchdog")
	for {
		select {
		case <-ctx.Done():
			m.Lock()
			klog.Infof("Watchdog: context canceled, attempt to stop child process")
			if err := m.stopLocked(); err != nil {
				m.Unlock()
				return fmt.Errorf("watchdog: stop failed: %w", err)
			}
			m.Unlock()
			return nil
		case <-ticker.C:
			if !m.lost() {
				continue
			}

			m.Lock()
			// Re-check state after acquiring the lock: another goroutine may
			// have already reaped the child or restarted it while we were
			// waiting for the lock.
			if m.handle == nil || len(m.waitResChan) == 0 {
				m.Unlock()
				continue
			}

			klog.Warningf("Watchdog: child terminated unexpectedly")
			// `m.waitLocked()` is known to not block at this point.
			if err := m.waitLocked(); err != nil {
				m.Unlock()
				return fmt.Errorf("watchdog: process lost, wait failed, treat fatal: %w", err)
			}

			klog.Warningf("Watchdog: start process again")
			if err := m.startLocked(); err != nil {
				m.Unlock()
				return fmt.Errorf("watchdog: process lost, restart failed, treat fatal: %w", err)
			}
			m.Unlock()
		}
	}
}

// Detect if process terminated unexpectedly.
func (m *ProcessManager) lost() bool {
	if !m.TryLock() {
		// Start or stop is in progress; do not inspect state.
		return false
	}
	defer m.Unlock()

	if m.handle == nil {
		// Not yet or not currently started.
		return false
	}

	if len(m.waitResChan) == 0 {
		// Currently running: background waitLocked() did not return.
		return false
	}

	return true
}
