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
	"regexp"
	"strconv"
	"strings"
)

var pciBusIDPattern = regexp.MustCompile(`^([0-9a-f]{4}|[0-9a-f]{8}):([0-9a-f]{2}):([01][0-9a-f])\.([0-7])$`)

// normalizePCIBusID accepts both the FM/NVML eight-digit domain and the
// sysfs four-digit domain without discarding significant domain bits.
func normalizePCIBusID(pci string) (string, error) {
	if pci == "" {
		return "", nil
	}
	parts := pciBusIDPattern.FindStringSubmatch(strings.ToLower(pci))
	if parts == nil {
		return "", fmt.Errorf("invalid PCI bus ID %q", pci)
	}
	domain, err := strconv.ParseUint(parts[1], 16, 32)
	if err != nil {
		return "", fmt.Errorf("invalid PCI domain in %q: %w", pci, err)
	}
	return fmt.Sprintf("%04x:%s:%s.%s", domain, parts[2], parts[3], parts[4]), nil
}

// PhysicalIDForGPU resolves a GPU's identity to FM's physical ID. NVML's
// platform module ID is a different identifier on some systems, including
// HGX A100. Either UUID or PCI address may identify a member, but when both
// are available they must agree. Repeated members across partitions are normal.
// When FM omits identities for the entire topology (H100 and later), use its
// documented physical ID == NVML module ID contract instead.
// https://docs.nvidia.com/datacenter/tesla/fabric-manager-user-guide/index.html#data-structures
func (m *Manager) PhysicalIDForGPU(uuid, pciBusID string, moduleID int) (int, bool, error) {
	pci, err := normalizePCIBusID(pciBusID)
	if err != nil {
		return 0, false, err
	}
	var physicalID int
	found := false
	hasIdentities := false
	moduleFound := false
	for _, partition := range m.partitionsByID {
		for _, gpu := range partition.GPUs {
			memberPCI, err := normalizePCIBusID(gpu.PCIBusID)
			if err != nil {
				return 0, false, fmt.Errorf("fabricmanager: partition %d physical ID %d: %w", partition.ID, gpu.PhysicalID, err)
			}
			// Some FM versions represent an unavailable BDF as all zeroes.
			if gpu.UUID == "" && memberPCI == "0000:00:00.0" {
				memberPCI = ""
			}
			if gpu.UUID != "" || memberPCI != "" {
				hasIdentities = true
			}
			if moduleID > 0 && gpu.PhysicalID == moduleID {
				moduleFound = true
			}
			uuidMatch := uuid != "" && uuid == gpu.UUID
			pciMatch := pci != "" && pci == memberPCI
			if !uuidMatch && !pciMatch {
				continue
			}
			if (uuid != "" && gpu.UUID != "" && !uuidMatch) || (pci != "" && memberPCI != "" && !pciMatch) {
				return 0, false, fmt.Errorf("fabricmanager: GPU UUID %q PCI %q conflicts with partition %d member UUID %q PCI %q",
					uuid, pciBusID, partition.ID, gpu.UUID, gpu.PCIBusID)
			}
			if found && physicalID != gpu.PhysicalID {
				return 0, false, fmt.Errorf("fabricmanager: GPU UUID %q PCI %q maps to multiple physical IDs (%d and %d)",
					uuid, pciBusID, physicalID, gpu.PhysicalID)
			}
			physicalID, found = gpu.PhysicalID, true
		}
	}
	// Do not use numeric coincidence to override missing or conflicting
	// identities in an identity-bearing topology such as HGX A100.
	if !hasIdentities && moduleFound {
		return moduleID, true, nil
	}
	return physicalID, found, nil
}
