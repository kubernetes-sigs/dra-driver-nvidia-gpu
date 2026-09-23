/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/klog/v2"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

const (
	hostsFilePath = "/etc/hosts"
	dnsNamePrefix = "compute-domain-daemon-"

	// sentinelIPAddress is used in the generated DNS name mapping for any
	// maxNodesPerIMEXDomain slot that doesn't currently have a live daemon
	// registered to it.
	//
	// Giving every slot a hosts file entry keeps resolution local: it
	// always succeeds via the "files" NSS source before DNS is ever
	// consulted, and connecting to a loopback address nothing listens on
	// fails immediately (a local ECONNREFUSED, no network round-trip)
	// which is exactly the right outcome for a slot with no daemon in it
	// yet.
	sentinelIPAddress = "127.0.0.2"
)

// dnsNameMapping pairs a DNS name with the address it currently resolves to
// in /etc/hosts, either a live daemon's pod IP, or sentinelIPAddress for a
// maxNodesPerIMEXDomain slot with no daemon registered to it yet.
type dnsNameMapping struct {
	dnsName string
	ip      string
}

// DNSNameManager manages the allocation of static DNS names to IP addresses.
type DNSNameManager struct {
	sync.Mutex
	mappings              []dnsNameMapping
	cliqueID              string
	maxNodesPerIMEXDomain int
	nodesConfigPath       string
	domainHash            string
}

// NewDNSNameManager creates a new DNS name manager.
func NewDNSNameManager(cliqueID string, maxNodesPerIMEXDomain int, nodesConfigPath string, cdUID string) *DNSNameManager {
	return &DNSNameManager{
		cliqueID:              cliqueID,
		maxNodesPerIMEXDomain: maxNodesPerIMEXDomain,
		nodesConfigPath:       nodesConfigPath,
		domainHash:            computeDomainHash(cdUID),
	}
}

// computeDomainHash returns a short, deterministic, DNS-label-safe hash of a
// ComputeDomain UID, used to fold ComputeDomain identity into generated DNS
// names without depending on the UID's own format or length.
func computeDomainHash(cdUID string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(cdUID))
	return rand.SafeEncodeString(fmt.Sprint(h.Sum32()))
}

// dnsNameFormat returns this manager's per-domain DNS name format string,
// e.g. "compute-domain-daemon-bcs2h4gtp8-%04d".
func (m *DNSNameManager) dnsNameFormat() string {
	return dnsNamePrefix + m.domainHash + "-%04d"
}

// UpdateDNSNameMappings updates the /etc/hosts file with any new IP to DNS name
// mappings. The boolean return value indicates whether the hosts file was
// updated or not (it must be ignored when the returned error is non-nil).
func (m *DNSNameManager) UpdateDNSNameMappings(daemons []*nvapi.ComputeDomainDaemonInfo) (bool, error) {
	m.Lock()
	defer m.Unlock()

	mappings, err := m.buildDNSNameMappings(daemons)
	if err != nil {
		return false, err
	}

	// If the existing mappings are unchanged, exit early
	if slices.Equal(mappings, m.mappings) {
		return false, nil
	}

	// Otherwise, update the cached mappings
	m.mappings = mappings

	// And update the hosts file with the new mapping
	return true, m.updateHostsFile()
}

// buildDNSNameMappings builds one dnsNameMapping per maxNodesPerIMEXDomain
// slot, in index order: a slot with a registered daemon in this clique maps
// its DNS name to that daemon's pod IP, and every other slot maps to
// sentinelIPAddress.
func (m *DNSNameManager) buildDNSNameMappings(daemons []*nvapi.ComputeDomainDaemonInfo) ([]dnsNameMapping, error) {
	// Index live daemons in this clique by their slot index.
	byIndex := make(map[int]*nvapi.ComputeDomainDaemonInfo)
	for _, daemon := range daemons {
		if daemon.CliqueID != m.cliqueID {
			continue
		}
		if daemon.Index < 0 || daemon.Index >= m.maxNodesPerIMEXDomain {
			return nil, fmt.Errorf("daemon %s has invalid index %d, must be in [0, %d)", daemon.NodeName, daemon.Index, m.maxNodesPerIMEXDomain)
		}
		if existing, exists := byIndex[daemon.Index]; exists {
			return nil, fmt.Errorf("multiple daemons registered at index %d in clique %q (%s and %s)", daemon.Index, m.cliqueID, existing.NodeName, daemon.NodeName)
		}
		byIndex[daemon.Index] = daemon
	}

	// Use this manager's own per-domain (hash-scoped) name format for every
	// slot, so nodes.cfg (WriteNodesConfig) and /etc/hosts (this function)
	// can never disagree on what a given slot's DNS name is.
	format := m.dnsNameFormat()
	mappings := make([]dnsNameMapping, m.maxNodesPerIMEXDomain)
	for i := 0; i < m.maxNodesPerIMEXDomain; i++ {
		ip := sentinelIPAddress
		if daemon, ok := byIndex[i]; ok {
			ip = daemon.IPAddress
		}
		mappings[i] = dnsNameMapping{
			dnsName: fmt.Sprintf(format, i),
			ip:      ip,
		}
	}

	return mappings, nil
}

// LogDNSNameMappings logs the current compute-domain-daemon mappings from memory.
func (m *DNSNameManager) LogDNSNameMappings() {
	m.Lock()
	defer m.Unlock()

	if len(m.mappings) == 0 {
		klog.V(2).Infof("Current compute-domain-daemon mappings: empty")
		return
	}

	// Already in ascending index order from buildDNSNameMappings, which for
	// the zero-padded per-domain DNS name format is also ascending DNS-name order.
	for _, mapping := range m.mappings {
		klog.V(2).Infof("%s -> %s", mapping.dnsName, mapping.ip)
	}
}

// updateHostsFile updates the /etc/hosts file with current IP to DNS name mappings.
func (m *DNSNameManager) updateHostsFile() error {
	// Read hosts file
	hostsContent, err := os.ReadFile(hostsFilePath)
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", hostsFilePath, err)
	}

	// Grab any lines to preserve, skipping existing DNS name mappings
	var preservedLines []string
	for _, line := range strings.Split(string(hostsContent), "\n") {
		line = strings.TrimSpace(line)

		// Skip existing compute-domain-daemon mappings
		if strings.Contains(line, dnsNamePrefix) {
			continue
		}

		// Keep all other lines
		preservedLines = append(preservedLines, line)
	}

	// Add preserved lines
	var newHostsContent strings.Builder
	for _, line := range preservedLines {
		newHostsContent.WriteString(line)
		newHostsContent.WriteString("\n")
	}

	// Add new DNS name mappings. Every maxNodesPerIMEXDomain slot gets an
	// entry: occupied slots resolve to their daemon's real IP, the rest
	// resolve to sentinelIPAddress so every name IMEX may try to resolve
	// is always satisfied locally.
	for _, mapping := range m.mappings {
		_, _ = fmt.Fprintf(&newHostsContent, "%s\t%s\n", mapping.ip, mapping.dnsName)
	}

	// Write the updated hosts file
	if err := os.WriteFile(hostsFilePath, []byte(newHostsContent.String()), 0644); err != nil {
		return fmt.Errorf("failed to write %s: %w", hostsFilePath, err)
	}

	return nil
}

// WriteNodesConfig creates a static nodes config file with DNS names.
func (m *DNSNameManager) WriteNodesConfig() error {
	// Ensure the directory exists
	dir := filepath.Dir(m.nodesConfigPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	// Create or overwrite the nodesConfig file
	f, err := os.Create(m.nodesConfigPath)
	if err != nil {
		return fmt.Errorf("failed to create nodes config file: %w", err)
	}
	defer f.Close()

	// Write static DNS names
	format := m.dnsNameFormat()
	for i := 0; i < m.maxNodesPerIMEXDomain; i++ {
		dnsName := fmt.Sprintf(format, i)
		if _, err := fmt.Fprintf(f, "%s\n", dnsName); err != nil {
			return fmt.Errorf("failed to write to nodes config file: %w", err)
		}
	}

	klog.Infof("Created static nodes config file with %d DNS names using format %s", m.maxNodesPerIMEXDomain, format)

	return nil
}
