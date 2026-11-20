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

package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigCapsPaths(t *testing.T) {
	tests := map[string]struct {
		altProcDriverPath string
		expectedGIPath    string
		expectedCIPath    string
	}{
		"default reads procfs": {
			expectedGIPath: "/proc/driver/nvidia/capabilities/gpu2/mig/gi5/access",
			expectedCIPath: "/proc/driver/nvidia/capabilities/gpu2/mig/gi5/ci1/access",
		},
		"ALT_PROC_DRIVER_PATH replaces /proc/driver": {
			altProcDriverPath: "/driver-root/proc/driver",
			expectedGIPath:    "/driver-root/proc/driver/nvidia/capabilities/gpu2/mig/gi5/access",
			expectedCIPath:    "/driver-root/proc/driver/nvidia/capabilities/gpu2/mig/gi5/ci1/access",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// Registered before t.Setenv so it runs after the env var is
			// restored: cleanups run last-in, first-out.
			t.Cleanup(ResetProcDriverPath)
			t.Setenv("ALT_PROC_DRIVER_PATH", tc.altProcDriverPath)
			ResetProcDriverPath()

			giPath, ciPath := MigCapsPaths(2, 5, 1)
			require.Equal(t, tc.expectedGIPath, giPath)
			require.Equal(t, tc.expectedCIPath, ciPath)
		})
	}
}
