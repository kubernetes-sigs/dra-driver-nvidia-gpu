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

import "reflect"

func STRUCT_VERSION(data interface{}, version uint32) uint32 {
	return uint32(reflect.Indirect(reflect.ValueOf(data)).Type().Size()) | (version << 24)
}

var (
	ConnectV1ParamsVersion                = STRUCT_VERSION(ConnectParams_v1{}, 1)
	ConnectV2ParamsVersion                = STRUCT_VERSION(ConnectParams_v2{}, 2)
	ConnectParamsVersion                  = STRUCT_VERSION(ConnectParams{}, 2)
	FabricPartitionListVersion            = STRUCT_VERSION(FabricPartitionList_v2{}, 1)
	ActivatedFabricPartitionListVersion   = STRUCT_VERSION(ActivatedFabricPartitionList_v1{}, 1)
	NvlinkFailedDevicesVersion            = STRUCT_VERSION(NvlinkFailedDevices_v1{}, 1)
	UnsupportedFabricPartitionListVersion = STRUCT_VERSION(UnsupportedFabricPartitionList_v1{}, 1)
)
