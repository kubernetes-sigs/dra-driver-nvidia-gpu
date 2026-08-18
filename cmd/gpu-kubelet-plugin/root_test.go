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
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFindFile(t *testing.T) {
	// Mirrors the search paths used by getDriverLibraryPath.
	librarySearchPaths := []string{
		"/usr/lib64",
		"/usr/lib/x86_64-linux-gnu",
		"/usr/lib/aarch64-linux-gnu",
		"/lib64",
		"/lib/x86_64-linux-gnu",
		"/lib/aarch64-linux-gnu",
	}
	// Mirrors the search paths used by getNvidiaSMIPath.
	binarySearchPaths := []string{
		"/opt/bin",
		"/usr/bin",
		"/usr/sbin",
		"/bin",
		"/sbin",
	}

	tests := map[string]struct {
		// name is the file findFile searches for.
		name string
		// searchIn are the folders searched in addition to the root.
		searchIn []string
		// dirs are created (relative to the test root) before searching.
		dirs []string
		// files are created (relative to the test root) before searching.
		files []string
		// expected is the path (relative to the test root) findFile should
		// return, or "" if an error is expected.
		expected string
	}{
		"library in first search path": {
			name:     "libnvidia-ml.so.1",
			searchIn: librarySearchPaths,
			files:    []string{"usr/lib64/libnvidia-ml.so.1"},
			expected: "usr/lib64/libnvidia-ml.so.1",
		},
		"directory in earlier search path does not shadow library": {
			name:     "libnvidia-ml.so.1",
			searchIn: librarySearchPaths,
			dirs:     []string{"usr/lib64/libnvidia-ml.so.1"},
			files:    []string{"usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1"},
			expected: "usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1",
		},
		"only directories found": {
			name:     "libnvidia-ml.so.1",
			searchIn: librarySearchPaths,
			dirs:     []string{"usr/lib64/libnvidia-ml.so.1"},
			expected: "",
		},
		"library not found": {
			name:     "libnvidia-ml.so.1",
			searchIn: librarySearchPaths,
			expected: "",
		},
		"nvidia-smi in binary search path": {
			name:     "nvidia-smi",
			searchIn: binarySearchPaths,
			files:    []string{"usr/bin/nvidia-smi"},
			expected: "usr/bin/nvidia-smi",
		},
		"directory in earlier search path does not shadow nvidia-smi": {
			name:     "nvidia-smi",
			searchIn: binarySearchPaths,
			dirs:     []string{"opt/bin/nvidia-smi"},
			files:    []string{"usr/bin/nvidia-smi"},
			expected: "usr/bin/nvidia-smi",
		},
	}

	for description, tc := range tests {
		t.Run(description, func(t *testing.T) {
			testRoot := t.TempDir()
			for _, d := range tc.dirs {
				require.NoError(t, os.MkdirAll(filepath.Join(testRoot, d), 0o755))
			}
			for _, f := range tc.files {
				path := filepath.Join(testRoot, f)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte{}, 0o644))
			}

			found, err := root(testRoot).findFile(tc.name, tc.searchIn...)
			if tc.expected == "" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			// t.TempDir may itself contain symlinks (e.g. on macOS), so
			// compare against the resolved expected path.
			expected, err := filepath.EvalSymlinks(filepath.Join(testRoot, tc.expected))
			require.NoError(t, err)
			require.Equal(t, expected, found)
		})
	}
}

func TestFindFileFollowsSymlink(t *testing.T) {
	const name = "libnvidia-ml.so.1"

	t.Run("symlink to a regular file is resolved", func(t *testing.T) {
		testRoot := t.TempDir()
		target := filepath.Join(testRoot, "opt", name)
		require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
		require.NoError(t, os.WriteFile(target, []byte{}, 0o644))

		linkDir := filepath.Join(testRoot, "usr", "lib64")
		require.NoError(t, os.MkdirAll(linkDir, 0o755))
		require.NoError(t, os.Symlink(target, filepath.Join(linkDir, name)))

		found, err := root(testRoot).findFile(name, "usr/lib64")
		require.NoError(t, err)
		want, err := filepath.EvalSymlinks(target)
		require.NoError(t, err)
		require.Equal(t, want, found)
	})

	t.Run("symlink to a directory is rejected", func(t *testing.T) {
		testRoot := t.TempDir()
		targetDir := filepath.Join(testRoot, "opt", name)
		require.NoError(t, os.MkdirAll(targetDir, 0o755))

		linkDir := filepath.Join(testRoot, "usr", "lib64")
		require.NoError(t, os.MkdirAll(linkDir, 0o755))
		require.NoError(t, os.Symlink(targetDir, filepath.Join(linkDir, name)))

		_, err := root(testRoot).findFile(name, "usr/lib64")
		require.Error(t, err)
	})

	t.Run("dangling symlink is rejected", func(t *testing.T) {
		testRoot := t.TempDir()
		linkDir := filepath.Join(testRoot, "usr", "lib64")
		require.NoError(t, os.MkdirAll(linkDir, 0o755))
		require.NoError(t, os.Symlink(filepath.Join(testRoot, "missing.so"), filepath.Join(linkDir, name)))

		_, err := root(testRoot).findFile(name, "usr/lib64")
		require.Error(t, err)
	})
}

func TestRootGetDevRoot(t *testing.T) {
	withDev := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(withDev, "dev"), 0o755))
	require.Equal(t, withDev, root(withDev).getDevRoot())

	require.Equal(t, "/", root(t.TempDir()).getDevRoot())

	devIsFile := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(devIsFile, "dev"), []byte{}, 0o644))
	require.Equal(t, "/", root(devIsFile).getDevRoot())
}

func TestRootGetDriverAndBinaryPaths(t *testing.T) {
	testRoot := t.TempDir()
	writeFile := func(rel string) string {
		p := filepath.Join(testRoot, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte{}, 0o644))
		want, err := filepath.EvalSymlinks(p)
		require.NoError(t, err)
		return want
	}
	wantNVML := writeFile("usr/lib64/libnvidia-ml.so.1")
	wantFM := writeFile("usr/lib64/libnvfm.so.1")
	wantSMI := writeFile("usr/bin/nvidia-smi")

	r := root(testRoot)

	got, err := r.getDriverLibraryPath()
	require.NoError(t, err)
	require.Equal(t, wantNVML, got)

	got, err = r.getFMLibraryPath()
	require.NoError(t, err)
	require.Equal(t, wantFM, got)

	got, err = r.getNvidiaSMIPath()
	require.NoError(t, err)
	require.Equal(t, wantSMI, got)

	_, err = root(t.TempDir()).getDriverLibraryPath()
	require.Error(t, err)
}
