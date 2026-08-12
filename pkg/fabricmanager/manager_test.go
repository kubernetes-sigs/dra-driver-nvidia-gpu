/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package fabricmanager

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// fakeClient is an in-memory FM client used to drive the Manager. It
// records lifecycle calls so we can assert correct ordering, and lets
// individual methods be made to fail.
type fakeClient struct {
	partitions []Partition

	initErr       error
	listErr       error
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
func (c *fakeClient) ActivateFabricPartition(id int) error {
	c.calls = append(c.calls, "activate")
	if c.activateErr != nil {
		return c.activateErr
	}
	c.activated = append(c.activated, id)
	c.setActive(id, true)
	return nil
}
func (c *fakeClient) DeactivateFabricPartition(id int) error {
	c.calls = append(c.calls, "deactivate")
	if c.deactivateErr != nil {
		return c.deactivateErr
	}
	c.deactivated = append(c.deactivated, id)
	c.setActive(id, false)
	return nil
}

// setActive mirrors a (de)activation into the backing partition list so that a
// subsequent GetSupportedFabricPartitions reflects it, as the real FM service
// would.
func (c *fakeClient) setActive(id int, active bool) {
	for i := range c.partitions {
		if c.partitions[i].ID == id {
			c.partitions[i].IsActive = active
			return
		}
	}
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

func TestPartitionGPUModuleIDs(t *testing.T) {
	p := Partition{GPUs: gpus(3, 7, 2)}
	if got, want := p.GPUModuleIDs(), []int{3, 7, 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("GPUModuleIDs = %v, want %v", got, want)
	}
}

func TestOpenLifecycle(t *testing.T) {
	client := &fakeClient{partitions: designDocPartitions()}

	m, err := Open(client)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if m == nil {
		t.Fatal("nil manager")
	}

	// Open initializes the library and reads the supported partitions. Each
	// partition operation opens its own connection later, so no connect step
	// happens here.
	want := []string{"init", "list"}
	if !reflect.DeepEqual(client.calls, want) {
		t.Errorf("after Open, client call order = %v, want %v", client.calls, want)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want = []string{"init", "list", "shutdown"}
	if !reflect.DeepEqual(client.calls, want) {
		t.Errorf("after Close, client call order = %v, want %v", client.calls, want)
	}
}

func TestDiscoverLookups(t *testing.T) {
	client := &fakeClient{partitions: designDocPartitions()}
	m, err := Open(client)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if p, ok := m.GetPartition(2); !ok || !reflect.DeepEqual(p.GPUModuleIDs(), []int{1, 2, 5, 6}) {
		t.Errorf("GetPartition(2) = (%+v, %v)", p, ok)
	}
	if _, ok := m.GetPartition(999); ok {
		t.Errorf("GetPartition(999) returned ok=true")
	}
}

// TestFindPartitionByModuleIDs covers the inverse lookup used at prepare time:
// given the exact set of GPU module ids in a claim, find the single partition
// whose membership matches exactly.
func TestFindPartitionByModuleIDs(t *testing.T) {
	client := &fakeClient{partitions: designDocPartitions()}
	m, err := Open(client)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close()

	// Exact match (order independent).
	if id, ok := m.FindPartitionByModuleIDs([]int{6, 1, 5, 2}); !ok || id != 2 {
		t.Errorf("FindPartitionByModuleIDs({1,2,5,6}) = (%d, %v), want (2, true)", id, ok)
	}
	// Single-GPU partition.
	if id, ok := m.FindPartitionByModuleIDs([]int{1}); !ok || id != 8 {
		t.Errorf("FindPartitionByModuleIDs({1}) = (%d, %v), want (8, true)", id, ok)
	}
	// A subset of a partition does not match.
	if _, ok := m.FindPartitionByModuleIDs([]int{1, 2}); ok {
		t.Errorf("FindPartitionByModuleIDs({1,2}) unexpectedly matched")
	}
	// The empty set never matches.
	if _, ok := m.FindPartitionByModuleIDs(nil); ok {
		t.Errorf("FindPartitionByModuleIDs(nil) unexpectedly matched")
	}
}

// TestDiscoverFMPartitionWithArbitraryModuleIDs covers FM reporting partitions
// whose member gpuModuleIDs the driver has not (yet) resolved to any local GPU
// (e.g. a GPU bound to vfio-pci before nv-fabricmanager started). The Manager
// no longer owns a PCI<->gpuModuleID map, so it simply records the partitions
// as reported; resolution happens on the VFIO device side.
func TestDiscoverFMPartitionWithArbitraryModuleIDs(t *testing.T) {
	client := &fakeClient{partitions: []Partition{
		{ID: 1, GPUs: gpus(1, 2, 99)},
	}}
	m, err := Open(client)
	if err != nil {
		t.Fatalf("Open should record partitions regardless of module ids: %v", err)
	}
	defer m.Close()

	p, ok := m.GetPartition(1)
	if !ok {
		t.Errorf("partition 1 not recorded")
	}
	if want := []int{1, 2, 99}; !reflect.DeepEqual(p.GPUModuleIDs(), want) {
		t.Errorf("partition 1 module ids = %v, want %v", p.GPUModuleIDs(), want)
	}
}

func TestDiscoverFMDuplicatePartitionID(t *testing.T) {
	client := &fakeClient{partitions: []Partition{
		{ID: 1, GPUs: gpus(1)},
		{ID: 1, GPUs: gpus(2)},
	}}
	if _, err := Open(client); err == nil {
		t.Errorf("expected error for duplicate partitionId, got nil")
	}
}

func TestDiscoverFMEmptyPartition(t *testing.T) {
	client := &fakeClient{partitions: []Partition{
		{ID: 1, GPUs: nil},
	}}
	if _, err := Open(client); err == nil {
		t.Errorf("expected error for empty partition, got nil")
	}
}

func TestDiscoverFMDuplicateGPUMember(t *testing.T) {
	client := &fakeClient{partitions: []Partition{
		{ID: 1, GPUs: gpus(1, 2, 1)},
	}}
	if _, err := Open(client); err == nil {
		t.Errorf("expected error for duplicate gpuModuleID member, got nil")
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
			name:     "list fails",
			mutate:   func(c *fakeClient) { c.listErr = errors.New("boom") },
			wantCall: "list",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeClient{partitions: designDocPartitions()}
			tc.mutate(client)
			if _, err := Open(client); err == nil {
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
	if _, err := Open(nil); err == nil {
		t.Errorf("expected error for nil FM client")
	}
}

func TestGetPartitionsBySizeByModuleID(t *testing.T) {
	client := &fakeClient{partitions: designDocPartitions()}
	m, err := Open(client)
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

	// An unknown module id yields an empty map and no error.
	got2, err := m.GetPartitionsBySizeByModuleID(99)
	if err != nil || len(got2) != 0 {
		t.Errorf("unknown module lookup: got=%v err=%v", got2, err)
	}
}

func TestGetPartitionsBySizeAmbiguous(t *testing.T) {
	// Two partitions of the same size both containing module 1.
	parts := []Partition{
		{ID: 10, GPUs: gpus(1, 2)},
		{ID: 11, GPUs: gpus(1, 3)},
	}
	client := &fakeClient{partitions: parts}
	m, err := Open(client)
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
	m, err := Open(client)
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

	assertActivated := func(id int, want bool) {
		t.Helper()
		got, err := m.isPartitionActivated(id)
		if err != nil {
			t.Fatalf("IsPartitionActivated(%d): %v", id, err)
		}
		if got != want {
			t.Errorf("IsPartitionActivated(%d) = %v, want %v", id, got, want)
		}
	}

	assertActivated(2, true)
	assertActivated(8, true)
	assertActivated(1, false)
	if !reflect.DeepEqual(client.activated, []int{2, 8}) {
		t.Errorf("client.activated = %v, want [2 8]", client.activated)
	}

	if err := m.ActivatePartition(999); err == nil {
		t.Errorf("expected error activating unknown partition")
	}

	if err := m.DeactivatePartition(2); err != nil {
		t.Fatalf("DeactivatePartition(2): %v", err)
	}
	assertActivated(2, false)
	assertActivated(8, true)
}

// TestActivatedResolvedFromFM verifies isPartitionActivated reflects whatever
// FM reports as active, without the Manager caching any activation state of its
// own (e.g. a partition activated externally, or still active after a driver
// restart).
func TestActivatedResolvedFromFM(t *testing.T) {
	parts := designDocPartitions()
	parts[1].IsActive = true // partition 2 already active per FM
	client := &fakeClient{partitions: parts}

	m, err := Open(client)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close()

	if active, err := m.isPartitionActivated(2); err != nil || !active {
		t.Errorf("isPartitionActivated(2) = (%v, %v), want (true, nil)", active, err)
	}
	if active, err := m.isPartitionActivated(1); err != nil || active {
		t.Errorf("isPartitionActivated(1) = (%v, %v), want (false, nil)", active, err)
	}
	// An unknown partition is simply not active.
	if active, err := m.isPartitionActivated(999); err != nil || active {
		t.Errorf("isPartitionActivated(999) = (%v, %v), want (false, nil)", active, err)
	}
}

// TestIsPartitionActivatedListError verifies isPartitionActivated surfaces an
// error when the live FM query fails.
func TestIsPartitionActivatedListError(t *testing.T) {
	client := &fakeClient{partitions: designDocPartitions()}
	m, err := Open(client)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close()

	client.listErr = errors.New("boom")
	if _, err := m.isPartitionActivated(2); err == nil {
		t.Errorf("expected error from isPartitionActivated when FM query fails")
	}
}

func TestActivatePartitionFMError(t *testing.T) {
	client := &fakeClient{
		partitions:  designDocPartitions(),
		activateErr: errors.New("FM_ST_IN_USE"),
	}
	m, err := Open(client)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if err := m.ActivatePartition(2); err == nil {
		t.Errorf("expected activation error, got nil")
	}
	if active, err := m.isPartitionActivated(2); err != nil || active {
		t.Errorf("isPartitionActivated(2) after failed activation = (%v, %v), want (false, nil)", active, err)
	}
}

func TestStubClient(t *testing.T) {
	c := NewStubClient()
	if err := c.Init(); err != nil {
		t.Errorf("stub Init: %v", err)
	}
	if _, err := c.GetSupportedFabricPartitions(); !errors.Is(err, ErrUnimplemented) {
		t.Errorf("stub GetSupportedFabricPartitions err = %v, want ErrUnimplemented", err)
	}
	if err := c.ActivateFabricPartition(1); !errors.Is(err, ErrUnimplemented) {
		t.Errorf("stub ActivateFabricPartition err = %v, want ErrUnimplemented", err)
	}
	if err := c.DeactivateFabricPartition(1); !errors.Is(err, ErrUnimplemented) {
		t.Errorf("stub DeactivateFabricPartition err = %v, want ErrUnimplemented", err)
	}
	if err := c.Shutdown(); err != nil {
		t.Errorf("stub Shutdown: %v", err)
	}
}

// pciGPUs builds partition members carrying both a module id and a PCI bus id,
// in the 8-digit upper-case domain form FM actually reports.
func pciGPUs(ids ...int) []PartitionGPU {
	out := make([]PartitionGPU, len(ids))
	for i, id := range ids {
		out[i] = PartitionGPU{
			PhysicalID: id,
			PCIBusID:   fmt.Sprintf("00000000:%02X:00.0", id),
		}
	}
	return out
}

func TestNormalizePCIBusID(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"fm form", "00000000:0F:00.0", "0000:0f:00.0"},
		{"sysfs form", "0000:0f:00.0", "0000:0f:00.0"},
		{"surrounding whitespace", "  00000000:0F:00.0\n", "0000:0f:00.0"},
		{"already lower 8-digit", "00000000:0f:00.0", "0000:0f:00.0"},
		// Anything shorter than a full BDF is lower-cased but not truncated, so a
		// malformed value cannot accidentally equal a well-formed one.
		{"short value", "0F:00.0", "0f:00.0"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizePCIBusID(tc.in); got != tc.want {
				t.Errorf("NormalizePCIBusID(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestModuleIDByPCIBusID(t *testing.T) {
	client := &fakeClient{partitions: []Partition{
		{ID: 1, GPUs: pciGPUs(1, 2, 3, 4)},
		{ID: 2, GPUs: pciGPUs(1, 2)},
	}}
	m, err := Open(client)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// A GPU appearing in several partitions is indexed once, and the caller may
	// present the address in either the FM or the sysfs form.
	for _, query := range []string{"00000000:02:00.0", "0000:02:00.0", "0000:02:00.0"} {
		id, ok := m.ModuleIDByPCIBusID(query)
		if !ok {
			t.Fatalf("ModuleIDByPCIBusID(%q): not found", query)
		}
		if id != 2 {
			t.Errorf("ModuleIDByPCIBusID(%q) = %d, want 2", query, id)
		}
	}

	if _, ok := m.ModuleIDByPCIBusID("0000:ff:00.0"); ok {
		t.Error("ModuleIDByPCIBusID for an address FM never reported: got ok=true, want false")
	}
}

func TestModuleIDByPCIBusIDMissingAddresses(t *testing.T) {
	// FM reporting no PCI bus id must not produce a bogus "" -> moduleID entry
	// that every unknown address would then match.
	client := &fakeClient{partitions: []Partition{{ID: 1, GPUs: gpus(1, 2)}}}
	m, err := Open(client)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if id, ok := m.ModuleIDByPCIBusID(""); ok {
		t.Errorf("ModuleIDByPCIBusID(\"\") = (%d, true), want not found", id)
	}
}

func TestOpenRejectsConflictingPCIBusID(t *testing.T) {
	// The same address reported with two different module ids means we cannot
	// trust the index; activating a partition built from the wrong GPU would
	// program the fabric for devices the claim does not hold.
	client := &fakeClient{partitions: []Partition{
		{ID: 1, GPUs: []PartitionGPU{{PhysicalID: 1, PCIBusID: "00000000:01:00.0"}}},
		{ID: 2, GPUs: []PartitionGPU{{PhysicalID: 2, PCIBusID: "00000000:01:00.0"}}},
	}}
	if _, err := Open(client); err == nil {
		t.Fatal("Open with conflicting PCI bus ids: got nil error, want failure")
	}
}

func TestActivePartitions(t *testing.T) {
	client := &fakeClient{partitions: []Partition{
		{ID: 1, GPUs: pciGPUs(1, 2, 3, 4)},
		{ID: 2, GPUs: pciGPUs(1, 2)},
		{ID: 3, GPUs: pciGPUs(3, 4)},
	}}
	m, err := Open(client)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	active, err := m.ActivePartitions()
	if err != nil {
		t.Fatalf("ActivePartitions: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("ActivePartitions before any activation = %v, want none", active)
	}

	if err := m.ActivatePartition(2); err != nil {
		t.Fatalf("ActivatePartition: %v", err)
	}

	// The result must come from a fresh query, not the topology recorded at Open
	// time -- activation state changes underneath the Manager.
	active, err = m.ActivePartitions()
	if err != nil {
		t.Fatalf("ActivePartitions: %v", err)
	}
	if len(active) != 1 || active[0].ID != 2 {
		t.Fatalf("ActivePartitions after activating 2 = %v, want just partition 2", active)
	}
}

func TestActivePartitionsClientError(t *testing.T) {
	client := &fakeClient{partitions: designDocPartitions()}
	m, err := Open(client)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	client.listErr = errors.New("boom")
	if _, err := m.ActivePartitions(); err == nil {
		t.Fatal("ActivePartitions with a failing client: got nil error, want failure")
	}
}
