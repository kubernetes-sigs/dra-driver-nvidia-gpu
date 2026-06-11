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

	"github.com/NVIDIA/go-nvfm/pkg/nvfm"
)

// nvfmClient is a Client backed by NVIDIA's go-nvfm bindings, which wrap the
// Fabric Manager C SDK (libnvfm.so / libnvidia-fabricmanager.so) loaded at
// runtime via dlopen.
type nvfmClient struct {
	lib    nvfm.Interface
	handle nvfm.Handle
}

// NewClient returns a Client backed by go-nvfm. libraryPath optionally points
// at a specific libnvfm.so; an empty string uses the default library name and
// relies on the dynamic loader's search path.
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

	handle, ret := c.lib.Connect(opts...)
	if err := toError(ret, "fmConnect"); err != nil {
		return err
	}
	c.handle = handle
	return nil
}

func (c *nvfmClient) Disconnect() error {
	if c.handle == nil {
		return nil
	}
	err := toError(c.handle.Disconnect(), "fmDisconnect")
	c.handle = nil
	return err
}

func (c *nvfmClient) Shutdown() error {
	return toError(c.lib.Shutdown(), "fmLibShutdown")
}

func (c *nvfmClient) GetSupportedFabricPartitions() ([]Partition, error) {
	if c.handle == nil {
		return nil, errNotConnected
	}
	list, ret := c.handle.GetSupportedFabricPartitions()
	if err := toError(ret, "fmGetSupportedFabricPartitions"); err != nil {
		return nil, err
	}
	return toPartitions(list), nil
}

func (c *nvfmClient) GetUnsupportedFabricPartitions() ([]UnsupportedPartition, error) {
	if c.handle == nil {
		return nil, errNotConnected
	}
	list, ret := c.handle.GetUnsupportedFabricPartitions()
	if err := toError(ret, "fmGetUnsupportedFabricPartitions"); err != nil {
		return nil, err
	}
	return toUnsupportedPartitions(list), nil
}

func (c *nvfmClient) ActivateFabricPartition(partitionID int) error {
	if c.handle == nil {
		return errNotConnected
	}
	return toError(
		c.handle.ActivateFabricPartition(nvfm.FabricPartitionId(partitionID)),
		"fmActivateFabricPartition",
	)
}

func (c *nvfmClient) DeactivateFabricPartition(partitionID int) error {
	if c.handle == nil {
		return errNotConnected
	}
	return toError(
		c.handle.DeactivateFabricPartition(nvfm.FabricPartitionId(partitionID)),
		"fmDeactivateFabricPartition",
	)
}

var errNotConnected = fmt.Errorf("fabricmanager: not connected (call Connect first)")

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

func toUnsupportedPartitions(list nvfm.UnsupportedFabricPartitionList) []UnsupportedPartition {
	count := clamp(list.NumPartitions, len(list.PartitionInfo))
	out := make([]UnsupportedPartition, 0, count)
	for i := 0; i < count; i++ {
		p := list.PartitionInfo[i]
		numGPUs := clamp(p.NumGpus, len(p.GpuPhysicalIds))
		ids := make([]int, 0, numGPUs)
		for j := 0; j < numGPUs; j++ {
			ids = append(ids, int(p.GpuPhysicalIds[j]))
		}
		out = append(out, UnsupportedPartition{
			ID:             int(p.PartitionId),
			GPUPhysicalIDs: ids,
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
