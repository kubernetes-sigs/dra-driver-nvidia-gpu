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

import "fmt"

// fm.ErrorString()
func (l *library) ErrorString(r Return) string {
	return r.Error()
}

func (r Return) String() string {
	return r.Error()
}

func (r Return) Error() string {
	switch r {
	case SUCCESS:
		return "SUCCESS"
	case BADPARAM:
		return "BADPARAM"
	case GENERIC_ERROR:
		return "GENERIC_ERROR"
	case NOT_SUPPORTED:
		return "NOT_SUPPORTED"
	case UNINITIALIZED:
		return "UNINITIALIZED"
	case TIMEOUT:
		return "TIMEOUT"
	case VERSION_MISMATCH:
		return "VERSION_MISMATCH"
	case IN_USE:
		return "IN_USE"
	case NOT_CONFIGURED:
		return "NOT_CONFIGURED"
	case CONNECTION_NOT_VALID:
		return "CONNECTION_NOT_VALID"
	case NVLINK_ERROR:
		return "NVLINK_ERROR"
	case RESOURCE_BAD:
		return "RESOURCE_BAD"
	case RESOURCE_IN_USE:
		return "RESOURCE_IN_USE"
	case RESOURCE_NOT_IN_USE:
		return "RESOURCE_NOT_IN_USE"
	case RESOURCE_EXHAUSTED:
		return "RESOURCE_EXHAUSTED"
	case RESOURCE_NOT_READY:
		return "RESOURCE_NOT_READY"
	case PARTITION_EXISTS:
		return "PARTITION_EXISTS"
	case PARTITION_ID_IN_USE:
		return "PARTITION_ID_IN_USE"
	case PARTITION_ID_NOT_IN_USE:
		return "PARTITION_ID_NOT_IN_USE"
	case PARTITION_NAME_IN_USE:
		return "PARTITION_NAME_IN_USE"
	case PARTITION_NAME_NOT_IN_USE:
		return "PARTITION_NAME_NOT_IN_USE"
	case PARTITION_ID_NAME_MISMATCH:
		return "PARTITION_ID_NAME_MISMATCH"
	case NOT_READY:
		return "NOT_READY"
	case RESOURCE_USED_IN_THIS_PARTITION:
		return "RESOURCE_USED_IN_THIS_PARTITION"
	case RESOURCE_USED_IN_ANOTHER_PARTITION:
		return "RESOURCE_USED_IN_ANOTHER_PARTITION"
	case PARTITION_MISWIRED_TRUNKS:
		return "PARTITION_MISWIRED_TRUNKS"
	case PARTITION_INSUFFICIENT_TRUNKS:
		return "PARTITION_INSUFFICIENT_TRUNKS"
	case PARTITION_MISSING_SWITCHES:
		return "PARTITION_MISSING_SWITCHES"
	case PARTITION_NETWORK_CONFIG_ERROR:
		return "PARTITION_NETWORK_CONFIG_ERROR"
	case PARTITION_ROUTE_PROGRAMMING_ERROR:
		return "PARTITION_ROUTE_PROGRAMMING_ERROR"
	default:
		return fmt.Sprintf("unknown return value: %d", r)
	}
}
