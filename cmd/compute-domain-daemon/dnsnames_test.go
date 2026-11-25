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
	"os"
	"path/filepath"
	"strings"
	"testing"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

func newTestManager(t *testing.T, maxNodes int) *DNSNameManager {
	t.Helper()
	return NewDNSNameManager("clique-a", maxNodes, filepath.Join(t.TempDir(), "nodes.cfg"), "test-cd-uid")
}

func TestBuildDNSNameMappings(t *testing.T) {
	m := newTestManager(t, 4)
	format := m.dnsNameFormat()

	t.Run("empty daemon list fills every slot with the sentinel", func(t *testing.T) {
		mappings, err := m.buildDNSNameMappings(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(mappings) != 4 {
			t.Fatalf("expected 4 mappings, got %d", len(mappings))
		}
		for i, mapping := range mappings {
			wantName := fmt.Sprintf(format, i)
			if mapping.dnsName != wantName {
				t.Errorf("slot %d: dnsName = %q, want %q", i, mapping.dnsName, wantName)
			}
			if mapping.ip != sentinelIPAddress {
				t.Errorf("slot %d: ip = %q, want sentinel %q", i, mapping.ip, sentinelIPAddress)
			}
		}
	})

	t.Run("occupied slots get their daemon's IP, others stay sentinel", func(t *testing.T) {
		daemons := []*nvapi.ComputeDomainDaemonInfo{
			{NodeName: "node-1", CliqueID: "clique-a", Index: 0, IPAddress: "10.0.0.1"},
			{NodeName: "node-2", CliqueID: "clique-a", Index: 2, IPAddress: "10.0.0.2"},
		}
		mappings, err := m.buildDNSNameMappings(daemons)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := map[int]string{0: "10.0.0.1", 1: sentinelIPAddress, 2: "10.0.0.2", 3: sentinelIPAddress}
		for i, wantIP := range want {
			if mappings[i].ip != wantIP {
				t.Errorf("slot %d: ip = %q, want %q", i, mappings[i].ip, wantIP)
			}
		}
	})

	t.Run("daemons from a different clique are ignored", func(t *testing.T) {
		daemons := []*nvapi.ComputeDomainDaemonInfo{
			{NodeName: "other", CliqueID: "clique-b", Index: 0, IPAddress: "10.0.0.9"},
		}
		mappings, err := m.buildDNSNameMappings(daemons)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if mappings[0].ip != sentinelIPAddress {
			t.Errorf("slot 0: ip = %q, want sentinel %q (daemon is in a different clique)", mappings[0].ip, sentinelIPAddress)
		}
	})

	t.Run("out-of-range index is an error", func(t *testing.T) {
		daemons := []*nvapi.ComputeDomainDaemonInfo{
			{NodeName: "node-1", CliqueID: "clique-a", Index: 4, IPAddress: "10.0.0.1"},
		}
		if _, err := m.buildDNSNameMappings(daemons); err == nil {
			t.Error("expected an error for an out-of-range index, got nil")
		}
	})

	t.Run("negative index is an error", func(t *testing.T) {
		daemons := []*nvapi.ComputeDomainDaemonInfo{
			{NodeName: "node-1", CliqueID: "clique-a", Index: -1, IPAddress: "10.0.0.1"},
		}
		if _, err := m.buildDNSNameMappings(daemons); err == nil {
			t.Error("expected an error for a negative index, got nil")
		}
	})

	t.Run("deterministic across repeated calls with the same input", func(t *testing.T) {
		daemons := []*nvapi.ComputeDomainDaemonInfo{
			{NodeName: "node-1", CliqueID: "clique-a", Index: 1, IPAddress: "10.0.0.5"},
		}
		first, err := m.buildDNSNameMappings(daemons)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		second, err := m.buildDNSNameMappings(daemons)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for i := range first {
			if first[i] != second[i] {
				t.Errorf("slot %d differs between calls: %+v vs %+v", i, first[i], second[i])
			}
		}
	})
}

// TestHostsFileMatchesNodesConfig is the byte-for-byte consistency check
// that would have caught the historical bug where buildDNSNameMappings and
// WriteNodesConfig disagreed on the DNS name format (one used the per-domain
// hashed name, the other a plain, unhashed one) -- every name IMEX looks up
// via nodes.cfg must resolve via the exact same string in /etc/hosts.
func TestHostsFileMatchesNodesConfig(t *testing.T) {
	dir := t.TempDir()
	nodesConfigPath := filepath.Join(dir, "nodes.cfg")
	m := NewDNSNameManager("clique-a", 3, nodesConfigPath, "test-cd-uid")

	if err := m.WriteNodesConfig(); err != nil {
		t.Fatalf("WriteNodesConfig: %v", err)
	}
	nodesConfigBytes, err := os.ReadFile(nodesConfigPath)
	if err != nil {
		t.Fatalf("failed to read nodes config: %v", err)
	}
	var nodesConfigNames []string
	for _, line := range strings.Split(strings.TrimSpace(string(nodesConfigBytes)), "\n") {
		if line != "" {
			nodesConfigNames = append(nodesConfigNames, line)
		}
	}

	mappings, err := m.buildDNSNameMappings(nil)
	if err != nil {
		t.Fatalf("buildDNSNameMappings: %v", err)
	}
	if len(mappings) != len(nodesConfigNames) {
		t.Fatalf("nodes.cfg has %d names, /etc/hosts mapping has %d", len(nodesConfigNames), len(mappings))
	}
	for i, mapping := range mappings {
		if mapping.dnsName != nodesConfigNames[i] {
			t.Errorf("slot %d: hosts file name %q does not match nodes.cfg name %q", i, mapping.dnsName, nodesConfigNames[i])
		}
	}
}

func TestDNSNameFormatIncludesDomainHash(t *testing.T) {
	m := newTestManager(t, 1)
	format := m.dnsNameFormat()
	if !strings.HasPrefix(format, dnsNamePrefix) {
		t.Errorf("format %q does not start with prefix %q", format, dnsNamePrefix)
	}
	if !strings.Contains(format, m.domainHash) {
		t.Errorf("format %q does not contain this manager's domain hash %q", format, m.domainHash)
	}
}
