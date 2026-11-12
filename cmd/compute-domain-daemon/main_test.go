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

package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDaemonKubeconfigSelection(t *testing.T) {
	for name, tc := range map[string]struct {
		flag          bool
		env           bool
		errorContains string
	}{
		"flag":                {flag: true, errorContains: "create out-of-cluster client configuration"},
		"environment":         {env: true, errorContains: "create out-of-cluster client configuration"},
		"in-cluster fallback": {errorContains: "create in-cluster client configuration"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("KUBECONFIG", "")
			t.Setenv("KUBERNETES_SERVICE_HOST", "")
			t.Setenv("KUBERNETES_SERVICE_PORT", "")
			missing := filepath.Join(t.TempDir(), "missing-kubeconfig")
			args := []string{"compute-domain-daemon", "--compute-domain-uuid=fixture"}
			if tc.flag {
				args = append(args, "--kubeconfig="+missing)
			}
			if tc.env {
				t.Setenv("KUBECONFIG", missing)
			}
			args = append(args, "run")
			app := newApp()
			// Logging configuration is process-global and can only be applied once.
			// Client configuration is parsed by the CLI independently of that hook.
			app.Before = nil
			err := app.Run(args)
			require.ErrorContains(t, err, tc.errorContains)
			if tc.flag || tc.env {
				require.ErrorContains(t, err, missing)
			}
		})
	}
}
