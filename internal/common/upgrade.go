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
	"fmt"
	"os"
	"path"
	"path/filepath"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

// RegistrarSocketPath returns the path of the registration socket that the
// kubelet plugin helper listens on. With a non-empty rolling update pod UID,
// that is a UID-suffixed socket name unique to one plugin instance (see
// [kubeletplugin.RollingUpdateRegistrarSocketFile]); otherwise the plain
// per-driver socket name.
func RegistrarSocketPath(registrarDir, driverName, rollingUpdatePodUID string) string {
	if rollingUpdatePodUID != "" {
		return path.Join(registrarDir, kubeletplugin.RollingUpdateRegistrarSocketFile(registrarDir, driverName, types.UID(rollingUpdatePodUID)))
	}
	return path.Join(registrarDir, driverName+"-reg.sock")
}

// DRASocketPath returns the path of the DRA gRPC socket that the kubelet
// plugin helper listens on. The library does not export its naming logic for
// this socket (and documents [kubeletplugin.PluginSocket] as test-only), so
// this mirrors the name construction in [kubeletplugin.Start]: "dra.sock",
// or "dra-<pod UID>.sock" when rolling updates are enabled. Keep in sync
// with the vendored k8s.io/dynamic-resource-allocation/kubeletplugin.
func DRASocketPath(pluginDir, rollingUpdatePodUID string) string {
	if rollingUpdatePodUID != "" {
		return path.Join(pluginDir, "dra-"+rollingUpdatePodUID+".sock")
	}
	return path.Join(pluginDir, "dra.sock")
}

// AtomicWriteFile writes data to a temporary file in the target's directory
// and renames it into place, fsyncing both the file and the directory. Use
// this for files in directories shared with other processes (e.g. another
// plugin instance during a rolling update): the target path is never observed
// in a truncated or partially written state, and a crash mid-write cannot
// leave a corrupt file at the target path.
func AtomicWriteFile(targetPath string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(targetPath)

	tmpFile, err := os.CreateTemp(dir, filepath.Base(targetPath)+".tmp*")
	if err != nil {
		return fmt.Errorf("error creating temporary file for %s: %w", targetPath, err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return fmt.Errorf("error writing %s: %w", tmpFile.Name(), err)
	}
	if err := tmpFile.Chmod(mode); err != nil {
		tmpFile.Close()
		return fmt.Errorf("error setting permissions on %s: %w", tmpFile.Name(), err)
	}
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		return fmt.Errorf("error syncing %s: %w", tmpFile.Name(), err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("error closing %s: %w", tmpFile.Name(), err)
	}

	if err := os.Rename(tmpFile.Name(), targetPath); err != nil {
		return fmt.Errorf("error renaming %s to %s: %w", tmpFile.Name(), targetPath, err)
	}

	// Fsync the directory so the rename itself survives a crash. Best-effort:
	// a failure here does not undo an otherwise successful write.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}

	return nil
}
