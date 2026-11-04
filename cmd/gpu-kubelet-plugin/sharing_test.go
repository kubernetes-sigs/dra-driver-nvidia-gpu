/*
 * Copyright 2026 The Kubernetes Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// mockFileChecker implements fileChecker for tests.
// existingPath is the single path Stat should report as existing; empty means nothing exists.
type mockFileChecker struct {
	existingPath string
}

func (m *mockFileChecker) Stat(path string) error {
	if path == m.existingPath {
		return nil
	}
	return errors.New("not found")
}

func TestSetMpsShmMountPath(t *testing.T) {
	testCases := map[string]struct {
		existingPath      string
		expectedMountPath string
	}{
		// /dev/shm exists under the driver root → daemon uses chroot → shm at <driverRootMountDir>/dev/shm.
		"dev/shm exists under driver root": {
			existingPath:      filepath.Join(driverRootMountDir, "dev", "shm"),
			expectedMountPath: filepath.Join(driverRootMountDir, "dev", "shm"),
		},
		// /dev/shm not present under driver root (e.g. GKE COS) → daemon runs directly
		// in the container namespace → shm at /dev/shm.
		"dev/shm does not exist under driver root — case for GKE COS": {
			existingPath:      "",
			expectedMountPath: MpsDefaultShmMountPath,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			checker := &mockFileChecker{existingPath: tc.existingPath}
			require.Equal(t, tc.expectedMountPath, setMpsShmMountPath(checker))
		})
	}
}

func TestRenderMpsControlDaemonDeploymentImagePullSettings(t *testing.T) {
	deployment, err := renderMpsControlDaemonDeployment(
		filepath.Join("..", "..", "templates", "mps-control-daemon.tmpl.yaml"),
		MpsControlDaemonTemplateData{
			NodeName:                  "node-a",
			MpsControlDaemonNamespace: "dra-driver-nvidia-gpu",
			MpsControlDaemonName:      "mps-control-daemon-test",
			CUDA_VISIBLE_DEVICES:      "GPU-0",
			NvidiaDriverRoot:          "/",
			MpsShmDirectory:           "/var/lib/kubelet/plugins/gpu.nvidia.com/mps/test/shm",
			MpsPipeDirectory:          "/var/lib/kubelet/plugins/gpu.nvidia.com/mps/test/pipe",
			MpsLogDirectory:           "/var/lib/kubelet/plugins/gpu.nvidia.com/mps/test/log",
			MpsImageName:              "registry.example.com/dra-driver:dev",
			MpsImagePullPolicy:        "Always",
			MpsImagePullSecretNames:   []string{"regcred", "mirrorcred"},
			MpsShmMountPath:           MpsDefaultShmMountPath,
		},
	)
	require.NoError(t, err)

	require.Equal(t, []corev1.LocalObjectReference{
		{Name: "regcred"},
		{Name: "mirrorcred"},
	}, deployment.Spec.Template.Spec.ImagePullSecrets)
	require.Len(t, deployment.Spec.Template.Spec.Containers, 1)
	require.Equal(t, corev1.PullAlways, deployment.Spec.Template.Spec.Containers[0].ImagePullPolicy)
}

func TestGetDefaultShmSize(t *testing.T) {
	const fallbackSize = "65536k"

	writeMeminfo := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "meminfo")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
		return path
	}

	t.Run("half of MemTotal is returned, keeping the unit", func(t *testing.T) {
		path := writeMeminfo(t, "MemTotal:       16316296 kB\nMemFree:         1000000 kB\n")

		// 16316296 / 2, with "kB" shortened to the "k" that mount(8) expects.
		require.Equal(t, "8158148k", getDefaultShmSize(path))
	})

	t.Run("preceding lines are skipped", func(t *testing.T) {
		path := writeMeminfo(t, "MemAvailable:    2000 kB\nSwapTotal:       4000 kB\nMemTotal:        1024 kB\n")

		require.Equal(t, "512k", getDefaultShmSize(path))
	})

	t.Run("an odd MemTotal is rounded down", func(t *testing.T) {
		path := writeMeminfo(t, "MemTotal:        1025 kB\n")

		require.Equal(t, "512k", getDefaultShmSize(path))
	})

	t.Run("a MemTotal without a unit is returned unitless", func(t *testing.T) {
		path := writeMeminfo(t, "MemTotal:        2048\n")

		require.Equal(t, "1024", getDefaultShmSize(path))
	})

	t.Run("only a MemTotal: prefix is matched", func(t *testing.T) {
		path := writeMeminfo(t, "MemTotalHuge:    2048 kB\n")

		require.Equal(t, fallbackSize, getDefaultShmSize(path), "no MemTotal line means the fallback")
	})

	for name, content := range map[string]string{
		"an unparseable MemTotal": "MemTotal:       not-a-number kB\n",
		"an empty MemTotal":       "MemTotal:\n",
		"a missing MemTotal":      "MemFree:         1000000 kB\n",
		"an empty file":           "",
	} {
		t.Run(name+" falls back", func(t *testing.T) {
			require.Equal(t, fallbackSize, getDefaultShmSize(writeMeminfo(t, content)))
		})
	}

	t.Run("an unreadable file falls back", func(t *testing.T) {
		require.Equal(t, fallbackSize, getDefaultShmSize(filepath.Join(t.TempDir(), "absent")))
	})
}
