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
	"bytes"
	"strings"
	"testing"
	"text/template"
)

// renderBaseIMEXConfig renders the actual template baked into the
// compute-domain-kubelet-plugin image.
func renderBaseIMEXConfig(t *testing.T) string {
	t.Helper()
	tmpl, err := template.ParseFiles("../../templates/compute-domain-daemon-config.tmpl.cfg")
	if err != nil {
		t.Fatalf("failed to parse template file: %v", err)
	}

	data := IMEXConfigTemplateData{
		IMEXCmdBindInterfaceIP:    "10.0.0.5",
		IMEXDaemonNodesConfigPath: "/imexd/nodes.cfg",
	}

	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		t.Fatalf("failed to execute template: %v", err)
	}
	return out.String()
}

func TestApplyIMEXConfigOverrides(t *testing.T) {
	base := renderBaseIMEXConfig(t)

	t.Run("no overrides leaves the base config untouched", func(t *testing.T) {
		got := applyIMEXConfigOverrides([]byte(base), nil)
		if string(got) != base {
			t.Errorf("expected base config unchanged, got a diff")
		}
	})

	t.Run("overriding an existing setting replaces its line in place", func(t *testing.T) {
		if !strings.Contains(base, "IMEX_NODE_DISCONNECTED_GRACE_TIME=-1") {
			t.Fatalf("test assumption broken: base config no longer contains the default grace time line")
		}

		got := string(applyIMEXConfigOverrides([]byte(base), map[string]string{
			"IMEX_NODE_DISCONNECTED_GRACE_TIME": "60",
		}))

		if !strings.Contains(got, "IMEX_NODE_DISCONNECTED_GRACE_TIME=60") {
			t.Errorf("expected overridden grace time line, got:\n%s", got)
		}
		if strings.Contains(got, "IMEX_NODE_DISCONNECTED_GRACE_TIME=-1") {
			t.Errorf("old default line was not replaced, got:\n%s", got)
		}
		// Exactly one occurrence of the key: no duplicate line left behind.
		if n := strings.Count(got, "IMEX_NODE_DISCONNECTED_GRACE_TIME="); n != 1 {
			t.Errorf("expected exactly 1 occurrence of the setting, got %d", n)
		}
	})

	t.Run("overriding a setting with no existing line appends it", func(t *testing.T) {
		got := string(applyIMEXConfigOverrides([]byte(base), map[string]string{
			"IMEX_SOME_FUTURE_SETTING": "42",
		}))

		if !strings.Contains(got, "IMEX_SOME_FUTURE_SETTING=42") {
			t.Errorf("expected new setting to be appended, got:\n%s", got)
		}
	})

	t.Run("comment lines containing '=' are never matched or corrupted", func(t *testing.T) {
		// Several comment lines in the real template contain '=' (e.g.
		// mentioning IMEX_CMD_ENABLED=0 or IMEX_AUTH_ENCRYPTION_MODE=SSL_TLS
		// in prose). None of those should be treated as a settable key.
		got := string(applyIMEXConfigOverrides([]byte(base), map[string]string{
			"IMEX_CMD_ENABLED": "0",
		}))

		// The real setting line for IMEX_CMD_ENABLED should be updated...
		if !strings.Contains(got, "\nIMEX_CMD_ENABLED=0") {
			t.Errorf("expected IMEX_CMD_ENABLED=0 setting line, got:\n%s", got)
		}
		// ...but comment lines mentioning it in prose must be untouched.
		if !strings.Contains(got, "Ignored if IMEX_CMD_ENABLED=0") {
			t.Errorf("comment line referencing IMEX_CMD_ENABLED was corrupted")
		}
	})

	t.Run("multiple overrides are all applied", func(t *testing.T) {
		got := string(applyIMEXConfigOverrides([]byte(base), map[string]string{
			"IMEX_NODE_DISCONNECTED_GRACE_TIME": "60",
			"LOG_LEVEL":                         "3",
			"IMEX_SOME_FUTURE_SETTING":          "42",
		}))

		for _, want := range []string{
			"IMEX_NODE_DISCONNECTED_GRACE_TIME=60",
			"LOG_LEVEL=3",
			"IMEX_SOME_FUTURE_SETTING=42",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("expected %q in rendered config, got:\n%s", want, got)
			}
		}
	})
}
