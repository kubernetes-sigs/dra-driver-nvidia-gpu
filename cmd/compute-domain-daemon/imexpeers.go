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
	"bufio"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

// imexDomainQueryTimeout bounds the `nvidia-imex-ctl -N` call made by the
// readiness check. It is shorter than the probe's timeoutSeconds so that a
// slow domain query is treated as inconclusive rather than as a probe failure.
const imexDomainQueryTimeout = 5 * time.Second

// imexDisabledLinkStates maps the connectivity table states in which IMEX has
// stopped talking to a peer to a human readable reason. These do not recover
// on their own: the daemons keep rejecting each other until the mismatch is
// fixed. Transient states (N, R, D, A, I, !R!) are deliberately not listed.
var imexDisabledLinkStates = map[string]string{
	"!V!": "version mismatch",
	"!M!": "node map mismatch",
	"!A!": "authentication error",
}

var (
	// Example: `Node #0   * 10.76.179.115 *    - READY    - Version: 580.00`.
	// The asterisks mark the node that nvidia-imex-ctl was run from.
	imexNodeLineRe    = regexp.MustCompile(`^\s*Node #(\d+)\s+([-*])\s*([^\s*]+)`)
	imexNodeVersionRe = regexp.MustCompile(`Version:\s*(\S*)`)
	imexMatrixRowRe   = regexp.MustCompile(`^\s*(\d+)\s+(\S.*)$`)
)

type imexNode struct {
	address string
	version string
}

// imexDisabledPeer is a peer that the local IMEX daemon cannot use, as seen
// in the domain connectivity table.
type imexDisabledPeer struct {
	index  int
	node   imexNode
	reason string
}

func (p imexDisabledPeer) String() string {
	s := fmt.Sprintf("node %d (%s): %s", p.index, p.node.address, p.reason)
	if p.node.version != "" {
		s += fmt.Sprintf(", peer version %s", p.node.version)
	}
	return s
}

// imexDomainStatus is the parsed output of `nvidia-imex-ctl -N`.
type imexDomainStatus struct {
	nodes map[int]imexNode
	// local is the index of the node that nvidia-imex-ctl ran on, or -1.
	local int
	// links[from][to] is the connectivity state reported for that pair.
	links map[int]map[int]string
}

// parseIMEXDomainStatus parses the human readable `nvidia-imex-ctl -N`
// output. If the output contains more than one snapshot (as `-n` does), the
// last one wins.
func parseIMEXDomainStatus(output string) (*imexDomainStatus, error) {
	var status *imexDomainStatus
	var columns []int
	inMatrix := false

	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		switch {
		case trimmed == "Nodes:":
			status = &imexDomainStatus{
				nodes: map[int]imexNode{},
				local: -1,
				links: map[int]map[int]string{},
			}
			columns = nil
			inMatrix = false
			continue
		case status == nil:
			continue
		case strings.HasPrefix(trimmed, `Nodes From\To`):
			columns = columns[:0]
			for _, field := range strings.Fields(strings.TrimPrefix(trimmed, `Nodes From\To`)) {
				col, err := strconv.Atoi(field)
				if err != nil {
					return nil, fmt.Errorf("unexpected connectivity table header %q", trimmed)
				}
				columns = append(columns, col)
			}
			inMatrix = true
			continue
		case strings.HasPrefix(trimmed, "Domain State:"):
			inMatrix = false
			continue
		}

		if inMatrix {
			m := imexMatrixRowRe.FindStringSubmatch(line)
			if m == nil {
				inMatrix = false
				continue
			}
			from, _ := strconv.Atoi(m[1])
			cells := strings.Fields(m[2])
			if len(cells) != len(columns) {
				return nil, fmt.Errorf("connectivity table row %d has %d cells, expected %d", from, len(cells), len(columns))
			}
			row := make(map[int]string, len(cells))
			for i, cell := range cells {
				// In monitoring mode, changed cells are wrapped in asterisks.
				row[columns[i]] = strings.Trim(cell, "*")
			}
			status.links[from] = row
			continue
		}

		if m := imexNodeLineRe.FindStringSubmatch(line); m != nil {
			index, _ := strconv.Atoi(m[1])
			node := imexNode{address: m[3]}
			if v := imexNodeVersionRe.FindStringSubmatch(line); v != nil {
				node.version = v[1]
			}
			status.nodes[index] = node
			if m[2] == "*" {
				status.local = index
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading nvidia-imex-ctl output: %w", err)
	}
	if status == nil || len(status.links) == 0 {
		return nil, errors.New("no IMEX domain connectivity table found")
	}
	return status, nil
}

// disabledPeers returns the peers that the local node has disabled
// communication with, in either direction. A link that is broken between two
// other nodes does not make the local node unusable, so it is not reported.
func (s *imexDomainStatus) disabledPeers() ([]imexDisabledPeer, error) {
	if s.local < 0 {
		return nil, errors.New("local node not marked in nvidia-imex-ctl output")
	}

	var peers []imexDisabledPeer
	for peer := range s.nodes {
		if peer == s.local {
			continue
		}
		reason, ok := imexDisabledLinkStates[s.links[s.local][peer]]
		if !ok {
			reason, ok = imexDisabledLinkStates[s.links[peer][s.local]]
		}
		if ok {
			peers = append(peers, imexDisabledPeer{index: peer, node: s.nodes[peer], reason: reason})
		}
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].index < peers[j].index })
	return peers, nil
}

// checkIMEXPeers fails if the local IMEX daemon has disabled communication
// with any peer in its domain, e.g. because the peer runs a different driver
// version. In that state every daemon still reports READY via `-q`, but any
// workload that spans the affected nodes hangs.
//
// Only a positive detection fails the check. If the domain query itself fails
// or its output cannot be interpreted, this is logged and the check passes, so
// that readiness never gets stricter than the local daemon check because of
// an nvidia-imex-ctl version that prints something unexpected.
func checkIMEXPeers(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, imexDomainQueryTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, imexCtlBinaryName, "-c", imexDaemonConfigPath, "-N")
	out, err := cmd.CombinedOutput()
	if err != nil {
		klog.V(1).Infof("skip IMEX peer check: %s -N failed (%s), stdout/err: %s", imexCtlBinaryName, err, out)
		return nil
	}
	return evaluateIMEXPeers(string(out))
}

func evaluateIMEXPeers(output string) error {
	status, err := parseIMEXDomainStatus(output)
	if err != nil {
		klog.V(1).Infof("skip IMEX peer check: %s", err)
		return nil
	}
	peers, err := status.disabledPeers()
	if err != nil {
		klog.V(1).Infof("skip IMEX peer check: %s", err)
		return nil
	}
	if len(peers) == 0 {
		return nil
	}

	descriptions := make([]string, 0, len(peers))
	for _, p := range peers {
		descriptions = append(descriptions, p.String())
	}
	local := status.nodes[status.local]
	return fmt.Errorf("IMEX daemon (version %s) has disabled communication with %d peer(s): %s",
		local.version, len(peers), strings.Join(descriptions, "; "))
}
