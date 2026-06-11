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

import "errors"

var ErrUnimplemented = errors.New("fabricmanager: client backend not implemented")

type ConnectParams struct {
	// AddressInfo is "<host>:<port>" for TCP, or a filesystem path when
	// AddressIsUnixSocket is true. Empty means use the FM SDK default.
	AddressInfo string
	// TimeoutMs is the connect timeout, in milliseconds. Zero uses the SDK
	// default.
	TimeoutMs uint32
	// AddressIsUnixSocket selects the unix-socket transport.
	AddressIsUnixSocket bool
}

// PartitionGPU describes a single GPU as reported by FM inside a fabric
// partition. The fields correspond to fmFabricPartitionGpuInfo_t.

type PartitionGPU struct {
	// PhysicalID is the GPU's physical/module ID
	PhysicalID          int
	UUID                string
	PCIBusID            string
	NumNvLinksAvailable uint32
	MaxNumNvLinks       uint32
	NvLinkLineRateMBps  uint32
}

// Partition is a single FM-supported fabric partition
// Corresponds to fmFabricPartitionInfo_t.
type Partition struct {
	ID       int
	IsActive bool
	GPUs     []PartitionGPU
}

// GPUModuleIDs returns the PhysicalIDs/ModuleIDs of all
// GPUs in the partition, in the order FM reported them.
func (p Partition) GPUModuleIDs() []int {
	ids := make([]int, len(p.GPUs))
	for i, g := range p.GPUs {
		ids[i] = g.PhysicalID
	}
	return ids
}

// UnsupportedPartition describes an FM-rejected partition
// Mirrors fmUnsupportedFabricPartitionInfo_t.
type UnsupportedPartition struct {
	ID             int
	GPUPhysicalIDs []int
}

// Client is a Go projection of the NVIDIA Fabric Manager C SDK
// (libnvidia-fabricmanager.so)
type Client interface {
	Init() error

	Connect(params ConnectParams) error

	// GetSupportedFabricPartitions returns every partition FM supports on this node, including each partition's GPU members.
	GetSupportedFabricPartitions() ([]Partition, error)

	// GetUnsupportedFabricPartitions returns partitions FM knows about but has marked unsupported (e.g. due to NVLink failures).
	GetUnsupportedFabricPartitions() ([]UnsupportedPartition, error)

	// ActivateFabricPartition asks FM to program the NVSwitch fabric for the given partition. Used as part of DRA allocation for GPU passthrough.
	ActivateFabricPartition(partitionID int) error

	// DeactivateFabricPartition releases the NVSwitch fabric programming
	// for the given partition.
	DeactivateFabricPartition(partitionID int) error

	// Disconnect closes the connection to nv-fabricmanager (fmDisconnect).
	Disconnect() error

	// Shutdown unloads/shuts down the FM library (fmLibShutdown).
	Shutdown() error
}
