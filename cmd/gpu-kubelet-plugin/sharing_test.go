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

func TestSetMpsPipeAndLogMountPaths(t *testing.T) {
	testCases := map[string]struct {
		existingPath     string
		expectedPipePath string
		expectedLogPath  string
	}{
		// The daemon runs in a chroot of the driver root, where MPS uses its default directories.
		"dev/shm exists under driver root": {
			existingPath:     filepath.Join(driverRootMountDir, "dev", "shm"),
			expectedPipePath: filepath.Join(driverRootMountDir, "tmp", "nvidia-mps"),
			expectedLogPath:  filepath.Join(driverRootMountDir, "var", "log", "nvidia-mps"),
		},
		// No chroot (e.g. GKE COS, Talos Linux): the driver root may be read-only and lack
		// tmp/ and var/, so the directories are mounted outside of it.
		"dev/shm does not exist under driver root": {
			existingPath:     "",
			expectedPipePath: MpsDefaultPipeMountPath,
			expectedLogPath:  MpsDefaultLogMountPath,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			checker := &mockFileChecker{existingPath: tc.existingPath}
			pipePath, logPath := setMpsPipeAndLogMountPaths(checker)
			require.Equal(t, tc.expectedPipePath, pipePath)
			require.Equal(t, tc.expectedLogPath, logPath)
		})
	}
}

func TestRenderMpsControlDaemonDeploymentMountPaths(t *testing.T) {
	deployment, err := renderMpsControlDaemonDeployment(
		filepath.Join("..", "..", "templates", "mps-control-daemon.tmpl.yaml"),
		MpsControlDaemonTemplateData{
			NodeName:                  "node-a",
			MpsControlDaemonNamespace: "dra-driver-nvidia-gpu",
			MpsControlDaemonName:      "mps-control-daemon-test",
			CUDA_VISIBLE_DEVICES:      "GPU-0",
			NvidiaDriverRoot:          "/usr/local",
			MpsShmDirectory:           "/var/lib/kubelet/plugins/gpu.nvidia.com/mps/test/shm",
			MpsPipeDirectory:          "/var/lib/kubelet/plugins/gpu.nvidia.com/mps/test/pipe",
			MpsLogDirectory:           "/var/lib/kubelet/plugins/gpu.nvidia.com/mps/test/log",
			MpsImageName:              "registry.example.com/dra-driver:dev",
			MpsShmMountPath:           MpsDefaultShmMountPath,
			MpsPipeMountPath:          MpsDefaultPipeMountPath,
			MpsLogMountPath:           MpsDefaultLogMountPath,
		},
	)
	require.NoError(t, err)

	container := deployment.Spec.Template.Spec.Containers[0]
	mounts := map[string]string{}
	for _, m := range container.VolumeMounts {
		mounts[m.Name] = m.MountPath
	}
	require.Equal(t, MpsDefaultPipeMountPath, mounts["mps-pipe-directory"])
	require.Equal(t, MpsDefaultLogMountPath, mounts["mps-log-directory"])
	require.Equal(t, []string{"cat", MpsDefaultLogMountPath + "/startup.log"}, container.StartupProbe.Exec.Command)
	require.Contains(t, container.Args[0], "export CUDA_MPS_PIPE_DIRECTORY="+MpsDefaultPipeMountPath)
	require.NotContains(t, container.Args[0], driverRootMountDir+"/var/log/nvidia-mps")
	require.Contains(t, container.Args[0], `export LD_LIBRARY_PATH="/lib/$(uname -m)-linux-gnu:/driver-root/lib64:`)
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
