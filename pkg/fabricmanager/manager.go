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
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

// Manager is the in-memory view this package exposes to the rest of the DRA
// driver. It owns:
//
//   - a PCI bus ID <-> gpuModuleId map populated by walking every visible GPU
//   - the list of FM-supported fabric partitions discovered through a Client
//   - a long-lived connection to nv-fabricmanager
type Manager struct {
	mu sync.RWMutex

	client Client
	opened bool

	gpuModuleIDByPCI map[string]int
	pciByGpuModuleID map[int]string

	partitionsByID map[int]Partition

	// activated tracks the set of partitions Activate has been called on
	// (and Deactivate has not yet undone).
	activated map[int]struct{}
}

// NVMLDeviceLister is the minimal subset of nvml.Interface required to build
// the gpuModuleId <-> PCI bus ID mapping.
type NVMLDeviceLister interface {
	DeviceGetCount() (int, nvml.Return)
	DeviceGetHandleByIndex(int) (nvml.Device, nvml.Return)
}

// Open builds a Manager and leaves a long-lived FM connection in place so the
// caller can subsequently activate and deactivate partitions. It:
//
//  1. Walks every GPU visible to NVML and records (PCI bus ID, gpuModuleId).
//  2. Calls Init+Connect on the FM client.
//  3. Calls GetSupportedFabricPartitions, augments the (PCI bus ID,
//     gpuModuleId) map with any GPUs only FM can see (e.g. GPUs already bound
//     to vfio-pci for passthrough, invisible to NVML), and records the FM-supported partitions.
//

func Open(lib NVMLDeviceLister, client Client, params ConnectParams) (*Manager, error) {
	if lib == nil {
		return nil, fmt.Errorf("fabricmanager: nil NVML interface")
	}
	if client == nil {
		return nil, fmt.Errorf("fabricmanager: nil FM client")
	}

	m := &Manager{
		client:           client,
		gpuModuleIDByPCI: make(map[string]int),
		pciByGpuModuleID: make(map[int]string),
		partitionsByID:   make(map[int]Partition),
		activated:        make(map[int]struct{}),
	}

	if err := m.pciIdToGpuModuleIdMap(lib); err != nil {
		return nil, fmt.Errorf("fabricmanager: pciIdToGpuModuleIdMap: %w", err)
	}

	if err := client.Init(); err != nil {
		return nil, fmt.Errorf("fabricmanager: fmLibInit: %w", err)
	}
	if err := client.Connect(params); err != nil {
		_ = client.Shutdown()
		return nil, fmt.Errorf("fabricmanager: fmConnect(%q): %w", params.AddressInfo, err)
	}

	partitions, err := client.GetSupportedFabricPartitions()
	if err != nil {
		_ = client.Disconnect()
		_ = client.Shutdown()
		return nil, fmt.Errorf("fabricmanager: fmGetSupportedFabricPartitions: %w", err)
	}

	if err := m.recordsPartitions(partitions); err != nil {
		_ = client.Disconnect()
		_ = client.Shutdown()
		return nil, err
	}

	// Seed activated set from any partitions FM already reports as active
	// (e.g. after a DRA driver restart with FM still running).
	for _, p := range partitions {
		if p.IsActive {
			m.activated[p.ID] = struct{}{}
		}
	}

	m.opened = true
	return m, nil
}

// Close tears down the Manager's FM connection.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.opened {
		return nil
	}
	m.opened = false
	client := m.client

	var firstErr error
	if err := client.Disconnect(); err != nil {
		firstErr = fmt.Errorf("fabricmanager: fmDisconnect: %w", err)
	}
	if err := client.Shutdown(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("fabricmanager: fmLibShutdown: %w", err)
	}
	return firstErr
}

// walkPCIModuleMapping walks every NVML-visible GPU and returns freshly built
// gpuModuleId <-> PCI bus ID maps. Duplicate moduleIds or PCI bus IDs among the
// visible GPUs are reported as errors. GPUs bound to vfio-pci are invisible to
// NVML and therefore absent from the returned maps.
func walkPCIModuleMapping(lib NVMLDeviceLister) (map[string]int, map[int]string, error) {
	count, ret := lib.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return nil, nil, fmt.Errorf("fabricmanager: NVML DeviceGetCount: %v", ret)
	}

	gpuModuleIDByPCI := make(map[string]int, count)
	pciByGpuModuleID := make(map[int]string, count)

	for i := 0; i < count; i++ {
		dev, ret := lib.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			return nil, nil, fmt.Errorf("fabricmanager: NVML DeviceGetHandleByIndex(%d): %v", i, ret)
		}

		moduleID, ret := dev.GetModuleId()
		if ret != nvml.SUCCESS {
			return nil, nil, fmt.Errorf("fabricmanager: NVML GetModuleId for device %d: %v", i, ret)
		}

		pciInfo, ret := dev.GetPciInfo()
		if ret != nvml.SUCCESS {
			return nil, nil, fmt.Errorf("fabricmanager: NVML GetPciInfo for device %d: %v", i, ret)
		}
		pciBusID := normalizePCIBusID(unix.ByteSliceToString(pciInfo.BusId[:]))
		if pciBusID == "" {
			return nil, nil, fmt.Errorf("fabricmanager: empty PCI bus ID for device %d (moduleId=%d)", i, moduleID)
		}

		if existing, ok := pciByGpuModuleID[moduleID]; ok {
			return nil, nil, fmt.Errorf("fabricmanager: duplicate gpuModuleId %d for PCI bus IDs %q and %q",
				moduleID, existing, pciBusID)
		}
		if existing, ok := gpuModuleIDByPCI[pciBusID]; ok {
			return nil, nil, fmt.Errorf("fabricmanager: duplicate PCI bus ID %q for gpuModuleIds %d and %d",
				pciBusID, existing, moduleID)
		}
		gpuModuleIDByPCI[pciBusID] = moduleID
		pciByGpuModuleID[moduleID] = pciBusID
	}
	return gpuModuleIDByPCI, pciByGpuModuleID, nil
}

// pciIdToGpuModuleIdMap populates the gpuModuleId <-> PCI bus ID maps by walking
// every NVML-visible GPU. The maps are replaced atomically so concurrent
// readers always see a consistent snapshot.
func (m *Manager) pciIdToGpuModuleIdMap(lib NVMLDeviceLister) error {
	gpuModuleIDByPCI, pciByGpuModuleID, err := walkPCIModuleMapping(lib)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.gpuModuleIDByPCI = gpuModuleIDByPCI
	m.pciByGpuModuleID = pciByGpuModuleID
	return nil
}

// SetModuleMappingForPCI records the (PCI bus ID, gpuModuleId) pair for a
// single GPU.
//
// This is used after a single GPU is rebound from the vfio-pci driver back to
// the nvidia driver during unprepare, the caller already knows that GPU's PCI
// bus ID and can resolve its gpuModuleId directly from NVML.
func (m *Manager) SetModuleMappingForPCI(pciBusID string, moduleID int) error {
	if err := m.checkOpen(); err != nil {
		return err
	}

	key := normalizePCIBusID(pciBusID)
	if key == "" {
		return fmt.Errorf("fabricmanager: empty PCI bus ID for gpuModuleId %d", moduleID)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// If the freshly observed pair conflicts with an existing entry, overwrite
	// it and prune the now-stale reverse mapping so both maps stay consistent.
	if existing, ok := m.gpuModuleIDByPCI[key]; ok && existing != moduleID {
		klog.Warningf("fabricmanager: PCI bus ID %q was mapped to gpuModuleId %d but NVML now reports %d; updating",
			key, existing, moduleID)
		delete(m.pciByGpuModuleID, existing)
	}
	if existing, ok := m.pciByGpuModuleID[moduleID]; ok && existing != key {
		klog.Warningf("fabricmanager: gpuModuleId %d was mapped to PCI bus ID %q but NVML now reports %q; updating",
			moduleID, existing, key)
		delete(m.gpuModuleIDByPCI, existing)
	}
	m.gpuModuleIDByPCI[key] = moduleID
	m.pciByGpuModuleID[moduleID] = key
	klog.V(2).Infof("fabricmanager: recorded GPU module mapping after rebind to nvidia driver: PCI=%s gpuModuleId=%d",
		key, moduleID)
	return nil
}

// recordsPartitions records the FM-supplied partitions, validating that every
// gpuModuleId referenced by FM corresponds to a GPU present in the module map.
func (m *Manager) recordsPartitions(parts []Partition) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	byID := make(map[int]Partition, len(parts))
	pciByGpuModuleID := m.pciByGpuModuleID

	for _, p := range parts {
		if _, dup := byID[p.ID]; dup {
			return fmt.Errorf("fabricmanager: FM returned duplicate partitionId %d", p.ID)
		}
		if len(p.GPUs) == 0 {
			return fmt.Errorf("fabricmanager: partition %d has no GPUs", p.ID)
		}
		seen := make(map[int]struct{}, len(p.GPUs))
		for _, g := range p.GPUs {
			if _, dup := seen[g.PhysicalID]; dup {
				return fmt.Errorf("fabricmanager: partition %d references gpuModuleId %d twice",
					p.ID, g.PhysicalID)
			}
			seen[g.PhysicalID] = struct{}{}
			if _, known := pciByGpuModuleID[g.PhysicalID]; !known {
				klog.Warningf("fabricmanager: gpuModuleId %d referenced by FM has no resolvable PCI bus ID and will be published without FM attributes", g.PhysicalID)
			}
		}
		byID[p.ID] = p
	}
	m.partitionsByID = byID
	return nil
}

// GetModuleIDByPCI returns the gpuModuleId associated with the given PCI bus
// ID, or false if no GPU on the node matches. Lookup is case-insensitive and
// tolerates both the 4-digit ("0000:3b:00.0") and 8-digit ("00000000:3B:00.0")
// PCI domain forms.
func (m *Manager) GetModuleIDByPCI(pciBusID string) (int, bool) {
	key := normalizePCIBusID(pciBusID)
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.gpuModuleIDByPCI[key]
	return id, ok
}

// GetPCIByModuleID returns the PCI bus ID associated with the given
// gpuModuleId, or false if no GPU on the node matches.
func (m *Manager) GetPCIByModuleID(moduleID int) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	pci, ok := m.pciByGpuModuleID[moduleID]
	return pci, ok
}

// GetPartition returns the FM partition info for the given partitionId, or
// false if no such partition was reported by FM.
func (m *Manager) GetPartition(partitionID int) (Partition, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.partitionsByID[partitionID]
	return p, ok
}

// Partitions returns every FM-supported partition, sorted ascending by id.
func (m *Manager) Partitions() []Partition {
	m.mu.RLock()
	ids := make([]int, 0, len(m.partitionsByID))
	for id := range m.partitionsByID {
		ids = append(ids, id)
	}
	out := make([]Partition, 0, len(ids))
	sort.Ints(ids)
	for _, id := range ids {
		out = append(out, m.partitionsByID[id])
	}
	m.mu.RUnlock()
	return out
}

// GetPartitionsByPCI returns all partitionIds that include the GPU identified
// by the given PCI bus ID. Sorted ascending. Returns (nil, false) if the PCI
// bus ID is unknown to NVML.
func (m *Manager) GetPartitionsByPCI(pciBusID string) ([]int, bool) {
	moduleID, ok := m.GetModuleIDByPCI(pciBusID)
	if !ok {
		return nil, false
	}
	return m.GetPartitionsByModuleID(moduleID), true
}

// GetPartitionsByModuleID returns all partitionIds that include the given
// gpuModuleId. Sorted ascending. Returns an empty (non-nil) slice if no
// partitions reference the module.
func (m *Manager) GetPartitionsByModuleID(moduleID int) []int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var ids []int
	for _, p := range m.partitionsByID {
		for _, g := range p.GPUs {
			if g.PhysicalID == moduleID {
				ids = append(ids, p.ID)
				break
			}
		}
	}
	sort.Ints(ids)
	if ids == nil {
		ids = []int{}
	}
	return ids
}

// GetPartitionsBySizeByModuleID returns a map keyed by partition size (number
// of GPUs in the partition) to the partitionId of the partition of that size
// that includes the given gpuModuleId. e.g.:
//
//	gpuModuleId: 1
//	partition1:  8
//	partition2:  4
//	partition4:  2
//	partition8:  1
//
// On a well-formed node FM produces exactly one partition per
// (size, GPU) pair; if more than one is found this method returns an error
func (m *Manager) GetPartitionsBySizeByModuleID(moduleID int) (map[int]int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make(map[int]int)
	for _, p := range m.partitionsByID {
		size := len(p.GPUs)
		for _, g := range p.GPUs {
			if g.PhysicalID == moduleID {
				if existing, dup := out[size]; dup {
					return nil, fmt.Errorf(
						"fabricmanager: gpuModuleId %d appears in two partitions of size %d (%d and %d)",
						moduleID, size, existing, p.ID)
				}
				out[size] = p.ID
				break
			}
		}
	}
	return out, nil
}

func (m *Manager) GetPartitionsBySizeByPCI(pciBusID string) (map[int]int, bool, error) {
	moduleID, ok := m.GetModuleIDByPCI(pciBusID)
	if !ok {
		return nil, false, nil
	}
	out, err := m.GetPartitionsBySizeByModuleID(moduleID)
	return out, true, err
}

// FindPartitionByModuleIDs returns the partitionId of the FM partition whose
// GPU member set is exactly equal to the given set of gpuModuleIds, or
// (0, false) if no partition matches
func (m *Manager) FindPartitionByModuleIDs(moduleIDs []int) (int, bool) {
	if len(moduleIDs) == 0 {
		return 0, false
	}
	want := make(map[int]struct{}, len(moduleIDs))
	for _, id := range moduleIDs {
		want[id] = struct{}{}
	}


	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, p := range m.partitionsByID {
		if len(p.GPUs) != len(want) {
			continue
		}
		match := true
		for _, g := range p.GPUs {
			if _, ok := want[g.PhysicalID]; !ok {
				match = false
				break
			}
		}
		if match {
			return p.ID, true
		}
	}
	return 0, false
}

// ActivatePartition asks Fabric Manager to program the NVSwitch fabric for
// the given partition.
func (m *Manager) ActivatePartition(partitionID int) error {
	if err := m.checkOpenPartition(partitionID); err != nil {
		return err
	}
	if err := m.client.ActivateFabricPartition(partitionID); err != nil {
		return fmt.Errorf("fabricmanager: fmActivateFabricPartition(%d): %w", partitionID, err)
	}
	m.markActivated(partitionID, true)
	return nil
}

// DeactivatePartition releases an activated partition. It should be called
// when the corresponding DRA claim is released
func (m *Manager) DeactivatePartition(partitionID int) error {
	if err := m.checkOpenPartition(partitionID); err != nil {
		return err
	}
	if err := m.client.DeactivateFabricPartition(partitionID); err != nil {
		return fmt.Errorf("fabricmanager: fmDeactivateFabricPartition(%d): %w", partitionID, err)
	}
	m.markActivated(partitionID, false)
	return nil
}

// ActivatedPartitions returns the set of partition IDs the Manager has
// observed activated (either reported active by FM at Open time or activated
// by this Manager since). Sorted ascending.
func (m *Manager) ActivatedPartitions() []int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]int, 0, len(m.activated))
	for id := range m.activated {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// UnsupportedPartitions returns partitions FM has marked unsupported (e.g.
// because of NVLink failures). On HGX-H100 and later this is always empty.
func (m *Manager) UnsupportedPartitions() ([]UnsupportedPartition, error) {
	if err := m.checkOpen(); err != nil {
		return nil, err
	}
	parts, err := m.client.GetUnsupportedFabricPartitions()
	if err != nil {
		return nil, fmt.Errorf("fabricmanager: fmGetUnsupportedFabricPartitions: %w", err)
	}
	return parts, nil
}

func (m *Manager) checkOpen() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.opened {
		return fmt.Errorf("fabricmanager: manager is closed")
	}
	return nil
}

func (m *Manager) checkOpenPartition(partitionID int) error {
	if err := m.checkOpen(); err != nil {
		return err
	}
	if _, ok := m.GetPartition(partitionID); !ok {
		return fmt.Errorf("fabricmanager: unknown partitionId %d", partitionID)
	}
	return nil
}

func (m *Manager) markActivated(partitionID int, active bool) {
	m.mu.Lock()
	if active {
		m.activated[partitionID] = struct{}{}
	} else {
		delete(m.activated, partitionID)
	}
	if p, ok := m.partitionsByID[partitionID]; ok {
		p.IsActive = active
		m.partitionsByID[partitionID] = p
	}
	m.mu.Unlock()
}

// normalizePCIBusID canonicalize to upper-case with an 8-digit domain. Inputs without a
// domain segment ("3b:00.0") or otherwise unparseable are returned upper-cased
// and trimmed without further mangling so callers still see a stable key.
func normalizePCIBusID(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return s
	}
	domain, rest := parts[0], parts[1]
	switch {
	case len(domain) < 8:
		domain = strings.Repeat("0", 8-len(domain)) + domain
	case len(domain) > 8:
		domain = domain[len(domain)-8:]
	}
	return domain + ":" + rest
}
