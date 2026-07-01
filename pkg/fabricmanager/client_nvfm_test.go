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

import (
	"testing"

	"github.com/NVIDIA/go-nvfm/pkg/nvfm"
)

// fakeHandle is a stand-in for nvfm.Handle representing a single per-call
// connection. activateRet/deactivateRet are the nvfm.Return values the handle
// reports for its (at most one) Activate/Deactivate call.
type fakeHandle struct {
	disconnected int

	activateRet   nvfm.Return
	deactivateRet nvfm.Return

	activateIDs   []int
	deactivateIDs []int
}

func (h *fakeHandle) ActivateFabricPartition(id nvfm.FabricPartitionId) nvfm.Return {
	h.activateIDs = append(h.activateIDs, int(id))
	return h.activateRet
}

func (h *fakeHandle) DeactivateFabricPartition(id nvfm.FabricPartitionId) nvfm.Return {
	h.deactivateIDs = append(h.deactivateIDs, int(id))
	return h.deactivateRet
}

func (h *fakeHandle) Disconnect() nvfm.Return {
	h.disconnected++
	return nvfm.SUCCESS
}

func (h *fakeHandle) ActivateFabricPartitionWithVFs(nvfm.FabricPartitionId, []nvfm.PciDevice) nvfm.Return {
	return nvfm.SUCCESS
}

func (h *fakeHandle) GetSupportedFabricPartitions() (nvfm.FabricPartitionList, nvfm.Return) {
	return nvfm.FabricPartitionList{}, nvfm.SUCCESS
}

func (h *fakeHandle) GetUnsupportedFabricPartitions() (nvfm.UnsupportedFabricPartitionList, nvfm.Return) {
	return nvfm.UnsupportedFabricPartitionList{}, nvfm.SUCCESS
}

func (h *fakeHandle) GetNvlinkFailedDevices() (nvfm.NvlinkFailedDevices, nvfm.Return) {
	return nvfm.NvlinkFailedDevices{}, nvfm.SUCCESS
}

func (h *fakeHandle) SetActivatedFabricPartitions([]nvfm.FabricPartitionId) nvfm.Return {
	return nvfm.SUCCESS
}

// fakeLibrary is a stand-in for nvfm.Interface that hands out a brand new
// fakeHandle on every Connect and records them so a test can assert that each
// operation opened (and tore down) its own connection. activateRet and
// deactivateRet are stamped onto every handle it creates.
type fakeLibrary struct {
	connects   int
	connectRet nvfm.Return

	activateRet   nvfm.Return
	deactivateRet nvfm.Return

	// handles records every handle handed out, in connect order.
	handles []*fakeHandle
}

func (l *fakeLibrary) Connect(...nvfm.ConnectOption) (nvfm.Handle, nvfm.Return) {
	l.connects++
	if l.connectRet != nvfm.SUCCESS {
		return nil, l.connectRet
	}
	h := &fakeHandle{activateRet: l.activateRet, deactivateRet: l.deactivateRet}
	l.handles = append(l.handles, h)
	return h, nvfm.SUCCESS
}

func (l *fakeLibrary) ConnectWithParams(nvfm.ConnectParams) (nvfm.Handle, nvfm.Return) {
	return l.Connect()
}

func (l *fakeLibrary) ErrorString(r nvfm.Return) string   { return r.Error() }
func (l *fakeLibrary) Extensions() nvfm.ExtendedInterface { return nil }
func (l *fakeLibrary) Init() nvfm.Return                  { return nvfm.SUCCESS }
func (l *fakeLibrary) Shutdown() nvfm.Return              { return nvfm.SUCCESS }

// TestEachOperationOpensFreshConnection verifies that every partition
// operation opens its own connection and tears it down before returning.
func TestEachOperationOpensFreshConnection(t *testing.T) {
	lib := &fakeLibrary{}
	c := &nvfmClient{lib: lib}

	if _, err := c.GetSupportedFabricPartitions(); err != nil {
		t.Fatalf("GetSupportedFabricPartitions: %v", err)
	}
	if err := c.ActivateFabricPartition(7); err != nil {
		t.Fatalf("ActivateFabricPartition: %v", err)
	}
	if err := c.DeactivateFabricPartition(7); err != nil {
		t.Fatalf("DeactivateFabricPartition: %v", err)
	}

	// One connect (and one matching handle) per operation.
	if lib.connects != 3 {
		t.Errorf("expected 3 connects (one per operation), got %d", lib.connects)
	}
	if len(lib.handles) != 3 {
		t.Fatalf("expected 3 handles handed out, got %d", len(lib.handles))
	}
	// Every per-call connection must be disconnected exactly once.
	for i, h := range lib.handles {
		if h.disconnected != 1 {
			t.Errorf("handle %d: expected exactly one disconnect, got %d", i, h.disconnected)
		}
	}
	// The activate/deactivate landed on their own fresh handles.
	if len(lib.handles[1].activateIDs) != 1 || lib.handles[1].activateIDs[0] != 7 {
		t.Errorf("expected activate(7) on second handle, got %v", lib.handles[1].activateIDs)
	}
	if len(lib.handles[2].deactivateIDs) != 1 || lib.handles[2].deactivateIDs[0] != 7 {
		t.Errorf("expected deactivate(7) on third handle, got %v", lib.handles[2].deactivateIDs)
	}
}

// TestOperationReturnsLogicalError verifies that a non-success FM return (e.g.
// IN_USE) is surfaced as an error and the per-call connection is still torn
// down.
func TestOperationReturnsLogicalError(t *testing.T) {
	lib := &fakeLibrary{activateRet: nvfm.IN_USE}
	c := &nvfmClient{lib: lib}

	if err := c.ActivateFabricPartition(3); err == nil {
		t.Fatalf("expected IN_USE error, got nil")
	}
	if lib.connects != 1 {
		t.Errorf("expected exactly one connect, got %d", lib.connects)
	}
	if len(lib.handles) != 1 || lib.handles[0].disconnected != 1 {
		t.Errorf("expected the connection to be torn down once, got %+v", lib.handles)
	}
}

// TestConnectFailurePropagates verifies that if opening the per-call connection
// fails, the operation returns an error that wraps the connect failure.
func TestConnectFailurePropagates(t *testing.T) {
	lib := &fakeLibrary{connectRet: nvfm.CONNECTION_NOT_VALID}
	c := &nvfmClient{lib: lib}

	if err := c.DeactivateFabricPartition(5); err == nil {
		t.Fatalf("expected error when connect fails, got nil")
	}
	if lib.connects != 1 {
		t.Errorf("expected exactly one connect attempt, got %d", lib.connects)
	}
}
