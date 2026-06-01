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
	"errors"
	"reflect"
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// fakeDevice is a minimal stand-in for nvml.Device that only implements the
// methods Manager.pciIdToGpuModuleIdMap calls. Other methods of the embedded
// nvml.Device interface remain nil and will panic if invoked, which is the
// desired behavior for unexpected production calls in a test.
type fakeDevice struct {
	nvml.Device
	moduleID  int
	pciBusID  string
	getModRet nvml.Return
	getPciRet nvml.Return
}

func (f *fakeDevice) GetModuleId() (int, nvml.Return) {
	ret := f.getModRet
	if ret == 0 {
		ret = nvml.SUCCESS
	}
	return f.moduleID, ret
}

func (f *fakeDevice) GetPciInfo() (nvml.PciInfo, nvml.Return) {
	var info nvml.PciInfo
	copy(info.BusId[:], f.pciBusID)
	ret := f.getPciRet
	if ret == 0 {
		ret = nvml.SUCCESS
	}
	return info, ret
}

type fakeLib struct {
	devices     []*fakeDevice
	getCountRet nvml.Return
}

func (l *fakeLib) DeviceGetCount() (int, nvml.Return) {
	ret := l.getCountRet
	if ret == 0 {
		ret = nvml.SUCCESS
	}
	return len(l.devices), ret
}

func (l *fakeLib) DeviceGetHandleByIndex(idx int) (nvml.Device, nvml.Return) {
	if idx < 0 || idx >= len(l.devices) {
		return nil, nvml.ERROR_INVALID_ARGUMENT
	}
	return l.devices[idx], nvml.SUCCESS
}

func newFakeLib(gpus ...fakeDevice) *fakeLib {
	devs := make([]*fakeDevice, len(gpus))
	for i := range gpus {
		g := gpus[i]
		devs[i] = &g
	}
	return &fakeLib{devices: devs}
}

// fakeClient is an in-memory FM client used to drive the Manager. It
// records lifecycle calls so we can assert correct ordering, and lets
// individual methods be made to fail.
type fakeClient struct {
	partitions  []Partition
	unsupported []UnsupportedPartition

	initErr       error
	connectErr    error
	listErr       error
	listUnsupErr  error
	disconnErr    error
	shutdownErr   error
	activateErr   error
	deactivateErr error

	calls []string

	activated   []int // partitions Activate was called on (in order)
	deactivated []int // partitions Deactivate was called on
}

func (c *fakeClient) Init() error {
	c.calls = append(c.calls, "init")
	return c.initErr
}
func (c *fakeClient) Connect(ConnectParams) error {
	c.calls = append(c.calls, "connect")
	return c.connectErr
}
func (c *fakeClient) Disconnect() error {
	c.calls = append(c.calls, "disconnect")
	return c.disconnErr
}
func (c *fakeClient) Shutdown() error {
	c.calls = append(c.calls, "shutdown")
	return c.shutdownErr
}
func (c *fakeClient) GetSupportedFabricPartitions() ([]Partition, error) {
	c.calls = append(c.calls, "list")
	if c.listErr != nil {
		return nil, c.listErr
	}
	return c.partitions, nil
}
func (c *fakeClient) GetUnsupportedFabricPartitions() ([]UnsupportedPartition, error) {
	c.calls = append(c.calls, "list-unsupported")
	if c.listUnsupErr != nil {
		return nil, c.listUnsupErr
	}
	return c.unsupported, nil
}
func (c *fakeClient) ActivateFabricPartition(id int) error {
	c.calls = append(c.calls, "activate")
	if c.activateErr != nil {
		return c.activateErr
	}
	c.activated = append(c.activated, id)
	return nil
}
func (c *fakeClient) DeactivateFabricPartition(id int) error {
	c.calls = append(c.calls, "deactivate")
	if c.deactivateErr != nil {
		return c.deactivateErr
	}
	c.deactivated = append(c.deactivated, id)
	return nil
}

// designDocPartitions returns the example partitions from §"Fabric Manager
// advertised by GPUs" of the design doc.
func designDocPartitions() []Partition {
	return []Partition{
		{ID: 1, GPUs: gpus(1, 2, 3, 4, 5, 6, 7, 8)},
		{ID: 2, GPUs: gpus(1, 2, 5, 6)},
		{ID: 3, GPUs: gpus(3, 4, 7, 8)},
		{ID: 4, GPUs: gpus(1, 3)},
		{ID: 8, GPUs: gpus(1)},
	}
}

func gpus(ids ...int) []PartitionGPU {
	out := make([]PartitionGPU, len(ids))
	for i, id := range ids {
		out[i] = PartitionGPU{PhysicalID: id}
	}
	return out
}

// eightGPUNode returns an NVML mock with 8 GPUs whose moduleIds are 1..8 and
// whose PCI bus IDs follow the canonical 8-digit-domain form NVML reports.
func eightGPUNode() *fakeLib {
	return newFakeLib(
		fakeDevice{moduleID: 1, pciBusID: "00000000:3B:00.0"},
		fakeDevice{moduleID: 2, pciBusID: "00000000:5C:00.0"},
		fakeDevice{moduleID: 3, pciBusID: "00000000:9D:00.0"},
		fakeDevice{moduleID: 4, pciBusID: "00000000:BE:00.0"},
		fakeDevice{moduleID: 5, pciBusID: "00000000:CF:00.0"},
		fakeDevice{moduleID: 6, pciBusID: "00000000:E0:00.0"},
		fakeDevice{moduleID: 7, pciBusID: "00000000:F1:00.0"},
		fakeDevice{moduleID: 8, pciBusID: "00000000:F2:00.0"},
	)
}

func TestPartitionGPUModuleIDs(t *testing.T) {
	p := Partition{GPUs: gpus(3, 7, 2)}
	if got, want := p.GPUModuleIDs(), []int{3, 7, 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("GPUModuleIDs = %v, want %v", got, want)
	}
}

func TestOpenLifecycle(t *testing.T) {
	client := &fakeClient{partitions: designDocPartitions()}

	m, err := Open(eightGPUNode(), client, ConnectParams{AddressInfo: "127.0.0.1:6666"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if m == nil {
		t.Fatal("nil manager")
	}

	// Open keeps the connection alive so the caller can later activate /
	// deactivate partitions.
	want := []string{"init", "connect", "list"}
	if !reflect.DeepEqual(client.calls, want) {
		t.Errorf("after Open, client call order = %v, want %v", client.calls, want)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want = []string{"init", "connect", "list", "disconnect", "shutdown"}
	if !reflect.DeepEqual(client.calls, want) {
		t.Errorf("after Close, client call order = %v, want %v", client.calls, want)
	}

	// Close is idempotent.
	if err := m.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if !reflect.DeepEqual(client.calls, want) {
		t.Errorf("Close should be idempotent; calls = %v", client.calls)
	}
}

func TestDiscoverLookups(t *testing.T) {
	client := &fakeClient{partitions: designDocPartitions()}
	m, err := Open(eightGPUNode(), client, ConnectParams{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	for _, pci := range []string{"00000000:3B:00.0", "0000:3b:00.0", "00000000:3b:00.0"} {
		id, ok := m.GetModuleIDByPCI(pci)
		if !ok || id != 1 {
			t.Errorf("GetModuleIDByPCI(%q) = (%d, %v), want (1, true)", pci, id, ok)
		}
	}
	if _, ok := m.GetModuleIDByPCI("00000000:FF:00.0"); ok {
		t.Errorf("GetModuleIDByPCI for unknown PCI returned ok=true")
	}

	pci, ok := m.GetPCIByModuleID(2)
	if !ok || pci != "00000000:5C:00.0" {
		t.Errorf("GetPCIByModuleID(2) = (%q, %v), want (00000000:5C:00.0, true)", pci, ok)
	}
	if _, ok := m.GetPCIByModuleID(99); ok {
		t.Errorf("GetPCIByModuleID(99) returned ok=true")
	}

	// GPU at module 1 belongs to partitions 1, 2, 4, 8.
	parts, ok := m.GetPartitionsByPCI("00000000:3B:00.0")
	if !ok {
		t.Fatalf("GetPartitionsByPCI ok=false")
	}
	if want := []int{1, 2, 4, 8}; !reflect.DeepEqual(parts, want) {
		t.Errorf("GetPartitionsByPCI = %v, want %v", parts, want)
	}

	// GPU module 3 belongs to partitions 1, 3, 4.
	parts = m.GetPartitionsByModuleID(3)
	if want := []int{1, 3, 4}; !reflect.DeepEqual(parts, want) {
		t.Errorf("GetPartitionsByModuleID(3) = %v, want %v", parts, want)
	}

	// GPU module 6 belongs only to partitions 1, 2.
	parts = m.GetPartitionsByModuleID(6)
	if want := []int{1, 2}; !reflect.DeepEqual(parts, want) {
		t.Errorf("GetPartitionsByModuleID(6) = %v, want %v", parts, want)
	}

	if p, ok := m.GetPartition(2); !ok || !reflect.DeepEqual(p.GPUModuleIDs(), []int{1, 2, 5, 6}) {
		t.Errorf("GetPartition(2) = (%+v, %v)", p, ok)
	}
	if _, ok := m.GetPartition(999); ok {
		t.Errorf("GetPartition(999) returned ok=true")
	}

	// Partitions() returns all, sorted by id.
	all := m.Partitions()
	gotIDs := make([]int, len(all))
	for i, p := range all {
		gotIDs[i] = p.ID
	}
	if want := []int{1, 2, 3, 4, 8}; !reflect.DeepEqual(gotIDs, want) {
		t.Errorf("Partitions() ids = %v, want %v", gotIDs, want)
	}
}

// TestDiscoverFMPartitionReferencesGPUUnknownToNVML covers FM reporting a
// gpuModuleId that NVML cannot resolve to a PCI bus ID (e.g. a GPU bound to
// vfio-pci before nv-fabricmanager started, for which FM supplies no usable
// PCI). Open must not fail: the partition is recorded and the GPU is simply
// published without FM attributes until it is rebound to the nvidia driver and
// the module mapping is refreshed.
func TestDiscoverFMPartitionReferencesGPUUnknownToNVML(t *testing.T) {
	client := &fakeClient{partitions: []Partition{
		{ID: 1, GPUs: gpus(1, 2, 99)}, // 99 not present in NVML
	}}
	m, err := Open(eightGPUNode(), client, ConnectParams{})
	if err != nil {
		t.Fatalf("Open should tolerate an unresolvable gpuModuleId: %v", err)
	}
	defer m.Close()

	if _, ok := m.GetPCIByModuleID(99); ok {
		t.Errorf("gpuModuleId 99 unexpectedly resolvable")
	}
	// The partition is still recorded despite the unresolvable member.
	if _, ok := m.GetPartition(1); !ok {
		t.Errorf("partition 1 not recorded")
	}
}

// TestSetModuleMappingForPCIRetainsVfioBoundEntries verifies that recording a
// single rebound GPU's mapping leaves entries for GPUs still bound to vfio-pci
// (invisible to NVML) untouched. Those entries are still needed to deactivate
// the GPUs' fabric partitions on a later unprepare.
func TestSetModuleMappingForPCIRetainsVfioBoundEntries(t *testing.T) {
	// All three GPUs are visible at Open and recorded.
	m, err := Open(newFakeLib(
		fakeDevice{moduleID: 1, pciBusID: "00000000:3B:00.0"},
		fakeDevice{moduleID: 2, pciBusID: "00000000:5C:00.0"},
		fakeDevice{moduleID: 3, pciBusID: "00000000:9D:00.0"},
	), &fakeClient{partitions: []Partition{{ID: 1, GPUs: gpus(1, 2, 3)}}}, ConnectParams{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close()

	// Module 1 is rebound to the nvidia driver and re-recorded; modules 2 and 3
	// are now bound to vfio-pci but their entries must be retained.
	if err := m.SetModuleMappingForPCI("00000000:3B:00.0", 1); err != nil {
		t.Fatalf("SetModuleMappingForPCI: %v", err)
	}

	for mod, pci := range map[int]string{
		1: "00000000:3B:00.0",
		2: "00000000:5C:00.0",
		3: "00000000:9D:00.0",
	} {
		if got, ok := m.GetPCIByModuleID(mod); !ok || got != pci {
			t.Errorf("GetPCIByModuleID(%d) = (%q, %v), want (%q, true)", mod, got, ok, pci)
		}
	}
}

// TestSetModuleMappingForPCI verifies the targeted single-entry update used
// during unprepare: a GPU bound to vfio-pci at Open (invisible to NVML) becomes
// resolvable once its (PCI, gpuModuleId) pair is recorded directly, without a
// full NVML walk. It also checks that a conflicting update prunes the stale
// reverse mapping so both maps stay consistent.
func TestSetModuleMappingForPCI(t *testing.T) {
	// Module 3 is bound to vfio-pci at Open and therefore invisible to NVML,
	// but FM reports it across its partitions.
	libAtOpen := newFakeLib(
		fakeDevice{moduleID: 1, pciBusID: "00000000:3B:00.0"},
		fakeDevice{moduleID: 2, pciBusID: "00000000:5C:00.0"},
	)
	client := &fakeClient{partitions: []Partition{
		{ID: 1, GPUs: gpus(1, 2, 3)},
		{ID: 2, GPUs: gpus(3)},
	}}
	m, err := Open(libAtOpen, client, ConnectParams{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close()

	if _, ok := m.GetPCIByModuleID(3); ok {
		t.Errorf("module 3 unexpectedly resolvable before rebind")
	}

	// The GPU is rebound to the nvidia driver; record only its pair. Use the
	// lower-case 4-digit form to verify normalization to the canonical form.
	if err := m.SetModuleMappingForPCI("0000:9d:00.0", 3); err != nil {
		t.Fatalf("SetModuleMappingForPCI: %v", err)
	}
	if id, ok := m.GetModuleIDByPCI("00000000:9D:00.0"); !ok || id != 3 {
		t.Errorf("GetModuleIDByPCI(passthrough) = (%d, %v), want (3, true)", id, ok)
	}
	if pci, ok := m.GetPCIByModuleID(3); !ok || pci != "00000000:9D:00.0" {
		t.Errorf("GetPCIByModuleID(3) = (%q, %v), want (00000000:9D:00.0, true)", pci, ok)
	}
	// Previously recorded GPUs are untouched by the single-entry update.
	if id, ok := m.GetModuleIDByPCI("00000000:3B:00.0"); !ok || id != 1 {
		t.Errorf("GetModuleIDByPCI(nvml) = (%d, %v), want (1, true)", id, ok)
	}

	// A conflicting update (PCI now reports a different moduleId) overwrites
	// the entry and prunes the now-stale reverse mapping.
	if err := m.SetModuleMappingForPCI("00000000:9D:00.0", 7); err != nil {
		t.Fatalf("SetModuleMappingForPCI (conflict): %v", err)
	}
	if id, ok := m.GetModuleIDByPCI("00000000:9D:00.0"); !ok || id != 7 {
		t.Errorf("GetModuleIDByPCI after conflict = (%d, %v), want (7, true)", id, ok)
	}
	if _, ok := m.GetPCIByModuleID(3); ok {
		t.Errorf("stale gpuModuleId 3 still resolvable after conflicting update")
	}
	if pci, ok := m.GetPCIByModuleID(7); !ok || pci != "00000000:9D:00.0" {
		t.Errorf("GetPCIByModuleID(7) = (%q, %v), want (00000000:9D:00.0, true)", pci, ok)
	}

	// An empty PCI bus ID is rejected.
	if err := m.SetModuleMappingForPCI("", 9); err == nil {
		t.Errorf("expected error for empty PCI bus ID")
	}
}

// TestSetModuleMappingForPCIClosed verifies the targeted update errors on a
// closed manager.
func TestSetModuleMappingForPCIClosed(t *testing.T) {
	m, err := Open(eightGPUNode(), &fakeClient{partitions: designDocPartitions()}, ConnectParams{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := m.SetModuleMappingForPCI("00000000:3B:00.0", 1); err == nil {
		t.Errorf("expected error updating module mapping on closed manager")
	}
}

// TestDiscoverAllGPUsInPassthrough covers a node where every GPU is bound to
// vfio-pci, so NVML enumerates zero devices (a count of 0 is not an NVML
// error). Open must still succeed (FM partitions are recorded); the GPUs are
// simply unresolvable until they are rebound to the nvidia driver and the
// module mapping is refreshed.
func TestDiscoverAllGPUsInPassthrough(t *testing.T) {
	lib := newFakeLib() // NVML sees nothing.
	client := &fakeClient{partitions: []Partition{
		{ID: 1, GPUs: gpus(1, 2)},
		{ID: 8, GPUs: gpus(1)},
	}}

	m, err := Open(lib, client, ConnectParams{})
	if err != nil {
		t.Fatalf("Open with all GPUs in passthrough: %v", err)
	}
	defer m.Close()

	// No GPU is resolvable yet (none visible to NVML, FM supplies no PCI).
	if _, ok := m.GetPCIByModuleID(1); ok {
		t.Errorf("module 1 unexpectedly resolvable before rebind")
	}
	// Partitions are still recorded.
	if parts := m.GetPartitionsByModuleID(1); !reflect.DeepEqual(parts, []int{1, 8}) {
		t.Errorf("GetPartitionsByModuleID(1) = %v, want [1 8]", parts)
	}

	// After all GPUs are rebound to the nvidia driver they become resolvable
	// as each one's mapping is recorded.
	for pci, mod := range map[string]int{"00000000:3B:00.0": 1, "00000000:5C:00.0": 2} {
		if err := m.SetModuleMappingForPCI(pci, mod); err != nil {
			t.Fatalf("SetModuleMappingForPCI(%q, %d): %v", pci, mod, err)
		}
	}
	for mod, want := range map[int]string{1: "00000000:3B:00.0", 2: "00000000:5C:00.0"} {
		if got, ok := m.GetPCIByModuleID(mod); !ok || got != want {
			t.Errorf("GetPCIByModuleID(%d) = (%q, %v), want (%q, true)", mod, got, ok, want)
		}
	}
}

func TestDiscoverFMDuplicatePartitionID(t *testing.T) {
	client := &fakeClient{partitions: []Partition{
		{ID: 1, GPUs: gpus(1)},
		{ID: 1, GPUs: gpus(2)},
	}}
	if _, err := Open(eightGPUNode(), client, ConnectParams{}); err == nil {
		t.Errorf("expected error for duplicate partitionId, got nil")
	}
}

func TestDiscoverFMEmptyPartition(t *testing.T) {
	client := &fakeClient{partitions: []Partition{
		{ID: 1, GPUs: nil},
	}}
	if _, err := Open(eightGPUNode(), client, ConnectParams{}); err == nil {
		t.Errorf("expected error for empty partition, got nil")
	}
}

func TestDiscoverNVMLDuplicateModuleID(t *testing.T) {
	lib := newFakeLib(
		fakeDevice{moduleID: 1, pciBusID: "00000000:3B:00.0"},
		fakeDevice{moduleID: 1, pciBusID: "00000000:5C:00.0"},
	)
	client := &fakeClient{partitions: designDocPartitions()}
	if _, err := Open(lib, client, ConnectParams{}); err == nil {
		t.Errorf("expected error for duplicate moduleId from NVML, got nil")
	}
}

func TestDiscoverNVMLFailure(t *testing.T) {
	lib := &fakeLib{getCountRet: nvml.ERROR_UNKNOWN}
	client := &fakeClient{partitions: designDocPartitions()}
	if _, err := Open(lib, client, ConnectParams{}); err == nil {
		t.Errorf("expected error from NVML DeviceGetCount, got nil")
	}
}

func TestDiscoverClientErrors(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*fakeClient)
		wantCall string
	}{
		{
			name:     "init fails",
			mutate:   func(c *fakeClient) { c.initErr = errors.New("boom") },
			wantCall: "init",
		},
		{
			name:     "connect fails",
			mutate:   func(c *fakeClient) { c.connectErr = errors.New("boom") },
			wantCall: "connect",
		},
		{
			name:     "list fails",
			mutate:   func(c *fakeClient) { c.listErr = errors.New("boom") },
			wantCall: "list",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeClient{partitions: designDocPartitions()}
			tc.mutate(client)
			if _, err := Open(eightGPUNode(), client, ConnectParams{}); err == nil {
				t.Errorf("expected error, got nil")
			}
			// We must have at least reached the failing call.
			found := false
			for _, c := range client.calls {
				if c == tc.wantCall {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected client to reach %q, calls=%v", tc.wantCall, client.calls)
			}
		})
	}
}

func TestDiscoverNilArgs(t *testing.T) {
	if _, err := Open(nil, &fakeClient{}, ConnectParams{}); err == nil {
		t.Errorf("expected error for nil NVML interface")
	}
	if _, err := Open(eightGPUNode(), nil, ConnectParams{}); err == nil {
		t.Errorf("expected error for nil FM client")
	}
}

func TestGetPartitionsBySizeByModuleID(t *testing.T) {
	client := &fakeClient{partitions: designDocPartitions()}
	m, err := Open(eightGPUNode(), client, ConnectParams{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close()

	// Module 1 is in: partition 1 (size 8), 2 (size 4), 4 (size 2), 8 (size 1).
	got, err := m.GetPartitionsBySizeByModuleID(1)
	if err != nil {
		t.Fatalf("GetPartitionsBySizeByModuleID(1): %v", err)
	}
	want := map[int]int{8: 1, 4: 2, 2: 4, 1: 8}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("size->partitionId for module 1 = %v, want %v", got, want)
	}

	// Same via PCI lookup.
	gotByPCI, ok, err := m.GetPartitionsBySizeByPCI("00000000:3B:00.0")
	if err != nil || !ok {
		t.Fatalf("GetPartitionsBySizeByPCI(known): ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(gotByPCI, want) {
		t.Errorf("size->partitionId via PCI = %v, want %v", gotByPCI, want)
	}

	// Unknown PCI returns ok=false, no error.
	got2, ok, err := m.GetPartitionsBySizeByPCI("00000000:FF:00.0")
	if got2 != nil || ok || err != nil {
		t.Errorf("unknown PCI lookup: got=%v ok=%v err=%v", got2, ok, err)
	}
}

func TestGetPartitionsBySizeAmbiguous(t *testing.T) {
	// Two partitions of the same size both containing module 1.
	parts := []Partition{
		{ID: 10, GPUs: gpus(1, 2)},
		{ID: 11, GPUs: gpus(1, 3)},
	}
	client := &fakeClient{partitions: parts}
	m, err := Open(eightGPUNode(), client, ConnectParams{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close()

	if _, err := m.GetPartitionsBySizeByModuleID(1); err == nil {
		t.Errorf("expected error for ambiguous size mapping")
	}
}

func TestActivateDeactivate(t *testing.T) {
	client := &fakeClient{partitions: designDocPartitions()}
	m, err := Open(eightGPUNode(), client, ConnectParams{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close()

	if err := m.ActivatePartition(2); err != nil {
		t.Fatalf("ActivatePartition(2): %v", err)
	}
	if err := m.ActivatePartition(8); err != nil {
		t.Fatalf("ActivatePartition(8): %v", err)
	}

	if got := m.ActivatedPartitions(); !reflect.DeepEqual(got, []int{2, 8}) {
		t.Errorf("ActivatedPartitions = %v, want [2 8]", got)
	}
	if !reflect.DeepEqual(client.activated, []int{2, 8}) {
		t.Errorf("client.activated = %v, want [2 8]", client.activated)
	}
	if p, _ := m.GetPartition(2); !p.IsActive {
		t.Errorf("expected partition 2 IsActive=true, got partition=%+v", p)
	}

	if err := m.ActivatePartition(999); err == nil {
		t.Errorf("expected error activating unknown partition")
	}

	if err := m.DeactivatePartition(2); err != nil {
		t.Fatalf("DeactivatePartition(2): %v", err)
	}
	if got := m.ActivatedPartitions(); !reflect.DeepEqual(got, []int{8}) {
		t.Errorf("after deactivate, ActivatedPartitions = %v, want [8]", got)
	}
	if p, _ := m.GetPartition(2); p.IsActive {
		t.Errorf("expected partition 2 IsActive=false after deactivate")
	}
}

func TestActivateInheritedFromFM(t *testing.T) {
	parts := designDocPartitions()
	parts[1].IsActive = true // partition 2 already active per FM
	client := &fakeClient{partitions: parts}

	m, err := Open(eightGPUNode(), client, ConnectParams{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close()

	if got := m.ActivatedPartitions(); !reflect.DeepEqual(got, []int{2}) {
		t.Errorf("expected activated set seeded from FM = [2], got %v", got)
	}
}

func TestUnsupportedPartitions(t *testing.T) {
	client := &fakeClient{
		partitions: designDocPartitions(),
		unsupported: []UnsupportedPartition{
			{ID: 99, GPUPhysicalIDs: []int{1, 2}},
		},
	}
	m, err := Open(eightGPUNode(), client, ConnectParams{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close()

	unsup, err := m.UnsupportedPartitions()
	if err != nil {
		t.Fatalf("UnsupportedPartitions: %v", err)
	}
	if len(unsup) != 1 || unsup[0].ID != 99 {
		t.Errorf("UnsupportedPartitions = %+v", unsup)
	}
}

func TestOperationsOnClosedManager(t *testing.T) {
	client := &fakeClient{partitions: designDocPartitions()}
	m, err := Open(eightGPUNode(), client, ConnectParams{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	if err := m.ActivatePartition(2); err == nil {
		t.Errorf("expected error activating on closed manager")
	}
	if err := m.DeactivatePartition(2); err == nil {
		t.Errorf("expected error deactivating on closed manager")
	}
	if _, err := m.UnsupportedPartitions(); err == nil {
		t.Errorf("expected error fetching unsupported on closed manager")
	}
	if err := m.SetModuleMappingForPCI("00000000:3B:00.0", 1); err == nil {
		t.Errorf("expected error updating module mapping on closed manager")
	}
}

func TestActivatePartitionFMError(t *testing.T) {
	client := &fakeClient{
		partitions:  designDocPartitions(),
		activateErr: errors.New("FM_ST_IN_USE"),
	}
	m, err := Open(eightGPUNode(), client, ConnectParams{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if err := m.ActivatePartition(2); err == nil {
		t.Errorf("expected activation error, got nil")
	}
	if got := m.ActivatedPartitions(); len(got) != 0 {
		t.Errorf("expected no activated partitions on failure, got %v", got)
	}
}

func TestStubClient(t *testing.T) {
	c := NewStubClient()
	if err := c.Init(); err != nil {
		t.Errorf("stub Init: %v", err)
	}
	if err := c.Connect(ConnectParams{}); err != nil {
		t.Errorf("stub Connect: %v", err)
	}
	if _, err := c.GetSupportedFabricPartitions(); !errors.Is(err, ErrUnimplemented) {
		t.Errorf("stub GetSupportedFabricPartitions err = %v, want ErrUnimplemented", err)
	}
	if _, err := c.GetUnsupportedFabricPartitions(); !errors.Is(err, ErrUnimplemented) {
		t.Errorf("stub GetUnsupportedFabricPartitions err = %v, want ErrUnimplemented", err)
	}
	if err := c.ActivateFabricPartition(1); !errors.Is(err, ErrUnimplemented) {
		t.Errorf("stub ActivateFabricPartition err = %v, want ErrUnimplemented", err)
	}
	if err := c.DeactivateFabricPartition(1); !errors.Is(err, ErrUnimplemented) {
		t.Errorf("stub DeactivateFabricPartition err = %v, want ErrUnimplemented", err)
	}
	if err := c.Disconnect(); err != nil {
		t.Errorf("stub Disconnect: %v", err)
	}
	if err := c.Shutdown(); err != nil {
		t.Errorf("stub Shutdown: %v", err)
	}
}
