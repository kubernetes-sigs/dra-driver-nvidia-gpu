/*
 * Copyright (c) 2026 NVIDIA CORPORATION.  All rights reserved.
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

package fabricmanager

import (
	"fmt"
	"sync"

	"github.com/NVIDIA/go-nvfm/pkg/nvfm"
	"k8s.io/klog/v2"
)

// nvfmClient is a Client backed by NVIDIA's go-nvfm bindings.
type nvfmClient struct {
	lib nvfm.Interface

	mu            sync.Mutex
	handle        nvfm.Handle
	params        ConnectParams
	connectedOnce bool
}

// NewClient returns a client backed by go-nvfm.
func NewClient(libraryPath string) Client {
	var opts []nvfm.LibraryOption
	if libraryPath != "" {
		opts = append(opts, nvfm.WithLibraryPath(libraryPath))
	}
	return &nvfmClient{lib: nvfm.New(opts...)}
}

func (c *nvfmClient) Init() error {
	return toError(c.lib.Init(), "fmLibInit")
}

func (c *nvfmClient) Connect(params ConnectParams) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Remember the connect parameters so a later reconnect can reproduce the
	// original transport/address/timeout.
	c.params = params
	c.connectedOnce = true
	return c.connectLocked()
}

// connectOptions translates the package-level ConnectParams into go-nvfm
// connect options.
func connectOptions(params ConnectParams) []nvfm.ConnectOption {
	opts := []nvfm.ConnectOption{}
	if params.AddressInfo != "" {
		if params.AddressIsUnixSocket {
			opts = append(opts, nvfm.WithUnixSocket(params.AddressInfo))
		} else {
			opts = append(opts, nvfm.WithAddress(params.AddressInfo))
		}
	}
	if params.TimeoutMs != 0 {
		opts = append(opts, nvfm.WithTimeoutMs(params.TimeoutMs))
	}
	return opts
}

// connectLocked establishes a fresh connection using the stored params. The
// caller must hold c.mu.
func (c *nvfmClient) connectLocked() error {
	handle, ret := c.lib.Connect(connectOptions(c.params)...)
	if err := toError(ret, "fmConnect"); err != nil {
		return err
	}
	c.handle = handle
	return nil
}

// reconnectLocked tears down any stale handle and re-establishes the
// connection. The caller must hold c.mu.
func (c *nvfmClient) reconnectLocked() error {
	if c.handle != nil {
		// Best-effort teardown; the connection is presumed already broken so
		// the error is not actionable.
		_ = c.handle.Disconnect()
		c.handle = nil
	}
	return c.connectLocked()
}

func (c *nvfmClient) Disconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handle == nil {
		return nil
	}
	err := toError(c.handle.Disconnect(), "fmDisconnect")
	c.handle = nil
	return err
}

func (c *nvfmClient) Shutdown() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return toError(c.lib.Shutdown(), "fmLibShutdown")
}

func (c *nvfmClient) GetSupportedFabricPartitions() ([]Partition, error) {
	var list nvfm.FabricPartitionList
	err := c.doWithReconnect("fmGetSupportedFabricPartitions", func(h nvfm.Handle) nvfm.Return {
		var ret nvfm.Return
		list, ret = h.GetSupportedFabricPartitions()
		return ret
	})
	if err != nil {
		return nil, err
	}
	return toPartitions(list), nil
}

func (c *nvfmClient) ActivateFabricPartition(partitionID int) error {
	return c.doWithReconnect("fmActivateFabricPartition", func(h nvfm.Handle) nvfm.Return {
		return h.ActivateFabricPartition(nvfm.FabricPartitionId(partitionID))
	})
}

func (c *nvfmClient) DeactivateFabricPartition(partitionID int) error {
	return c.doWithReconnect("fmDeactivateFabricPartition", func(h nvfm.Handle) nvfm.Return {
		return h.DeactivateFabricPartition(nvfm.FabricPartitionId(partitionID))
	})
}

// doWithReconnect runs fn against the live FM handle while holding the
// connection lock. If fn reports that the connection is no longer valid (e.g.
// nv-fabricmanager restarted), it reconnects once and retries fn a single time.
func (c *nvfmClient) doWithReconnect(op string, fn func(nvfm.Handle) nvfm.Return) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connectedOnce {
		return fmt.Errorf("fabricmanager: not connected (call Connect first)")
	}
	if c.handle == nil {
		if err := c.reconnectLocked(); err != nil {
			return fmt.Errorf("%s: reconnect failed: %w", op, err)
		}
	}

	ret := fn(c.handle)
	if ret != nvfm.CONNECTION_NOT_VALID && ret != nvfm.UNINITIALIZED {
		return toError(ret, op)
	}

	klog.Warningf("fabricmanager: %s returned %s; reconnecting to nv-fabricmanager and retrying once", op, ret)
	if err := c.reconnectLocked(); err != nil {
		return fmt.Errorf("%s: reconnect after %s failed: %w", op, ret, err)
	}
	return toError(fn(c.handle), op)
}

// toError converts an nvfm.Return into an error, returning nil on SUCCESS.
func toError(ret nvfm.Return, op string) error {
	if ret == nvfm.SUCCESS {
		return nil
	}
	return fmt.Errorf("%s: %s", op, ret)
}

// clamp returns the smaller of n (a count reported by FM) and the static
// length of the backing array, guarding against malformed counts.
func clamp(n uint32, max int) int {
	if int(n) > max {
		return max
	}
	return int(n)
}

func toPartitions(list nvfm.FabricPartitionList) []Partition {
	count := clamp(list.NumPartitions, len(list.PartitionInfo))
	out := make([]Partition, 0, count)
	for i := 0; i < count; i++ {
		p := list.PartitionInfo[i]
		numGPUs := clamp(p.NumGpus, len(p.GpuInfo))
		gpus := make([]PartitionGPU, 0, numGPUs)
		for j := 0; j < numGPUs; j++ {
			g := p.GpuInfo[j]
			gpus = append(gpus, PartitionGPU{
				PhysicalID:          int(g.PhysicalId),
				UUID:                int8ToString(g.Uuid[:]),
				PCIBusID:            int8ToString(g.PciBusId[:]),
				NumNvLinksAvailable: g.NumNvLinksAvailable,
				MaxNumNvLinks:       g.MaxNumNvLinks,
				NvLinkLineRateMBps:  g.NvlinkLineRateMBps,
			})
		}
		out = append(out, Partition{
			ID:       int(p.PartitionId),
			IsActive: p.IsActive != 0,
			GPUs:     gpus,
		})
	}
	return out
}

// int8ToString converts a NUL-terminated C char array (represented as []int8
// by cgo) into a Go string.
func int8ToString(b []int8) string {
	buf := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		buf = append(buf, byte(c))
	}
	return string(buf)
}
