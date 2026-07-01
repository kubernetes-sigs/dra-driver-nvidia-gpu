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

// fm.LibInit()
func (l *library) Init() Return {
	l.Lock()
	defer l.Unlock()

	if l.refcount > 0 {
		l.refcount++
		return SUCCESS
	}

	if err := l.dl.Open(); err != nil {
		return GENERIC_ERROR
	}

	ret := fmLibInitFunc()
	if ret != SUCCESS {
		_ = l.dl.Close()
		return ret
	}

	l.refcount = 1
	return SUCCESS
}

// fm.LibShutdown()
func (l *library) Shutdown() Return {
	l.Lock()
	defer l.Unlock()

	if l.refcount == 0 {
		return UNINITIALIZED
	}
	if l.refcount > 1 {
		l.refcount--
		return SUCCESS
	}

	ret := fmLibShutdownFunc()
	if ret != SUCCESS {
		return ret
	}

	if err := l.dl.Close(); err != nil {
		return GENERIC_ERROR
	}

	l.refcount = 0
	return SUCCESS
}
