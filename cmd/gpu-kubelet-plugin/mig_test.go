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
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
)

func TestMigSpecTupleToCanonicalName(t *testing.T) {
	tests := map[string]struct {
		tuple       MigSpecTuple
		profileName string
		want        DeviceName
	}{
		"dots stripped from profile name": {
			tuple:       MigSpecTuple{ParentMinor: 0, ProfileID: 19, PlacementStart: 0},
			profileName: "1g.5gb",
			want:        "gpu-0-mig-1g5gb-19-0",
		},
		"uppercase folded to lowercase": {
			tuple:       MigSpecTuple{ParentMinor: 1, ProfileID: 14, PlacementStart: 2},
			profileName: "2G.10GB",
			want:        "gpu-1-mig-2g10gb-14-2",
		},
		// '+' is not RFC1123-legal and becomes '-', so a profile-name segment can
		// itself contain a hyphen -- the parse test covers recovery from this.
		"plus becomes hyphen": {
			tuple:       MigSpecTuple{ParentMinor: 3, ProfileID: 19, PlacementStart: 4},
			profileName: "1g.5gb+me",
			want:        "gpu-3-mig-1g5gb-me-19-4",
		},
	}

	for description, tc := range tests {
		t.Run(description, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.tuple.ToCanonicalName(tc.profileName))
		})
	}
}

func TestNewMigSpecTupleFromCanonicalName(t *testing.T) {
	tests := map[string]struct {
		name string
		want *MigSpecTuple // nil => an error is expected
	}{
		"valid name": {
			name: "gpu-0-mig-1g5gb-19-0",
			want: &MigSpecTuple{ParentMinor: 0, ProfileID: 19, PlacementStart: 0},
		},
		// The profile-name group is greedy; trailing -<id>-<start> must still be
		// recovered when the profile name itself contains a hyphen.
		"profile name containing a hyphen": {
			name: "gpu-3-mig-1g5gb-me-19-4",
			want: &MigSpecTuple{ParentMinor: 3, ProfileID: 19, PlacementStart: 4},
		},
		"missing placement group":  {name: "gpu-0-mig-1g5gb-19"},
		"not a mig device name":    {name: "gpu-0-gpu-0"},
		"non-numeric parent minor": {name: "gpu-x-mig-1g5gb-19-0"},
	}

	for description, tc := range tests {
		t.Run(description, func(t *testing.T) {
			got, err := NewMigSpecTupleFromCanonicalName(DeviceName(tc.name))
			if tc.want == nil {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	// The three numeric dimensions survive a round trip; the profile name is
	// deliberately not part of the parsed tuple (device_state.go relies on this
	// to classify device names).
	t.Run("round trip recovers numeric dimensions", func(t *testing.T) {
		in := MigSpecTuple{ParentMinor: 7, ProfileID: 21, PlacementStart: 4}
		got, err := NewMigSpecTupleFromCanonicalName(in.ToCanonicalName("3g.20gb"))
		require.NoError(t, err)
		assert.Equal(t, &in, got)
	})
}

func TestToRFC1123Compliant(t *testing.T) {
	tests := map[string]struct {
		in   string
		want string
	}{
		"lowercased":               {in: "Foo", want: "foo"},
		"illegal chars to hyphen":  {in: "a_b+c", want: "a-b-c"},
		"leading/trailing trimmed": {in: "-abc-", want: "abc"},
		"trailing dot trimmed":     {in: "abc.", want: "abc"},
	}

	for description, tc := range tests {
		t.Run(description, func(t *testing.T) {
			assert.Equal(t, tc.want, toRFC1123Compliant(tc.in))
		})
	}
}

func TestCamelToDNSName(t *testing.T) {
	tests := map[string]struct {
		in   string
		want string
	}{
		"camel case split": {in: "copyEngines", want: "copy-engines"},
		"acronym boundary": {in: "HTTPServer", want: "http-server"},
		"digit boundary":   {in: "gpu2Slice", want: "gpu2-slice"},
	}

	for description, tc := range tests {
		t.Run(description, func(t *testing.T) {
			assert.Equal(t, tc.want, camelToDNSName(tc.in))
		})
	}
}

func TestCommonCapacitiesMig(t *testing.T) {
	// Distinct value per field so a mis-wired mapping (a capacity reading the
	// wrong profile counter) is caught, not just the memory conversion.
	caps := CommonCapacitiesMig(&nvml.GpuInstanceProfileInfo{
		MultiprocessorCount: 14,
		CopyEngineCount:     2,
		DecoderCount:        3,
		EncoderCount:        4,
		JpegCount:           5,
		OfaCount:            6,
		MemorySizeMB:        7,
	})

	// Quantity.Value has a pointer receiver, so read each through a copy of the
	// (non-addressable) map value.
	valueOf := func(name resourceapi.QualifiedName) int64 {
		c := caps[name]
		return c.Value.Value()
	}

	require.Len(t, caps, 7)
	assert.Equal(t, int64(14), valueOf("multiprocessors"))
	assert.Equal(t, int64(2), valueOf("copyEngines"))
	assert.Equal(t, int64(3), valueOf("decoders"))
	assert.Equal(t, int64(4), valueOf("encoders"))
	assert.Equal(t, int64(5), valueOf("jpegEngines"))
	assert.Equal(t, int64(6), valueOf("ofaEngines"))
	// MemorySizeMB is documented as MiB, so it is announced as MiB * 2^20 bytes.
	assert.Equal(t, int64(7*1024*1024), valueOf("memory"))
}
