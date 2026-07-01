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

package nvfm

type fabricManager struct {
	handle *nvfmHandle
}

// fm.Disconnect()
func (fm *fabricManager) Disconnect() Return {
	return fmDisconnectFunc(fm.handle)
}

// fm.GetSupportedFabricPartitions()
func (fm *fabricManager) GetSupportedFabricPartitions() (FabricPartitionList, Return) {
	var partitions FabricPartitionList
	partitions.Version = FabricPartitionListVersion
	ret := fmGetSupportedFabricPartitionsFunc(fm.handle, &partitions)
	return partitions, ret
}

// fm.GetUnsupportedFabricPartitions()
func (fm *fabricManager) GetUnsupportedFabricPartitions() (UnsupportedFabricPartitionList, Return) {
	var partitions UnsupportedFabricPartitionList
	partitions.Version = UnsupportedFabricPartitionListVersion
	ret := fmGetUnsupportedFabricPartitionsFunc(fm.handle, &partitions)
	return partitions, ret
}

// fm.ActivateFabricPartition()
func (fm *fabricManager) ActivateFabricPartition(id FabricPartitionId) Return {
	return fmActivateFabricPartitionFunc(fm.handle, id)
}

// fm.ActivateFabricPartitionWithVFs()
func (fm *fabricManager) ActivateFabricPartitionWithVFs(id FabricPartitionId, vfs []PciDevice) Return {
	if len(vfs) > MAX_NUM_GPUS {
		return BADPARAM
	}
	if len(vfs) == 0 {
		return fmActivateFabricPartitionWithVFsFunc(fm.handle, id, nil, 0)
	}
	return fmActivateFabricPartitionWithVFsFunc(fm.handle, id, &vfs[0], uint32(len(vfs)))
}

// fm.DeactivateFabricPartition()
func (fm *fabricManager) DeactivateFabricPartition(id FabricPartitionId) Return {
	return fmDeactivateFabricPartitionFunc(fm.handle, id)
}

// fm.SetActivatedFabricPartitions()
func (fm *fabricManager) SetActivatedFabricPartitions(ids []FabricPartitionId) Return {
	if len(ids) > MAX_FABRIC_PARTITIONS {
		return BADPARAM
	}

	list := ActivatedFabricPartitionList{
		Version:       ActivatedFabricPartitionListVersion,
		NumPartitions: uint32(len(ids)),
	}
	for i, id := range ids {
		list.PartitionIds[i] = uint32(id)
	}
	return fmSetActivatedFabricPartitionsFunc(fm.handle, &list)
}

// fm.GetNvlinkFailedDevices()
func (fm *fabricManager) GetNvlinkFailedDevices() (NvlinkFailedDevices, Return) {
	var devices NvlinkFailedDevices
	devices.Version = NvlinkFailedDevicesVersion
	ret := fmGetNvlinkFailedDevicesFunc(fm.handle, &devices)
	return devices, ret
}
