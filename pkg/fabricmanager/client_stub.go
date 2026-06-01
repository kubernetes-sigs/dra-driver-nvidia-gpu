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

// stubClient is a placeholder Client. It exists so that this package compiles
// and is usable for unit tests / dry-runs without the cgo-backed FM SDK
// (libnvfm.so) on the build host.
//
// All partition query/activation methods return ErrUnimplemented;
// Init/Connect/Disconnect/Shutdown are no-ops so a caller's defer chain is
// harmless. Callers (e.g. Manager.Open) should treat ErrUnimplemented as "FM
// not available on this node" and skip publishing FM-derived attributes.
//
// The production backend is NewClient (client_nvfm.go), which wraps NVIDIA's
// go-nvfm bindings. From the Manager's perspective nothing changes: it always
// programs against the Client interface.
type stubClient struct{}

// NewStubClient returns a no-op FM client whose partition queries always fail
// with ErrUnimplemented.
func NewStubClient() Client {
	return &stubClient{}
}

func (*stubClient) Init() error                 { return nil }
func (*stubClient) Connect(ConnectParams) error { return nil }
func (*stubClient) Disconnect() error           { return nil }
func (*stubClient) Shutdown() error             { return nil }

func (*stubClient) GetSupportedFabricPartitions() ([]Partition, error) {
	return nil, ErrUnimplemented
}

func (*stubClient) GetUnsupportedFabricPartitions() ([]UnsupportedPartition, error) {
	return nil, ErrUnimplemented
}

func (*stubClient) ActivateFabricPartition(int) error   { return ErrUnimplemented }
func (*stubClient) DeactivateFabricPartition(int) error { return ErrUnimplemented }
