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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
)

// pathListSep is the separator for PATH-like envvars. The platform sets this
// character.
var pathListSep = string(filepath.ListSeparator)

func TestPrependPathListEnvvar(t *testing.T) {
	// No real process sets this envvar name. Therefore this test does not read the
	// inherited environment. This test also does not change that environment.
	const envvar = "NVIDIA_DRA_TEST_PATH_LIST"

	tests := map[string]struct {
		// envValue is the value of the envvar before the call. os.Getenv returns
		// the empty string for an empty envvar. os.Getenv returns the empty
		// string also for an absent envvar. Therefore an empty envValue tests
		// both conditions.
		envValue string
		prepend  []string
		expected string
	}{
		"no prepend returns the current value verbatim": {
			envValue: "/a" + pathListSep + "/b",
			expected: "/a" + pathListSep + "/b",
		},
		"no prepend and no current value returns empty": {
			envValue: "",
			expected: "",
		},
		"single prepend onto an empty value has no trailing separator": {
			envValue: "",
			prepend:  []string{"/lib/libnvidia-ml.so.1"},
			expected: "/lib/libnvidia-ml.so.1",
		},
		"single prepend onto a single entry": {
			envValue: "/a",
			prepend:  []string{"/lib/libnvidia-ml.so.1"},
			expected: "/lib/libnvidia-ml.so.1" + pathListSep + "/a",
		},
		"multiple prepends keep their relative order": {
			envValue: "/a" + pathListSep + "/b",
			prepend:  []string{"/x", "/y"},
			expected: "/x" + pathListSep + "/y" + pathListSep + "/a" + pathListSep + "/b",
		},
		"multiple prepends onto an empty value have no stray separator": {
			envValue: "",
			prepend:  []string{"/x", "/y"},
			expected: "/x" + pathListSep + "/y",
		},
	}

	for description, tc := range tests {
		t.Run(description, func(t *testing.T) {
			t.Setenv(envvar, tc.envValue)

			require.Equal(t, tc.expected, prependPathListEnvvar(envvar, tc.prepend...))
		})
	}
}

func TestSetOrOverrideEnvvar(t *testing.T) {
	tests := map[string]struct {
		envvars  []string
		key      string
		value    string
		expected []string
	}{
		"a key that is not present is appended": {
			envvars:  []string{"A=1", "B=2"},
			key:      "C",
			value:    "3",
			expected: []string{"A=1", "B=2", "C=3"},
		},
		"an existing key is dropped and the new value appended at the end": {
			envvars:  []string{"A=1", "B=2", "C=3"},
			key:      "B",
			value:    "new",
			expected: []string{"A=1", "C=3", "B=new"},
		},
		"every occurrence of a duplicated key is dropped": {
			envvars:  []string{"A=1", "LD_PRELOAD=x", "B=2", "LD_PRELOAD=y"},
			key:      "LD_PRELOAD",
			value:    "z",
			expected: []string{"A=1", "B=2", "LD_PRELOAD=z"},
		},
		"an entry without a separator is matched on its whole name": {
			envvars:  []string{"BARE", "A=1"},
			key:      "BARE",
			value:    "1",
			expected: []string{"A=1", "BARE=1"},
		},
		"an unrelated entry without a separator is preserved": {
			envvars:  []string{"BARE", "A=1"},
			key:      "B",
			value:    "2",
			expected: []string{"BARE", "A=1", "B=2"},
		},
		"only the first separator delimits the key": {
			envvars:  []string{"A=b=c", "B=2"},
			key:      "A",
			value:    "new",
			expected: []string{"B=2", "A=new"},
		},
		"a value may itself contain a separator": {
			envvars:  []string{"A=1"},
			key:      "B",
			value:    "x=y",
			expected: []string{"A=1", "B=x=y"},
		},
		"an empty list yields a single entry": {
			envvars:  nil,
			key:      "A",
			value:    "1",
			expected: []string{"A=1"},
		},
	}

	for description, tc := range tests {
		t.Run(description, func(t *testing.T) {
			require.Equal(t, tc.expected, setOrOverrideEnvvar(tc.envvars, tc.key, tc.value))
		})
	}
}

func TestSetMax(t *testing.T) {
	// requireCapacities compares the integer values in m. A direct comparison of
	// resource.Quantity structs is less reliable.
	requireCapacities := func(t *testing.T, m PartCapacityMap, expected map[resourceapi.QualifiedName]int64) {
		t.Helper()

		got := make(map[resourceapi.QualifiedName]int64, len(m))
		for name, capacity := range m {
			got[name] = capacity.Value.Value()
		}
		require.Equal(t, expected, got)
	}

	t.Run("a key that is absent is inserted", func(t *testing.T) {
		m := make(PartCapacityMap)

		setMax(m, "memory", intcap(4))

		requireCapacities(t, m, map[resourceapi.QualifiedName]int64{"memory": 4})
	})

	t.Run("a larger value replaces the stored one", func(t *testing.T) {
		m := PartCapacityMap{"memory": intcap(4)}

		setMax(m, "memory", intcap(8))

		requireCapacities(t, m, map[resourceapi.QualifiedName]int64{"memory": 8})
	})

	t.Run("a smaller value is ignored", func(t *testing.T) {
		m := PartCapacityMap{"memory": intcap(8)}

		setMax(m, "memory", intcap(4))

		requireCapacities(t, m, map[resourceapi.QualifiedName]int64{"memory": 8})
	})

	t.Run("an equal value is ignored", func(t *testing.T) {
		m := PartCapacityMap{"memory": intcap(8)}

		setMax(m, "memory", intcap(8))

		requireCapacities(t, m, map[resourceapi.QualifiedName]int64{"memory": 8})
	})

	t.Run("unrelated keys are left untouched", func(t *testing.T) {
		m := PartCapacityMap{"memory": intcap(8), "copy-engines": intcap(2)}

		setMax(m, "memory", intcap(16))

		requireCapacities(t, m, map[resourceapi.QualifiedName]int64{"memory": 16, "copy-engines": 2})
	})
}

func TestDeviceLibGetGpuInfoByMinor(t *testing.T) {
	tests := map[string]struct {
		gpuInfosByUUID map[string]*GpuInfo
		minor          GPUMinor
		expectedUUID   string
		expectedError  string
	}{
		"finds the GPU with the matching minor": {
			gpuInfosByUUID: map[string]*GpuInfo{
				"GPU-1": {UUID: "GPU-1", minor: 1},
				"GPU-2": {UUID: "GPU-2", minor: 2},
			},
			minor:        2,
			expectedUUID: "GPU-2",
		},
		"finds a GPU with minor zero": {
			gpuInfosByUUID: map[string]*GpuInfo{
				"GPU-0": {UUID: "GPU-0", minor: 0},
			},
			minor:        0,
			expectedUUID: "GPU-0",
		},
		"returns an error when the minor is not found": {
			gpuInfosByUUID: map[string]*GpuInfo{
				"GPU-1": {UUID: "GPU-1", minor: 1},
			},
			minor:         2,
			expectedError: "gpu info not found for minor 2",
		},
		"returns an error when no GPUs are known": {
			minor:         1,
			expectedError: "gpu info not found for minor 1",
		},
	}

	for description, tc := range tests {
		t.Run(description, func(t *testing.T) {
			l := deviceLib{gpuInfosByUUID: tc.gpuInfosByUUID}

			gpuInfo, err := l.getGpuInfoByMinor(tc.minor)
			if tc.expectedError != "" {
				require.EqualError(t, err, tc.expectedError)
				require.Nil(t, gpuInfo)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expectedUUID, gpuInfo.UUID)
		})
	}
}

// fakeSMI replaces the nvidia-smi binary and the NVML library. setTimeSlice and
// setComputeMode run these two files.
type fakeSMI struct {
	// path is the value for deviceLib.nvidiaSMIPath.
	path string
	// libPath is the value for deviceLib.driverLibraryPath. This file exists, but
	// the file is empty. Therefore the dynamic loader gives only a warning.
	libPath string
	// logPath is the log file. The stub writes one line for each call.
	logPath string
}

// newFakeSMI writes an executable stub. For each call, the stub writes one line
// in the format "<LD_PRELOAD>|<args>". Then the stub exits with exitCode.
func newFakeSMI(t *testing.T, exitCode int) fakeSMI {
	t.Helper()

	dir := t.TempDir()
	smi := fakeSMI{
		path:    filepath.Join(dir, "nvidia-smi"),
		libPath: filepath.Join(dir, "libnvidia-ml.so.1"),
		logPath: filepath.Join(dir, "invocations.log"),
	}

	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s|%%s\\n' \"$LD_PRELOAD\" \"$*\" >> %q\nexit %d\n", smi.logPath, exitCode)
	require.NoError(t, os.WriteFile(smi.path, []byte(script), 0o755))
	require.NoError(t, os.WriteFile(smi.libPath, []byte{}, 0o644))

	return smi
}

// invocations returns one entry for each recorded call. The order of the
// entries is the order of the calls.
func (s fakeSMI) invocations(t *testing.T) []string {
	t.Helper()

	content, err := os.ReadFile(s.logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)

	trimmed := strings.TrimSuffix(string(content), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func TestDeviceLibSetTimeSlice(t *testing.T) {
	t.Run("one invocation per UUID", func(t *testing.T) {
		t.Setenv("LD_PRELOAD", "")
		smi := newFakeSMI(t, 0)
		l := deviceLib{nvidiaSMIPath: smi.path, driverLibraryPath: smi.libPath}

		require.NoError(t, l.setTimeSlice([]string{"GPU-1", "GPU-2"}, 5))

		require.Equal(t, []string{
			smi.libPath + "|compute-policy -i GPU-1 --set-timeslice 5",
			smi.libPath + "|compute-policy -i GPU-2 --set-timeslice 5",
		}, smi.invocations(t))
	})

	t.Run("an empty UUID list invokes nothing", func(t *testing.T) {
		smi := newFakeSMI(t, 0)
		l := deviceLib{nvidiaSMIPath: smi.path, driverLibraryPath: smi.libPath}

		require.NoError(t, l.setTimeSlice(nil, 5))

		require.Empty(t, smi.invocations(t))
	})

	t.Run("a failing invocation aborts the remaining UUIDs", func(t *testing.T) {
		smi := newFakeSMI(t, 1)
		l := deviceLib{nvidiaSMIPath: smi.path, driverLibraryPath: smi.libPath}

		require.Error(t, l.setTimeSlice([]string{"GPU-1", "GPU-2"}, 5))

		require.Len(t, smi.invocations(t), 1)
	})

	t.Run("the driver library is prepended to an existing LD_PRELOAD", func(t *testing.T) {
		t.Setenv("LD_PRELOAD", "/preexisting.so")
		smi := newFakeSMI(t, 0)
		l := deviceLib{nvidiaSMIPath: smi.path, driverLibraryPath: smi.libPath}

		require.NoError(t, l.setTimeSlice([]string{"GPU-1"}, 5))

		require.Equal(t, []string{
			smi.libPath + pathListSep + "/preexisting.so|compute-policy -i GPU-1 --set-timeslice 5",
		}, smi.invocations(t))
	})
}

func TestDeviceLibSetComputeMode(t *testing.T) {
	t.Run("one invocation per UUID", func(t *testing.T) {
		t.Setenv("LD_PRELOAD", "")
		smi := newFakeSMI(t, 0)
		l := deviceLib{nvidiaSMIPath: smi.path, driverLibraryPath: smi.libPath}

		require.NoError(t, l.setComputeMode([]string{"GPU-1", "GPU-2"}, "EXCLUSIVE_PROCESS"))

		require.Equal(t, []string{
			smi.libPath + "|-i GPU-1 -c EXCLUSIVE_PROCESS",
			smi.libPath + "|-i GPU-2 -c EXCLUSIVE_PROCESS",
		}, smi.invocations(t))
	})

	t.Run("an empty UUID list invokes nothing", func(t *testing.T) {
		smi := newFakeSMI(t, 0)
		l := deviceLib{nvidiaSMIPath: smi.path, driverLibraryPath: smi.libPath}

		require.NoError(t, l.setComputeMode(nil, "DEFAULT"))

		require.Empty(t, smi.invocations(t))
	})

	t.Run("a failing invocation aborts the remaining UUIDs", func(t *testing.T) {
		smi := newFakeSMI(t, 1)
		l := deviceLib{nvidiaSMIPath: smi.path, driverLibraryPath: smi.libPath}

		require.Error(t, l.setComputeMode([]string{"GPU-1", "GPU-2"}, "DEFAULT"))

		require.Len(t, smi.invocations(t), 1)
	})

	t.Run("the driver library is prepended to an existing LD_PRELOAD", func(t *testing.T) {
		t.Setenv("LD_PRELOAD", "/preexisting.so")
		smi := newFakeSMI(t, 0)
		l := deviceLib{nvidiaSMIPath: smi.path, driverLibraryPath: smi.libPath}

		require.NoError(t, l.setComputeMode([]string{"GPU-1"}, "DEFAULT"))

		require.Equal(t, []string{
			smi.libPath + pathListSep + "/preexisting.so|-i GPU-1 -c DEFAULT",
		}, smi.invocations(t))
	})
}
