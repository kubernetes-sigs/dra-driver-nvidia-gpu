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

// gpuIdentity holds the available identifiers for a physical GPU, combining
// consistent information from its appearances in different FM partitions.
type gpuIdentity struct {
	uuid string
	pci  string
}

// PhysicalIDForGPU resolves a GPU's identity using indexes validated when the
// FM topology was recorded. UUID and PCI address must agree when both are known.
// When FM omits identities for the entire topology (H100 and later), use its
// documented physical ID == NVML module ID contract instead.
// https://docs.nvidia.com/datacenter/tesla/fabric-manager-user-guide/index.html#data-structures
func (m *Manager) PhysicalIDForGPU(uuid, pciBusID string, moduleID int) (int, bool, error) {
	pci, err := normalizePCIBusID(pciBusID)
	if err != nil {
		return 0, false, err
	}
	// Do not use numeric coincidence to override missing or conflicting
	// identities in an identity-bearing topology such as HGX A100.
	if len(m.physicalIDByUUID) == 0 && len(m.physicalIDByPCI) == 0 {
		if _, found := m.identityByPhysicalID[moduleID]; moduleID > 0 && found {
			return moduleID, true, nil
		}
		return 0, false, nil
	}

	uuidID, uuidFound := m.physicalIDByUUID[uuid]
	pciID, pciFound := m.physicalIDByPCI[pci]
	if uuidFound && pciFound && uuidID != pciID {
		return 0, false, fmt.Errorf("fabricmanager: GPU UUID %q PCI %q conflicts: physical IDs %d and %d", uuid, pciBusID, uuidID, pciID)
	}
	physicalID := uuidID
	if !uuidFound {
		if !pciFound {
			return 0, false, nil
		}
		physicalID = pciID
	}
	identity := m.identityByPhysicalID[physicalID]
	if (uuid != "" && identity.uuid != "" && uuid != identity.uuid) || (pci != "" && identity.pci != "" && pci != identity.pci) {
		return 0, false, fmt.Errorf("fabricmanager: GPU UUID %q PCI %q conflicts with physical ID %d UUID %q PCI %q", uuid, pciBusID, physicalID, identity.uuid, identity.pci)
	}
	return physicalID, true, nil
}
