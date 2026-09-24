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
