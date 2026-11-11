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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const imexCtlLegend = `Connectivity Table Legend:
I - Invalid - Node wasn't reachable, no connection status available
N - Never Connected
R - Recovering - Connection was lost, but clean up has not yet been triggered.
D - Disconnected - Connection was lost, and clean up has been triggreed.
A - Authenticating - If GSSAPI enabled, client has initiated mutual authentication.
!V! - Version mismatch, communication disabled.
!M! - Node map mismatch, communication disabled.
!A! - Authentication error, communication disabled.
C - Connected - Ready for operation

`

// Healthy 4-node domain, as printed by `nvidia-imex-ctl -N`.
const imexCtlHealthy = imexCtlLegend + `3/18/2025 19:17:50.187
Nodes:
Node #0   - 10.76.179.115      - READY                - Version: 580.178.04
Node #1   * 10.76.183.30 *     - READY                - Version: 580.178.04
Node #2   - 10.76.184.132      - READY                - Version: 580.178.04
Node #3   - 10.76.186.255      - READY                - Version: 580.178.04

 Nodes From\To  0   1   2   3
       0        C   C   C   C
       1        C   C   C   C
       2        C   C   C   C
       3        C   C   C   C
Domain State: UP
`

// Nodes 2 and 3 still run the old driver: both halves block each other, and
// every node still reports READY.
const imexCtlVersionSplit = imexCtlLegend + `11/3/2026 10:02:11.004
Nodes:
Node #0   * 10.76.179.115 *    - READY                - Version: 580.178.04
Node #1   - 10.76.183.30       - READY                - Version: 580.178.04
Node #2   - 10.76.184.132      - READY                - Version: 580.173.02
Node #3   - 10.76.186.255      - READY                - Version: 580.173.02

 Nodes From\To  0   1   2   3
       0        C   C  !V! !V!
       1        C   C  !V! !V!
       2       !V! !V!  C   C
       3       !V! !V!  C   C
Domain State: UP
`

func TestEvaluateIMEXPeers(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		wantErr []string
	}{
		{
			name:   "all peers connected",
			output: imexCtlHealthy,
		},
		{
			name:   "version mismatch with two peers",
			output: imexCtlVersionSplit,
			wantErr: []string{
				"IMEX daemon (version 580.178.04) has disabled communication with 2 peer(s)",
				"node 2 (10.76.184.132): version mismatch, peer version 580.173.02",
				"node 3 (10.76.186.255): version mismatch, peer version 580.173.02",
			},
		},
		{
			name:   "same split seen from an old node",
			output: strings.Replace(strings.Replace(imexCtlVersionSplit, "* 10.76.179.115 *", "- 10.76.179.115  ", 1), "- 10.76.186.255  ", "* 10.76.186.255 *", 1),
			wantErr: []string{
				"IMEX daemon (version 580.173.02) has disabled communication with 2 peer(s)",
				"node 0 (10.76.179.115): version mismatch, peer version 580.178.04",
				"node 1 (10.76.183.30): version mismatch, peer version 580.178.04",
			},
		},
		{
			name: "only the peer blocked the local node",
			output: imexCtlLegend + `Nodes:
Node #0   * 10.0.0.1 *         - READY                - Version: 580.178.04
Node #1   - 10.0.0.2           - READY                - Version: 580.178.04

 Nodes From\To  0   1
       0        C   C
       1       !M!  C
Domain State: UP
`,
			wantErr: []string{"node 1 (10.0.0.2): node map mismatch"},
		},
		{
			name: "broken link between two other nodes does not affect the local node",
			output: imexCtlLegend + `Nodes:
Node #0   * 10.0.0.1 *         - READY                - Version: 580.178.04
Node #1   - 10.0.0.2           - READY                - Version: 580.178.04
Node #2   - 10.0.0.3           - READY                - Version: 580.178.04

 Nodes From\To  0   1   2
       0        C   C   C
       1        C   C  !A!
       2        C  !A!  C
Domain State: UP
`,
		},
		{
			name: "transient states during startup do not fail",
			output: imexCtlLegend + `Nodes:
Node #0   * 10.0.0.1 *         - READY                - Version: 580.178.04
Node #1   - 10.0.0.2           - UNAVAILABLE          - Version:
Node #2   - 127.0.0.2          - UNAVAILABLE          - Version:
Node #3   - 10.0.0.4           - READY                - Version: 580.178.04

 Nodes From\To  0   1   2   3
       0        C   N   N   A
       1        I   I   I   I
       2        I   I   I   I
       3        R   I   I   C
Domain State: DEGRADED
`,
		},
		{
			name: "monitoring mode markers and hostnames are tolerated, last snapshot wins",
			output: imexCtlLegend + `Nodes:
Node #0   * 10.0.0.1 *         - READY                - Version: 580.178.04    - Hostname: a
Node #1   - 10.0.0.2           - READY                - Version: 580.178.04    - Hostname: b

 Nodes From\To  0   1
       0        C   C
       1        C   C
Domain State: UP

Nodes:
Node #0   * 10.0.0.1 *         - READY                - Version: 580.178.04    - Hostname: a
Node #1   - 10.0.0.2           - *READY*              - Version: 580.173.02    - Hostname: b

 Nodes From\To  0   1
       0        C  *!V!*
       1       *!V!* C
Domain State: UP
`,
			wantErr: []string{"node 1 (10.0.0.2): version mismatch, peer version 580.173.02"},
		},
		{
			name: "local node not marked is inconclusive",
			output: imexCtlLegend + `Nodes:
Node #0   - 10.0.0.1           - READY                - Version: 580.178.04
Node #1   - 10.0.0.2           - READY                - Version: 580.173.02

 Nodes From\To  0   1
       0        C  !V!
       1       !V!  C
Domain State: UP
`,
		},
		{
			name:   "unexpected output is inconclusive",
			output: "READY\n",
		},
		{
			name: "malformed table is inconclusive",
			output: `Nodes:
Node #0   * 10.0.0.1 *         - READY                - Version: 580.178.04
Node #1   - 10.0.0.2           - READY                - Version: 580.173.02

 Nodes From\To  0   1
       0        C
Domain State: UP
`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := evaluateIMEXPeers(tc.output)
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err.Error(), want)
				}
			}
		})
	}
}

func TestParseIMEXDomainStatus(t *testing.T) {
	status, err := parseIMEXDomainStatus(imexCtlVersionSplit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.local != 0 {
		t.Errorf("local = %d, want 0", status.local)
	}
	if len(status.nodes) != 4 {
		t.Errorf("parsed %d nodes, want 4", len(status.nodes))
	}
	if got := status.nodes[2].version; got != "580.173.02" {
		t.Errorf("node 2 version = %q, want 580.173.02", got)
	}
	if got := status.links[2][0]; got != "!V!" {
		t.Errorf("link 2->0 = %q, want !V!", got)
	}
	if got := status.links[3][3]; got != "C" {
		t.Errorf("link 3->3 = %q, want C", got)
	}
}

// installFakeIMEXCtl puts an nvidia-imex-ctl on PATH that answers `-q` with
// READY and `-N` with domainOutput (or fails if domainOutput is empty).
func installFakeIMEXCtl(t *testing.T, domainOutput string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "domain.txt"), []byte(domainOutput), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
for arg in "$@"; do
	case "$arg" in
	-q) echo READY; exit 0 ;;
	-N) [ -s "` + dir + `/domain.txt" ] || { echo "connection refused" >&2; exit 1; }
	    cat "` + dir + `/domain.txt"; exit 0 ;;
	esac
done
exit 2
`
	if err := os.WriteFile(filepath.Join(dir, imexCtlBinaryName), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCheckIMEXPeers(t *testing.T) {
	tests := []struct {
		name         string
		checkPeers   bool
		domainOutput string
		wantErr      string
	}{
		{
			name:         "without the peer check only the local daemon is checked",
			domainOutput: imexCtlVersionSplit,
		},
		{
			name:         "peer check passes on a healthy domain",
			checkPeers:   true,
			domainOutput: imexCtlHealthy,
		},
		{
			name:         "peer check fails on a version mismatch",
			checkPeers:   true,
			domainOutput: imexCtlVersionSplit,
			wantErr:      "disabled communication with 2 peer(s)",
		},
		{
			name:       "peer check passes when the domain query fails",
			checkPeers: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			installFakeIMEXCtl(t, tc.domainOutput)
			flags := &Flags{cliqueID: "test-clique", checkIMEXPeers: tc.checkPeers}

			err := check(t.Context(), func() {}, flags)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected an error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

// The readiness probe passes --imex-peers to the check subcommand; make sure
// the flag is wired up. newApp() can only be run once per process because it
// applies the global logging configuration.
func TestCheckCommandIMEXPeersFlag(t *testing.T) {
	installFakeIMEXCtl(t, imexCtlVersionSplit)
	t.Setenv("CLIQUE_ID", "test-clique")

	err := newApp().Run([]string{"compute-domain-daemon", "check", "--imex-peers"})
	if err == nil || !strings.Contains(err.Error(), "disabled communication with 2 peer(s)") {
		t.Fatalf("expected the peer check to fail, got: %v", err)
	}
}
