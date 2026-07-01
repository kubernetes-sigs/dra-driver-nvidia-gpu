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
)

type Manager struct {
	client Client

	partitionsByID map[int]Partition
	activated      map[int]struct{}
}

// Open builds a Manager for the caller to subsequently activate and deactivate
// partitions. The client carries its own connection parameters (see
// NewClient); each FM operation opens and tears down its own connection.
func Open(client Client) (*Manager, error) {
	if client == nil {
		return nil, fmt.Errorf("fabricmanager: nil FM client")
	}

	m := &Manager{
		client:         client,
		partitionsByID: make(map[int]Partition),
		activated:      make(map[int]struct{}),
	}

	if err := client.Init(); err != nil {
		return nil, fmt.Errorf("fabricmanager: fmLibInit: %w", err)
	}

	partitions, err := client.GetSupportedFabricPartitions()
	if err != nil {
		_ = client.Shutdown()
		return nil, fmt.Errorf("fabricmanager: fmGetSupportedFabricPartitions: %w", err)
	}

	if err := m.recordsPartitions(partitions); err != nil {
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

	return m, nil
}

// Close tears down the Manager's FM library session. It is expected to be
// called exactly once, during driver shutdown, after all Prepare / Unprepare
// activity has stopped.
func (m *Manager) Close() error {
	if err := m.client.Shutdown(); err != nil {
		return fmt.Errorf("fabricmanager: fmLibShutdown: %w", err)
	}
	return nil
}

// recordsPartitions records the FM-supplied partitions, validating that every
// partition is well-formed (unique id, non-empty, no duplicate GPU members).
func (m *Manager) recordsPartitions(parts []Partition) error {
	byID := make(map[int]Partition, len(parts))

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
		}
		byID[p.ID] = p
	}
	m.partitionsByID = byID
	return nil
}

// GetPartition returns the FM partition info for the given partitionId, or
// false if no such partition was reported by FM.
func (m *Manager) GetPartition(partitionID int) (Partition, bool) {
	p, ok := m.partitionsByID[partitionID]
	return p, ok
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
// (size, GPU) pair; if more than one is found this method returns an error.
func (m *Manager) GetPartitionsBySizeByModuleID(moduleID int) (map[int]int, error) {
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

// FindPartitionByModuleIDs returns the partitionId of the FM partition whose
// GPU member set is exactly equal to the given set of gpuModuleIds, or
// (0, false) if no partition matches.
func (m *Manager) FindPartitionByModuleIDs(moduleIDs []int) (int, bool) {
	if len(moduleIDs) == 0 {
		return 0, false
	}
	want := make(map[int]struct{}, len(moduleIDs))
	for _, id := range moduleIDs {
		want[id] = struct{}{}
	}

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
	if err := m.checkKnownPartition(partitionID); err != nil {
		return err
	}
	if err := m.client.ActivateFabricPartition(partitionID); err != nil {
		return fmt.Errorf("fabricmanager: fmActivateFabricPartition(%d): %w", partitionID, err)
	}
	m.markActivated(partitionID, true)
	return nil
}

// DeactivatePartition releases an activated partition. It should be called
// when the corresponding DRA claim is released.
func (m *Manager) DeactivatePartition(partitionID int) error {
	if err := m.checkKnownPartition(partitionID); err != nil {
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
	ids := make([]int, 0, len(m.activated))
	for id := range m.activated {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// checkKnownPartition returns an error if partitionID was not reported by FM at
// Open time.
func (m *Manager) checkKnownPartition(partitionID int) error {
	if _, ok := m.GetPartition(partitionID); !ok {
		return fmt.Errorf("fabricmanager: unknown partitionId %d", partitionID)
	}
	return nil
}

func (m *Manager) markActivated(partitionID int, active bool) {
	if active {
		m.activated[partitionID] = struct{}{}
	} else {
		delete(m.activated, partitionID)
	}
	if p, ok := m.partitionsByID[partitionID]; ok {
		p.IsActive = active
		m.partitionsByID[partitionID] = p
	}
}
