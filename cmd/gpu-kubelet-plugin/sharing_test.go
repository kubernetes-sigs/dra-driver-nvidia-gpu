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
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

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

const mpsTemplatePath = "../../templates/mps-control-daemon.tmpl.yaml"

func renderMpsTestDeployment(t *testing.T, chroot bool) *corev1.Container {
	t.Helper()
	deployment, err := renderMpsControlDaemonDeployment(mpsTemplatePath, MpsControlDaemonTemplateData{
		NodeName:                        "node-a",
		MpsControlDaemonNamespace:       "dra-driver-nvidia-gpu",
		MpsControlDaemonName:            "mps-control-daemon-test",
		CUDA_VISIBLE_DEVICES:            "GPU-0",
		DefaultActiveThreadPercentage:   "50",
		DefaultPinnedDeviceMemoryLimits: map[string]string{"GPU-0": "8192M"},
		NvidiaDriverRoot:                "/usr/local",
		MpsShmDirectory:                 "/var/lib/kubelet/plugins/gpu.nvidia.com/mps/test/shm",
		MpsPipeDirectory:                "/var/lib/kubelet/plugins/gpu.nvidia.com/mps/test/pipe",
		MpsLogDirectory:                 "/var/lib/kubelet/plugins/gpu.nvidia.com/mps/test/log",
		MpsImageName:                    "registry.example.com/dra-driver:dev",
		ChrootDriverRoot:                chroot,
	})
	require.NoError(t, err)
	require.Len(t, deployment.Spec.Template.Spec.Containers, 1)
	return &deployment.Spec.Template.Spec.Containers[0]
}

func mountPaths(c *corev1.Container) map[string]corev1.VolumeMount {
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range c.VolumeMounts {
		mounts[m.Name] = m
	}
	return mounts
}

func TestDriverRootHasShell(t *testing.T) {
	testCases := map[string]struct {
		files    map[string]os.FileMode
		dirs     []string
		expected bool
	}{
		"bin/sh executable":       {files: map[string]os.FileMode{"bin/sh": 0o755}, expected: true},
		"usr/bin/sh executable":   {files: map[string]os.FileMode{"usr/bin/sh": 0o755}, expected: true},
		"sh not executable":       {files: map[string]os.FileMode{"bin/sh": 0o644}},
		"sh is a directory":       {dirs: []string{"bin/sh"}},
		"dev/shm without a shell": {dirs: []string{"dev/shm"}},
		"empty driver root":       {},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for _, d := range tc.dirs {
				require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o755))
			}
			for f, mode := range tc.files {
				require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, f)), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(root, f), nil, mode))
			}
			require.Equal(t, tc.expected, driverRootHasShell(root))
		})
	}
}

func TestRenderMpsControlDaemonDeploymentStateMounts(t *testing.T) {
	testCases := map[string]struct {
		chroot           bool
		stateRoot        string
		driverRootRO     bool
		expectedRunMode  string
		unexpectedScript []string
	}{
		"chroot into driver root": {
			chroot:           true,
			stateRoot:        "/driver-root",
			expectedRunMode:  `RUN="chroot /driver-root sh -c"`,
			unexpectedScript: []string{"LD_LIBRARY_PATH"},
		},
		"no shell in driver root": {
			chroot:           false,
			stateRoot:        "",
			driverRootRO:     true,
			expectedRunMode:  `RUN="sh -c"`,
			unexpectedScript: []string{"chroot", "/driver-root/tmp", "/driver-root/var"},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			c := renderMpsTestDeployment(t, tc.chroot)
			mounts := mountPaths(c)

			require.Equal(t, "/driver-root", mounts["driver-root"].MountPath)
			require.Equal(t, tc.driverRootRO, mounts["driver-root"].ReadOnly)
			require.Equal(t, tc.stateRoot+"/dev/shm", mounts["mps-shm-directory"].MountPath)
			require.Equal(t, tc.stateRoot+"/tmp/nvidia-mps", mounts["mps-pipe-directory"].MountPath)
			require.Equal(t, tc.stateRoot+"/var/log/nvidia-mps", mounts["mps-log-directory"].MountPath)
			for _, m := range []string{"mps-shm-directory", "mps-pipe-directory", "mps-log-directory"} {
				require.False(t, mounts[m].ReadOnly, m)
			}

			logDir := mounts["mps-log-directory"].MountPath
			require.Equal(t, []string{"cat", logDir + "/startup.log"}, c.StartupProbe.Exec.Command)

			require.Equal(t, []string{"sh", "-c"}, c.Command)
			require.Len(t, c.Args, 1)
			script := c.Args[0]
			require.Contains(t, script, tc.expectedRunMode)
			require.Contains(t, script, `> `+logDir+`/startup.log`)
			require.Contains(t, script, `tail -n +1 -f `+logDir+`/control.log`)
			for _, s := range tc.unexpectedScript {
				require.NotContains(t, script, s)
			}
		})
	}
}

// TestMpsControlDaemonRunnerWithoutDriverRootShell executes the rendered
// no-chroot runner against a fake driver root to check that only NVIDIA
// libraries reach the loader and that MPS state lands in the mounted log dir.
func TestMpsControlDaemonRunnerWithoutDriverRootShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	c := renderMpsTestDeployment(t, false)
	tmp := t.TempDir()
	driverRoot := filepath.Join(tmp, "driver-root")
	libDir := filepath.Join(tmp, "nvidia-libs")
	logDir := filepath.Join(tmp, "log")
	record := filepath.Join(tmp, "record")
	require.NoError(t, os.MkdirAll(logDir, 0o755))

	script := strings.NewReplacer(
		"/driver-root", driverRoot,
		"/tmp/nvidia-libs", libDir,
		"/var/log/nvidia-mps", logDir,
	).Replace(c.Args[0])

	writeFile := func(rel, content string, mode os.FileMode) {
		p := filepath.Join(driverRoot, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), mode))
	}
	symlink := func(target, rel string) {
		p := filepath.Join(driverRoot, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.Symlink(target, p))
	}

	writeFile("usr/lib64/libcuda.so.580.95.05", "", 0o644)
	symlink("libcuda.so.580.95.05", "usr/lib64/libcuda.so.1")
	symlink("libcuda.so.1", "usr/lib64/libcuda.so")
	writeFile("usr/lib64/libnvidia-ml.so.580.95.05", "", 0o644)
	symlink("libnvidia-ml.so.580.95.05", "usr/lib64/libnvidia-ml.so.1")
	writeFile("lib/x86_64-linux-gnu/libnvidia-ml.so.1", "", 0o644)
	writeFile("lib/aarch64-linux-gnu/libnvidia-ptxjitcompiler.so.1", "", 0o644)
	symlink("libnvidia-missing.so.1.0", "usr/lib/libnvidia-dangling.so.1")
	for _, hostLib := range []string{"libc.so.6", "ld-linux-x86-64.so.2", "ld-linux-aarch64.so.1", "libm.so.6", "libpthread.so.0"} {
		writeFile("usr/lib64/"+hostLib, "", 0o644)
		writeFile("lib/"+hostLib, "", 0o644)
	}
	fakeTool := `#!/bin/sh
printf '%s|%s|%s\n' "$(basename "$0")" "$*" "$LD_LIBRARY_PATH" >> ` + record + `
[ -t 0 ] || cat >> ` + record + `
echo "$PATH" > ` + record + `.path
`
	writeFile("usr/bin/nvidia-cuda-mps-control", fakeTool, 0o755)
	writeFile("usr/bin/tail", fakeTool, 0o755)

	cmd := exec.Command("sh", "-c", script)
	cmd.Stdin = strings.NewReader("")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LD_LIBRARY_PATH=/host/leak"}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	entries, err := os.ReadDir(libDir)
	require.NoError(t, err)
	links := map[string]string{}
	var names []string
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(libDir, e.Name()))
		require.NoError(t, err)
		links[e.Name()] = target
		names = append(names, e.Name())
	}
	sort.Strings(names)
	require.Equal(t, []string{
		"libcuda.so",
		"libcuda.so.1",
		"libcuda.so.580.95.05",
		"libnvidia-ml.so.1",
		"libnvidia-ml.so.580.95.05",
		"libnvidia-ptxjitcompiler.so.1",
	}, names)
	require.Equal(t, filepath.Join(driverRoot, "usr/lib64/libnvidia-ml.so.1"), links["libnvidia-ml.so.1"])
	require.Equal(t, filepath.Join(driverRoot, "lib/aarch64-linux-gnu/libnvidia-ptxjitcompiler.so.1"), links["libnvidia-ptxjitcompiler.so.1"])
	_, err = os.Stat(filepath.Join(libDir, "libcuda.so"))
	require.NoError(t, err, "versioned symlink chain must resolve")

	recorded, err := os.ReadFile(record)
	require.NoError(t, err)
	require.Equal(t, strings.Join([]string{
		"nvidia-cuda-mps-control|-d|" + libDir,
		"nvidia-cuda-mps-control||" + libDir,
		"set_default_active_thread_percentage 50",
		"nvidia-cuda-mps-control||" + libDir,
		"set_default_device_pinned_mem_limit GPU-0 8192M",
		"tail|-n +1 -f " + logDir + "/control.log|" + libDir,
	}, "\n")+"\n", string(recorded))

	path, err := os.ReadFile(record + ".path")
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(strings.TrimSpace(string(path)), ":"+os.Getenv("PATH")), string(path))

	startup, err := os.ReadFile(filepath.Join(logDir, "startup.log"))
	require.NoError(t, err)
	require.Equal(t, "startup complete\n", string(startup))
}
