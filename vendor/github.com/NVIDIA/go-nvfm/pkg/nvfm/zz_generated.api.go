// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Generated Code; DO NOT EDIT.

package nvfm

// The variables below represent package-level methods from the library type.
var (
	Connect           = libnvfm.Connect
	ConnectWithParams = libnvfm.ConnectWithParams
	ErrorString       = libnvfm.ErrorString
	Extensions        = libnvfm.Extensions
	Init              = libnvfm.Init
	Shutdown          = libnvfm.Shutdown
)

// Interface represents the interface for the library type.
type Interface interface {
	Connect(opts ...ConnectOption) (Handle, Return)
	ConnectWithParams(params ConnectParams) (Handle, Return)
	ErrorString(r Return) string
	Extensions() ExtendedInterface
	Init() Return
	Shutdown() Return
}

// Handle represents the interface for the fabricManager type.
type Handle interface {
	ActivateFabricPartition(id FabricPartitionId) Return
	ActivateFabricPartitionWithVFs(id FabricPartitionId, vfs []PciDevice) Return
	DeactivateFabricPartition(id FabricPartitionId) Return
	Disconnect() Return
	GetNvlinkFailedDevices() (NvlinkFailedDevices, Return)
	GetSupportedFabricPartitions() (FabricPartitionList, Return)
	GetUnsupportedFabricPartitions() (UnsupportedFabricPartitionList, Return)
	SetActivatedFabricPartitions(ids []FabricPartitionId) Return
}
