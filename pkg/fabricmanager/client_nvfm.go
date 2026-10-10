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

package fabricmanager

import (
	"fmt"

	"github.com/NVIDIA/go-nvfm/pkg/nvfm"
	"k8s.io/klog/v2"
)

// nvfmClient is a Client backed by NVIDIA's go-nvfm bindings.
//
// It is not internally synchronized: the GPU kubelet plugin guarantees that at
// most one Prepare or Unprepare executes at a time (a node-wide exclusive file
// lock plus an in-process DeviceState mutex), and all runtime FM operations
// (Activate/Deactivate) are reached only from those serialized paths.
type nvfmClient struct {
	lib    nvfm.Interface
	params ConnectParams
}

// NewClient returns a client backed by go-nvfm.
func NewClient(libraryPath string, params ConnectParams) Client {
	var opts []nvfm.LibraryOption
	if libraryPath != "" {
		opts = append(opts, nvfm.WithLibraryPath(libraryPath))
	}
	return &nvfmClient{lib: nvfm.New(opts...), params: params}
}

func (c *nvfmClient) Init() error {
	return toError(c.lib.Init(), "fmLibInit")
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

func (c *nvfmClient) Shutdown() error {
	return toError(c.lib.Shutdown(), "fmLibShutdown")
}

func (c *nvfmClient) GetSupportedFabricPartitions() ([]Partition, error) {
	var list nvfm.FabricPartitionList
	err := c.withConnection("fmGetSupportedFabricPartitions", func(h nvfm.Handle) nvfm.Return {
		var ret nvfm.Return
		list, ret = h.GetSupportedFabricPartitions()
		return ret
	})
	if err != nil {
		return nil, err
	}
	return toPartitions(list)
}

func (c *nvfmClient) ActivateFabricPartition(partitionID int) error {
	return c.withConnection("fmActivateFabricPartition", func(h nvfm.Handle) nvfm.Return {
		return h.ActivateFabricPartition(nvfm.FabricPartitionId(partitionID))
	})
}

func (c *nvfmClient) DeactivateFabricPartition(partitionID int) error {
	return c.withConnection("fmDeactivateFabricPartition", func(h nvfm.Handle) nvfm.Return {
		return h.DeactivateFabricPartition(nvfm.FabricPartitionId(partitionID))
	})
}

// withConnection opens a brand new connection to nv-fabricmanager, runs fn
// against it, and disconnects before returning.
func (c *nvfmClient) withConnection(op string, fn func(nvfm.Handle) nvfm.Return) error {
	handle, ret := c.lib.Connect(connectOptions(c.params)...)
	if err := toError(ret, "fmConnect"); err != nil {
		return fmt.Errorf("%s: connect failed: %w", op, err)
	}
	defer func() {
		if err := toError(handle.Disconnect(), "fmDisconnect"); err != nil {
			klog.Warningf("fabricmanager: %s: %v", op, err)
		}
	}()

	return toError(fn(handle), op)
}

// toError converts an nvfm.Return into an error, returning nil on SUCCESS.
func toError(ret nvfm.Return, op string) error {
	if ret == nvfm.SUCCESS {
		return nil
	}
	return fmt.Errorf("%s: %s", op, ret)
}

// checkCount validates a count reported by FM against the static length of the
// backing array. A count that exceeds the array capacity means the runtime
// libnvfm and the vendored headers disagree (an ABI mismatch) or FM returned a
// malformed count. Silently clamping such a count would record a truncated,
// incomplete partition that FindPartitionByPhysicalIDs could then match against a
// requested GPU subset and activate a partition containing additional GPUs, so
// we fail loudly instead.
func checkCount(n uint32, max int) (int, error) {
	if int(n) > max {
		return 0, fmt.Errorf("FM reported %d but the vendored array only holds %d; "+
			"the runtime libnvfm likely disagrees with the vendored headers", n, max)
	}
	return int(n), nil
}

func toPartitions(list nvfm.FabricPartitionList) ([]Partition, error) {
	count, err := checkCount(list.NumPartitions, len(list.PartitionInfo))
	if err != nil {
		return nil, fmt.Errorf("invalid partition count: %w", err)
	}
	out := make([]Partition, 0, count)
	for i := 0; i < count; i++ {
		p := list.PartitionInfo[i]
		numGPUs, err := checkCount(p.NumGpus, len(p.GpuInfo))
		if err != nil {
			return nil, fmt.Errorf("invalid GPU count for partition %d: %w", p.PartitionId, err)
		}
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
	return out, nil
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
