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

package imex

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseConfigOverrides(t *testing.T) {
	tests := map[string]struct {
		csv     string
		want    map[string]string
		wantErr bool
	}{
		"empty string yields no overrides": {
			csv:  "",
			want: map[string]string{},
		},
		"single pair": {
			csv:  "IMEX_NODE_DISCONNECTED_GRACE_TIME=60",
			want: map[string]string{"IMEX_NODE_DISCONNECTED_GRACE_TIME": "60"},
		},
		"multiple pairs with trailing comma (matches template-rendered form)": {
			csv: "IMEX_NODE_DISCONNECTED_GRACE_TIME=60,LOG_LEVEL=3,",
			want: map[string]string{
				"IMEX_NODE_DISCONNECTED_GRACE_TIME": "60",
				"LOG_LEVEL":                         "3",
			},
		},
		"whitespace around keys and values is trimmed": {
			csv:  " LOG_LEVEL = 3 ",
			want: map[string]string{"LOG_LEVEL": "3"},
		},
		"value may itself contain '='": {
			csv:  "IMEX_SECURITY_TARGET_OVERRIDE=a=b",
			want: map[string]string{"IMEX_SECURITY_TARGET_OVERRIDE": "a=b"},
		},
		"empty value is allowed": {
			csv:  "LOG_FILE_NAME=",
			want: map[string]string{"LOG_FILE_NAME": ""},
		},
		"missing '=' is an error": {
			csv:     "LOG_LEVEL",
			wantErr: true,
		},
		"empty key is an error": {
			csv:     "=3",
			wantErr: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ParseConfigOverrides(tc.csv)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestValidateConfigOverrides(t *testing.T) {
	tests := map[string]struct {
		overrides map[string]string
		wantErr   bool
	}{
		"nil overrides are fine": {
			overrides: nil,
		},
		"unrelated overrides are fine": {
			overrides: map[string]string{"IMEX_NODE_DISCONNECTED_GRACE_TIME": "60", "LOG_LEVEL": "3"},
		},
		"overriding the command bind address is rejected": {
			overrides: map[string]string{"IMEX_CMD_BIND_INTERFACE_IP": "10.0.0.1"},
			wantErr:   true,
		},
		"overriding the generated nodes config path is rejected": {
			overrides: map[string]string{"IMEX_NODE_CONFIG_FILE": "/tmp/evil.cfg"},
			wantErr:   true,
		},
		"a driver-managed key alongside unrelated ones is still rejected": {
			overrides: map[string]string{"LOG_LEVEL": "3", "IMEX_NODE_CONFIG_FILE": "/tmp/evil.cfg"},
			wantErr:   true,
		},
		"a newline in a value is rejected": {
			overrides: map[string]string{"LOG_LEVEL": "3\nIMEX_NODE_CONFIG_FILE=/tmp/evil.cfg"},
			wantErr:   true,
		},
		"a carriage return in a value is rejected": {
			overrides: map[string]string{"LOG_LEVEL": "3\rIMEX_NODE_CONFIG_FILE=/tmp/evil.cfg"},
			wantErr:   true,
		},
		"a newline in a key is rejected": {
			overrides: map[string]string{"LOG_LEVEL\nIMEX_NODE_CONFIG_FILE": "/tmp/evil.cfg"},
			wantErr:   true,
		},
		"non-snake-case keys are not validated": {
			overrides: map[string]string{"log_level": "3"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := ValidateConfigOverrides(tc.overrides)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}
