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

package imex

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestChannelAllocationRoundTrip(t *testing.T) {
	data, err := ChannelAllocation{Channel: 7, IMEXDomain: "clique-a"}.Encode()
	require.NoError(t, err)
	require.JSONEq(t, `{"channel":7,"imexDomain":"clique-a"}`, string(data.Raw))

	decoded, err := DecodeChannelAllocation(data)
	require.NoError(t, err)
	require.Equal(t, &ChannelAllocation{Channel: 7, IMEXDomain: "clique-a"}, decoded)

	decoded, err = DecodeChannelAllocation(nil)
	require.NoError(t, err)
	require.Nil(t, decoded, "no data before admission")

	_, err = DecodeChannelAllocation(&runtime.RawExtension{Raw: []byte(`{"channel":2048}`)})
	require.Error(t, err)
}

func TestValidateMaxChannels(t *testing.T) {
	tests := map[string]struct {
		maxChannels int
		wantErr     bool
	}{
		"zero":           {maxChannels: 0, wantErr: true},
		"one":            {maxChannels: 1},
		"default":        {maxChannels: DefaultMaxChannels},
		"driver maximum": {maxChannels: MaxChannels},
		"above maximum":  {maxChannels: MaxChannels + 1, wantErr: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := ValidateMaxChannels(tc.maxChannels)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}
