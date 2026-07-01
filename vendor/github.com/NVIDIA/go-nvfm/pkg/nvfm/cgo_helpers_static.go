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

import "C"

var cgoAllocsUnknown = new(struct{})

func clen(n []int8) int {
	for i := 0; i < len(n); i++ {
		if n[i] == 0 {
			return i
		}
	}
	return len(n)
}

func int8ArrayString(n []int8) string {
	b := make([]byte, clen(n))
	for i := range b {
		b[i] = byte(n[i])
	}
	return string(b)
}

func setInt8ArrayString(dst []int8, src string) {
	for i := range dst {
		dst[i] = 0
	}
	if len(dst) == 0 {
		return
	}
	limit := len(dst) - 1
	if len(src) < limit {
		limit = len(src)
	}
	for i := 0; i < limit; i++ {
		dst[i] = int8(src[i])
	}
}
