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

const (
	defaultAddress   = "127.0.0.1"
	defaultTimeoutMs = 1000
)

type connectOptions struct {
	address  string
	timeout  uint32
	addrType uint32
	unix     bool
}

type ConnectOption func(*connectOptions)

func WithAddress(address string) ConnectOption {
	return func(o *connectOptions) {
		o.address = address
		o.addrType = ADDR_TYPE_INET
		o.unix = false
	}
}

func WithUnixSocket(path string) ConnectOption {
	return func(o *connectOptions) {
		o.address = path
		o.addrType = ADDR_TYPE_UNIX
		o.unix = true
	}
}

func WithTimeoutMs(timeoutMs uint32) ConnectOption {
	return func(o *connectOptions) {
		o.timeout = timeoutMs
	}
}

// fm.Connect()
func (l *library) Connect(opts ...ConnectOption) (Handle, Return) {
	params := newConnectParams(opts...)
	return l.ConnectWithParams(params)
}

// fm.ConnectWithParams()
func (l *library) ConnectWithParams(params ConnectParams) (Handle, Return) {
	var handle *nvfmHandle
	ret := fmConnectFunc(&params, &handle)
	if ret != SUCCESS {
		return nil, ret
	}
	if handle == nil {
		return nil, GENERIC_ERROR
	}
	return &fabricManager{handle: handle}, SUCCESS
}

func newConnectParams(opts ...ConnectOption) ConnectParams {
	options := connectOptions{
		address:  defaultAddress,
		timeout:  defaultTimeoutMs,
		addrType: ADDR_TYPE_INET,
	}
	for _, opt := range opts {
		opt(&options)
	}

	params := ConnectParams{
		Version:             ConnectParamsVersion,
		TimeoutMs:           options.timeout,
		AddressIsUnixSocket: 0,
		AddressType:         options.addrType,
	}
	if options.unix {
		params.AddressIsUnixSocket = 1
	}
	setInt8ArrayString(params.AddressInfo[:], options.address)
	return params
}
