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

const mpsTestShmDirectory = "/var/lib/kubelet/plugins/gpu.nvidia.com/mps/test/shm"

func renderMpsTestDeployment(t *testing.T, chroot bool) *corev1.PodSpec {
	t.Helper()
	deployment, err := renderMpsControlDaemonDeployment(mpsTemplatePath, MpsControlDaemonTemplateData{
		NodeName:                        "node-a",
		MpsControlDaemonNamespace:       "dra-driver-nvidia-gpu",
		MpsControlDaemonName:            "mps-control-daemon-test",
		CUDA_VISIBLE_DEVICES:            "GPU-0",
		DefaultActiveThreadPercentage:   "50",
		DefaultPinnedDeviceMemoryLimits: map[string]string{"GPU-0": "8192M"},
		NvidiaDriverRoot:                "/usr/local",
		MpsShmDirectory:                 mpsTestShmDirectory,
		MpsPipeDirectory:                "/var/lib/kubelet/plugins/gpu.nvidia.com/mps/test/pipe",
		MpsLogDirectory:                 "/var/lib/kubelet/plugins/gpu.nvidia.com/mps/test/log",
		MpsImageName:                    "registry.example.com/dra-driver:dev",
		ChrootDriverRoot:                chroot,
	})
	require.NoError(t, err)
	require.Len(t, deployment.Spec.Template.Spec.Containers, 1)
	return &deployment.Spec.Template.Spec
}

// mountsByPath indexes mounts by container path. A volume may be mounted more
// than once, so indexing by volume name would silently drop mounts.
func mountsByPath(t *testing.T, c *corev1.Container) map[string]corev1.VolumeMount {
	t.Helper()
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range c.VolumeMounts {
		_, dup := mounts[m.MountPath]
		require.False(t, dup, "duplicate mount path %s", m.MountPath)
		mounts[m.MountPath] = m
	}
	return mounts
}

func mountPathsOf(c *corev1.Container, volume string) []string {
	var paths []string
	for _, m := range c.VolumeMounts {
		if m.Name == volume {
			paths = append(paths, m.MountPath)
		}
	}
	sort.Strings(paths)
	return paths
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
		chroot       bool
		stateRoot    string
		driverRootRO bool
		shmPaths     []string
	}{
		"chroot into driver root": {
			chroot:    true,
			stateRoot: "/driver-root",
			shmPaths:  []string{"/dev/shm", "/driver-root/dev/shm"},
		},
		"no shell in driver root": {
			chroot:       false,
			stateRoot:    "",
			driverRootRO: true,
			shmPaths:     []string{"/dev/shm"},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			spec := renderMpsTestDeployment(t, tc.chroot)
			c := &spec.Containers[0]
			mounts := mountsByPath(t, c)

			require.Equal(t, "driver-root", mounts["/driver-root"].Name)
			require.Equal(t, tc.driverRootRO, mounts["/driver-root"].ReadOnly)
			require.Equal(t, tc.shmPaths, mountPathsOf(c, "mps-shm-directory"))
			require.Equal(t, []string{tc.stateRoot + "/tmp/nvidia-mps"}, mountPathsOf(c, "mps-pipe-directory"))
			require.Equal(t, []string{tc.stateRoot + "/var/log/nvidia-mps"}, mountPathsOf(c, "mps-log-directory"))
			for path, m := range mounts {
				if m.Name == "driver-root" {
					continue
				}
				require.False(t, m.ReadOnly, path)
				if !tc.chroot {
					require.False(t, strings.HasPrefix(path, "/driver-root/"), "read-only driver root must not have nested mountpoints: %s", path)
				}
			}

			var shmVolume *corev1.Volume
			for i := range spec.Volumes {
				if spec.Volumes[i].Name == "mps-shm-directory" {
					shmVolume = &spec.Volumes[i]
				}
			}
			require.NotNil(t, shmVolume)
			require.NotNil(t, shmVolume.HostPath)
			require.Equal(t, mpsTestShmDirectory, shmVolume.HostPath.Path)

			logDir := tc.stateRoot + "/var/log/nvidia-mps"
			require.Equal(t, []string{"cat", logDir + "/startup.log"}, c.StartupProbe.Exec.Command)

			require.Equal(t, []string{"sh", "-c"}, c.Command)
			require.Len(t, c.Args, 1)
			script := c.Args[0]
			require.Contains(t, script, `> `+logDir+`/startup.log`)
			require.Contains(t, script, `tail -n +1 -f `+logDir+`/control.log`)
			if !tc.chroot {
				require.NotContains(t, script, "chroot")
			}
		})
	}
}

// TestMpsControlDaemonRunner executes the rendered script against a fake
// driver root whose shell may change between rendering and pod start, with
// every rendered mount path relocated under a scratch directory.
func TestMpsControlDaemonRunner(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	testCases := map[string]struct {
		shellAtRender bool
		shellAtStart  bool
		expectChroot  bool
	}{
		"no shell in driver root":               {},
		"shell appears after shell-less render": {shellAtStart: true},
		"shell present":                         {shellAtRender: true, shellAtStart: true, expectChroot: true},
		"shell removed after render":            {shellAtRender: true},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			mnt := filepath.Join(tmp, "mnt")
			driverRoot := filepath.Join(mnt, "driver-root")
			libDir := filepath.Join(mnt, "tmp", "nvidia-libs")
			shimDir := filepath.Join(tmp, "container tools")
			record := filepath.Join(tmp, "record")

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
printf '%s|%s|%s|%s|%s\n' "$(basename "$0")" "$*" "$LD_LIBRARY_PATH" "${CUDA_MPS_PIPE_DIRECTORY-unset}" "${CUDA_MPS_LOG_DIRECTORY-unset}" >> ` + record + `
[ -t 0 ] || cat >> ` + record + `
echo "$PATH" > ` + record + `.$(basename "$0").path
`
			writeFile("usr/bin/nvidia-cuda-mps-control", fakeTool, 0o755)
			// Driver root tools other than the MPS binaries must never run
			// against the container's loader.
			poison := "#!/bin/sh\necho \"driver root $(basename \"$0\")\" >> " + record + "\nexit 1\n"
			writeFile("usr/bin/tail", poison, 0o755)
			// chroot cannot run unprivileged, and tail must not block.
			for _, tool := range []string{"chroot", "tail"} {
				require.NoError(t, os.MkdirAll(shimDir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(shimDir, tool), []byte(fakeTool), 0o755))
			}

			shell := filepath.Join(driverRoot, "bin", "sh")
			if tc.shellAtRender {
				writeFile("bin/sh", poison, 0o755)
			}
			spec := renderMpsTestDeployment(t, driverRootHasShell(driverRoot))
			c := &spec.Containers[0]
			if tc.shellAtStart {
				writeFile("bin/sh", poison, 0o755)
			} else {
				require.NoError(t, os.RemoveAll(shell))
			}

			// Relocate every rendered mount path, longest first so nested
			// mounts are not rewritten through their parent.
			paths := []string{"/tmp/nvidia-libs"}
			for _, m := range c.VolumeMounts {
				paths = append(paths, m.MountPath)
				require.NoError(t, os.MkdirAll(filepath.Join(mnt, m.MountPath), 0o755))
			}
			sort.Slice(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })
			var oldnew []string
			for _, p := range paths {
				oldnew = append(oldnew, p, filepath.Join(mnt, p))
			}
			script := strings.NewReplacer(oldnew...).Replace(c.Args[0])

			pipePaths := mountPathsOf(c, "mps-pipe-directory")
			logPaths := mountPathsOf(c, "mps-log-directory")
			require.Len(t, pipePaths, 1)
			require.Len(t, logPaths, 1)
			pipeDir := filepath.Join(mnt, pipePaths[0])
			logDir := filepath.Join(mnt, logPaths[0])

			basePath := shimDir + ":" + os.Getenv("PATH")
			cmd := exec.Command("sh", "-c", script)
			cmd.Stdin = strings.NewReader("")
			cmd.Env = []string{"PATH=" + basePath, "LD_LIBRARY_PATH=/host/leak"}
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, string(out))

			recorded, err := os.ReadFile(record)
			require.NoError(t, err)
			tailPath, err := os.ReadFile(record + ".tail.path")
			require.NoError(t, err)
			require.Equal(t, basePath, strings.TrimSpace(string(tailPath)))
			startup, err := os.ReadFile(filepath.Join(logDir, "startup.log"))
			require.NoError(t, err)
			require.Equal(t, "startup complete\n", string(startup))

			if tc.expectChroot {
				env := "|/host/leak|unset|unset"
				run := "chroot|" + driverRoot + " sh -c "
				require.Equal(t, strings.Join([]string{
					run + "nvidia-cuda-mps-control -d" + env,
					run + "echo set_default_active_thread_percentage 50 | nvidia-cuda-mps-control" + env,
					run + "echo set_default_device_pinned_mem_limit GPU-0 8192M | nvidia-cuda-mps-control" + env,
					"tail|-n +1 -f " + logDir + "/control.log" + env,
				}, "\n")+"\n", string(recorded))
				require.NoDirExists(t, libDir)
				return
			}

			env := "|" + libDir + "|" + pipeDir + "|" + logDir
			require.Equal(t, strings.Join([]string{
				"nvidia-cuda-mps-control|-d" + env,
				"nvidia-cuda-mps-control|" + env,
				"set_default_active_thread_percentage 50",
				"nvidia-cuda-mps-control|" + env,
				"set_default_device_pinned_mem_limit GPU-0 8192M",
				"tail|-n +1 -f " + logDir + "/control.log" + env,
			}, "\n")+"\n", string(recorded))
			mpsPath, err := os.ReadFile(record + ".nvidia-cuda-mps-control.path")
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(string(mpsPath), filepath.Join(driverRoot, "usr/bin")+":"), string(mpsPath))
			require.True(t, strings.HasSuffix(strings.TrimSpace(string(mpsPath)), ":"+basePath), string(mpsPath))

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
		})
	}
}
