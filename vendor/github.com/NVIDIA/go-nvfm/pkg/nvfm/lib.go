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

import (
	"errors"
	"fmt"
	"sync"

	"github.com/NVIDIA/go-nvfm/pkg/dl"
)

const (
	defaultNvfmLibraryName      = "libnvfm.so"
	defaultNvfmLibraryLoadFlags = dl.RTLD_LAZY | dl.RTLD_GLOBAL
)

var errLibraryNotLoaded = errors.New("library not loaded")
var errLibraryAlreadyLoaded = errors.New("library already loaded")

type dynamicLibrary interface {
	Lookup(string) error
	Open() error
	Close() error
}

type library struct {
	sync.Mutex
	path     string
	refcount refcount
	dl       dynamicLibrary
}

var _ Interface = (*library)(nil)

var libnvfm = newLibrary()

func New(opts ...LibraryOption) Interface {
	return newLibrary(opts...)
}

func newLibrary(opts ...LibraryOption) *library {
	l := &library{}
	l.init(opts...)
	return l
}

func (l *library) init(opts ...LibraryOption) {
	o := libraryOptions{}
	for _, opt := range opts {
		opt(&o)
	}

	if o.path == "" {
		o.path = defaultNvfmLibraryName
	}
	if o.flags == 0 {
		o.flags = defaultNvfmLibraryLoadFlags
	}

	l.path = o.path
	l.dl = dl.New(o.path, o.flags)
}

func (l *library) Extensions() ExtendedInterface {
	return l
}

// LookupSymbol checks whether a symbol exists in the loaded Fabric Manager library.
func (l *library) LookupSymbol(name string) error {
	if l == nil || l.refcount == 0 {
		return fmt.Errorf("error looking up %s: %w", name, errLibraryNotLoaded)
	}
	return l.dl.Lookup(name)
}
