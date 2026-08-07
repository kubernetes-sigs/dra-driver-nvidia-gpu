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
	"fmt"
	"os"

	"k8s.io/klog/v2"
)

// Fabric Manager connection environment variables and their defaults.
const (
	// fmAddressEnvvar selects the FM TCP transport and sets the host to
	// connect to. When this variable is set (even to ""), TCP is used and an
	// empty value falls back to defaultFMAddress.
	fmAddressEnvvar = "NVIDIA_FABRICMANAGER_ADDRESS"
	// fmUnixSocketEnvvar overrides the unix socket path used when TCP is not
	// selected. An empty value falls back to defaultFMUnixSocket.
	fmUnixSocketEnvvar = "NVIDIA_FABRICMANAGER_UNIX_SOCKET"

	defaultFMAddress    = "127.0.0.1"
	defaultFMUnixSocket = "/driver-root/run/nvidia-fabricmanager/socket"
)

type Manager struct {
	client         Client
	partitionsByID map[int]Partition
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
	}

	if err := client.Init(); err != nil {
		return nil, fmt.Errorf("fabricmanager: fmLibInit: %w", err)
	}

	partitions, err := client.GetSupportedFabricPartitions()
	if err != nil {
		_ = client.Shutdown()
		return nil, fmt.Errorf("fabricmanager: fmGetSupportedFabricPartitions: %w. \n NOTE: Ensure FABRIC_MODE is correctly set", err)
	}

	if err := m.recordsPartitions(partitions); err != nil {
		_ = client.Shutdown()
		return nil, err
	}

	return m, nil
}

// OpenFabricManager builds an FM Manager backed by go-nvfm, using the FM client
// library at libPath. The connection transport and address are resolved from
// the NVIDIA_FABRICMANAGER_* environment variables.
func OpenFabricManager(libPath string) (*Manager, error) {
	params := ConnectParams{}
	if addr, ok := os.LookupEnv(fmAddressEnvvar); ok {
		if addr == "" {
			addr = defaultFMAddress
		}
		params.AddressInfo = addr
	} else {
		socket := defaultFMUnixSocket
		if v, ok := os.LookupEnv(fmUnixSocketEnvvar); ok && v != "" {
			socket = v
		}
		params.AddressInfo = socket
		params.AddressIsUnixSocket = true
	}

	client := NewClient(libPath, params)

	m, err := Open(client)
	if err != nil {
		return nil, fmt.Errorf("fabric manager not available: %w", err)
	}

	klog.Infof("Fabric Manager connection established; FM partition attributes enabled")
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

// recordsPartitions records the FM-supplied partition topology, validating
// that every partition is well-formed (unique id, non-empty, no duplicate GPU
// members).
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
				return fmt.Errorf("fabricmanager: partition %d references gpuModuleID %d twice",
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
// that includes the given gpuModuleID. e.g.:
//
//	gpuModuleID: 1
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
						"fabricmanager: gpuModuleID %d appears in two partitions of size %d (%d and %d)",
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
// GPU member set is exactly equal to the given set of gpuModuleIDs, or
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

// ActivatePartition asks Fabric Manager to program the NVSwitch fabric for the
// given partition. It is idempotent: if FM already reports the partition as
// active (e.g. on a retried Prepare, or after a driver restart), it returns
// nil without re-activating.
func (m *Manager) ActivatePartition(partitionID int) error {
	if err := m.checkKnownPartition(partitionID); err != nil {
		return err
	}
	activated, err := m.isPartitionActivated(partitionID)
	if err != nil {
		return fmt.Errorf("fabricmanager: resolving partition %d activation state: %w", partitionID, err)
	}
	if activated {
		klog.V(4).Infof("fabricmanager: partition %d already active; skipping activation", partitionID)
		return nil
	}
	if err := m.client.ActivateFabricPartition(partitionID); err != nil {
		return fmt.Errorf("fabricmanager: fmActivateFabricPartition(%d): %w", partitionID, err)
	}
	return nil
}

// DeactivatePartition releases an activated partition. It should be called
// when the corresponding DRA claim is released. It is idempotent: if FM
// already reports the partition as inactive, it returns nil without calling
// FM.
func (m *Manager) DeactivatePartition(partitionID int) error {
	if err := m.checkKnownPartition(partitionID); err != nil {
		return err
	}
	activated, err := m.isPartitionActivated(partitionID)
	if err != nil {
		return fmt.Errorf("fabricmanager: resolving partition %d activation state: %w", partitionID, err)
	}
	if !activated {
		klog.V(4).Infof("fabricmanager: partition %d already inactive; skipping deactivation", partitionID)
		return nil
	}
	if err := m.client.DeactivateFabricPartition(partitionID); err != nil {
		return fmt.Errorf("fabricmanager: fmDeactivateFabricPartition(%d): %w", partitionID, err)
	}
	return nil
}

// isPartitionActivated reports whether FM currently considers the given
// partition active.
func (m *Manager) isPartitionActivated(partitionID int) (bool, error) {
	partitions, err := m.client.GetSupportedFabricPartitions()
	if err != nil {
		return false, fmt.Errorf("fabricmanager: fmGetSupportedFabricPartitions: %w", err)
	}
	for _, p := range partitions {
		if p.ID == partitionID {
			return p.IsActive, nil
		}
	}
	return false, nil
}

// checkKnownPartition returns an error if partitionID was not reported by FM at
// Open time.
func (m *Manager) checkKnownPartition(partitionID int) error {
	if _, ok := m.GetPartition(partitionID); !ok {
		return fmt.Errorf("fabricmanager: unknown partitionId %d", partitionID)
	}
	return nil
}
