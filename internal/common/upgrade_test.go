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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRegistrarSocketPath(t *testing.T) {
	registrarDir := "/var/lib/kubelet/plugins_registry"
	uid := "08a5f01c-b566-45ea-abb9-6522b1bcbc22"

	t.Run("without rolling update pod UID", func(t *testing.T) {
		require.Equal(t,
			"/var/lib/kubelet/plugins_registry/gpu.nvidia.com-reg.sock",
			RegistrarSocketPath(registrarDir, "gpu.nvidia.com", ""),
		)
		require.Equal(t,
			"/var/lib/kubelet/plugins_registry/compute-domain.nvidia.com-reg.sock",
			RegistrarSocketPath(registrarDir, "compute-domain.nvidia.com", ""),
		)
	})

	t.Run("with rolling update pod UID", func(t *testing.T) {
		require.Equal(t,
			"/var/lib/kubelet/plugins_registry/gpu.nvidia.com-08a5f01c-b566-45ea-abb9-6522b1bcbc22-reg.sock",
			RegistrarSocketPath(registrarDir, "gpu.nvidia.com", uid),
		)
		require.Equal(t,
			"/var/lib/kubelet/plugins_registry/compute-domain.nvidia.com-08a5f01c-b566-45ea-abb9-6522b1bcbc22-reg.sock",
			RegistrarSocketPath(registrarDir, "compute-domain.nvidia.com", uid),
		)
	})
}

func TestDRASocketPath(t *testing.T) {
	uid := "08a5f01c-b566-45ea-abb9-6522b1bcbc22"

	t.Run("without rolling update pod UID", func(t *testing.T) {
		require.Equal(t,
			"/var/lib/kubelet/plugins/gpu.nvidia.com/dra.sock",
			DRASocketPath("/var/lib/kubelet/plugins/gpu.nvidia.com", ""),
		)
	})

	t.Run("with rolling update pod UID", func(t *testing.T) {
		require.Equal(t,
			"/var/lib/kubelet/plugins/gpu.nvidia.com/dra-08a5f01c-b566-45ea-abb9-6522b1bcbc22.sock",
			DRASocketPath("/var/lib/kubelet/plugins/gpu.nvidia.com", uid),
		)
	})
}

func TestAtomicWriteFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "binary")

	require.NoError(t, AtomicWriteFile(target, []byte("v1"), 0755))
	content, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "v1", string(content))

	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0755), info.Mode().Perm())

	// Overwriting an existing target must succeed and leave no temp files.
	require.NoError(t, AtomicWriteFile(target, []byte("v2"), 0755))
	content, err = os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "v2", string(content))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}
