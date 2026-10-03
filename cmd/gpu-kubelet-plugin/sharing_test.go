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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
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

// fakeUUIDProvider is a static UUIDProvider. gpus and migDevices are returned
// as-is; UUIDs() returns their concatenation.
type fakeUUIDProvider struct {
	gpus       []string
	migDevices []string
}

func (f fakeUUIDProvider) UUIDs() []string {
	return append(append([]string{}, f.gpus...), f.migDevices...)
}

func (f fakeUUIDProvider) GpuUUIDs() []string { return f.gpus }

func (f fakeUUIDProvider) MigDeviceUUIDs() []string { return f.migDevices }

// testConfig builds a Config backed by clientset. Fields not set here are the
// ones sharing.go never reads.
func testConfig(clientset kubernetes.Interface, pluginsDir string) *Config {
	return &Config{
		flags: &Flags{
			nodeName:                    "node-a",
			namespace:                   "dra-driver-nvidia-gpu",
			imageName:                   "registry.example.com/dra-driver:dev",
			serviceAccountName:          "mps-sa",
			kubeletPluginsDirectoryPath: pluginsDir,
		},
		clientsets:           pkgflags.ClientSets{Core: clientset},
		imagePullPolicy:      "Always",
		imagePullSecretNames: []string{"regcred"},
	}
}

// mpsDeployment returns a Deployment as IsControlDaemonStarted/Stopped and
// AssertReady look it up: by the name derived from id.
func mpsDeployment(namespace, id string, readyReplicas int32, selector map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf(MpsControlDaemonNameFmt, id),
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: selector},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: readyReplicas},
	}
}

// templatePath is the real MPS control daemon template shipped with the driver.
func templatePath() string {
	return filepath.Join("..", "..", "templates", "mps-control-daemon.tmpl.yaml")
}

func TestOsFileCheckerStat(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "present")
	require.NoError(t, os.WriteFile(existing, []byte{}, 0o644))

	require.NoError(t, osFileChecker{}.Stat(existing))
	require.NoError(t, osFileChecker{}.Stat(dir), "a directory is reported as existing too")
	require.Error(t, osFileChecker{}.Stat(filepath.Join(dir, "absent")))
}

func TestNewTimeSlicingManager(t *testing.T) {
	l := &deviceLib{nvidiaSMIPath: "/usr/bin/nvidia-smi"}

	require.Same(t, l, NewTimeSlicingManager(l).nvdevlib)
}

func TestTimeSlicingManagerSetTimeSlice(t *testing.T) {
	t.Run("compute mode is reset to DEFAULT before the time slice is set", func(t *testing.T) {
		t.Setenv("LD_PRELOAD", "")
		smi := newFakeSMI(t, 0)
		m := NewTimeSlicingManager(&deviceLib{nvidiaSMIPath: smi.path, driverLibraryPath: smi.libPath})

		err := m.SetTimeSlice(
			[]string{"GPU-1", "GPU-2"},
			&configapi.TimeSlicingConfig{Interval: ptr.To(configapi.LongTimeSlice)},
		)

		require.NoError(t, err)
		// Both GPUs are set to DEFAULT first; only then is the time slice applied.
		require.Equal(t, []string{
			smi.libPath + "|-i GPU-1 -c DEFAULT",
			smi.libPath + "|-i GPU-2 -c DEFAULT",
			smi.libPath + "|compute-policy -i GPU-1 --set-timeslice 3",
			smi.libPath + "|compute-policy -i GPU-2 --set-timeslice 3",
		}, smi.invocations(t))
	})

	t.Run("each interval maps to its nvidia-smi value", func(t *testing.T) {
		for interval, want := range map[configapi.TimeSliceInterval]string{
			configapi.DefaultTimeSlice: "0",
			configapi.ShortTimeSlice:   "1",
			configapi.MediumTimeSlice:  "2",
			configapi.LongTimeSlice:    "3",
		} {
			t.Run(string(interval), func(t *testing.T) {
				t.Setenv("LD_PRELOAD", "")
				smi := newFakeSMI(t, 0)
				m := NewTimeSlicingManager(&deviceLib{nvidiaSMIPath: smi.path, driverLibraryPath: smi.libPath})

				require.NoError(t, m.SetTimeSlice([]string{"GPU-1"}, &configapi.TimeSlicingConfig{Interval: ptr.To(interval)}))

				require.Equal(t,
					[]string{smi.libPath + "|compute-policy -i GPU-1 --set-timeslice " + want},
					smi.invocations(t)[1:],
				)
			})
		}
	})

	t.Run("an empty UUID list invokes nothing", func(t *testing.T) {
		smi := newFakeSMI(t, 0)
		m := NewTimeSlicingManager(&deviceLib{nvidiaSMIPath: smi.path, driverLibraryPath: smi.libPath})

		require.NoError(t, m.SetTimeSlice(nil, &configapi.TimeSlicingConfig{Interval: ptr.To(configapi.ShortTimeSlice)}))

		require.Empty(t, smi.invocations(t))
	})

	t.Run("a failing nvidia-smi aborts before the time slice is set", func(t *testing.T) {
		smi := newFakeSMI(t, 1)
		m := NewTimeSlicingManager(&deviceLib{nvidiaSMIPath: smi.path, driverLibraryPath: smi.libPath})

		err := m.SetTimeSlice([]string{"GPU-1"}, &configapi.TimeSlicingConfig{Interval: ptr.To(configapi.ShortTimeSlice)})

		require.ErrorContains(t, err, "error setting compute mode")
		require.Len(t, smi.invocations(t), 1, "the time slice must not be applied after a compute mode failure")
	})

	t.Run("a failing time slice invocation is reported", func(t *testing.T) {
		t.Setenv("LD_PRELOAD", "")
		dir := t.TempDir()
		smiPath := filepath.Join(dir, "nvidia-smi")
		libPath := filepath.Join(dir, "libnvidia-ml.so.1")
		// Succeed for the compute mode reset, fail for the time slice.
		script := "#!/bin/sh\ncase \"$1\" in compute-policy) exit 1 ;; esac\nexit 0\n"
		require.NoError(t, os.WriteFile(smiPath, []byte(script), 0o755))
		require.NoError(t, os.WriteFile(libPath, []byte{}, 0o644))
		m := NewTimeSlicingManager(&deviceLib{nvidiaSMIPath: smiPath, driverLibraryPath: libPath})

		err := m.SetTimeSlice([]string{"GPU-1"}, &configapi.TimeSlicingConfig{Interval: ptr.To(configapi.ShortTimeSlice)})

		require.ErrorContains(t, err, "error setting time slice")
	})
}

func TestNewMpsManager(t *testing.T) {
	config := testConfig(k8sfake.NewSimpleClientset(), "/var/lib/kubelet/plugins")
	l := &deviceLib{}

	m := NewMpsManager(config, l, "/run/nvidia/driver", "/templates/mps.tmpl.yaml")

	// The control files live under <kubelet plugins dir>/<driver name>/mps.
	require.Equal(t, filepath.Join("/var/lib/kubelet/plugins", DriverName, MpsControlFilesDirName), m.controlFilesRoot)
	require.Equal(t, "/run/nvidia/driver", m.hostDriverRoot)
	require.Equal(t, "/templates/mps.tmpl.yaml", m.templatePath)
	require.Same(t, config, m.config)
	require.Same(t, l, m.nvdevlib)
}

func TestGetMpsControlDaemonID(t *testing.T) {
	m := NewMpsManager(testConfig(k8sfake.NewSimpleClientset(), "/plugins"), nil, "/", "")
	devices := fakeUUIDProvider{gpus: []string{"GPU-1", "GPU-2"}}

	id := m.GetMpsControlDaemonID("claim-uid", devices)

	// <claim UID>-<first 5 hex characters of the SHA256 over the joined UUIDs>.
	sum := sha256.Sum256([]byte("GPU-1,GPU-2"))
	require.Equal(t, "claim-uid-"+hex.EncodeToString(sum[:])[:5], id)

	t.Run("the ID is stable across calls", func(t *testing.T) {
		require.Equal(t, id, m.GetMpsControlDaemonID("claim-uid", devices))
	})

	t.Run("a different claim UID yields a different ID", func(t *testing.T) {
		require.NotEqual(t, id, m.GetMpsControlDaemonID("other-uid", devices))
	})

	t.Run("a different device set yields a different ID", func(t *testing.T) {
		other := fakeUUIDProvider{gpus: []string{"GPU-1", "GPU-3"}}
		require.NotEqual(t, id, m.GetMpsControlDaemonID("claim-uid", other))
	})

	t.Run("the device order is significant", func(t *testing.T) {
		reversed := fakeUUIDProvider{gpus: []string{"GPU-2", "GPU-1"}}
		require.NotEqual(t, id, m.GetMpsControlDaemonID("claim-uid", reversed))
	})

	t.Run("MIG devices contribute to the ID", func(t *testing.T) {
		withMig := fakeUUIDProvider{gpus: []string{"GPU-1", "GPU-2"}, migDevices: []string{"MIG-1"}}
		require.NotEqual(t, id, m.GetMpsControlDaemonID("claim-uid", withMig))
	})
}

func TestNewMpsControlDaemon(t *testing.T) {
	config := testConfig(k8sfake.NewSimpleClientset(), "/var/lib/kubelet/plugins")
	m := NewMpsManager(config, nil, "/", "")
	devices := fakeUUIDProvider{gpus: []string{"GPU-1"}}

	d := m.NewMpsControlDaemon("claim-uid", devices)

	id := m.GetMpsControlDaemonID("claim-uid", devices)
	root := filepath.Join(m.controlFilesRoot, id)

	require.Equal(t, id, d.GetID())
	require.Equal(t, id, d.id)
	require.Equal(t, "node-a", d.nodeName)
	require.Equal(t, "dra-driver-nvidia-gpu", d.namespace)
	require.Equal(t, fmt.Sprintf(MpsControlDaemonNameFmt, id), d.name)
	require.Equal(t, root, d.rootDir)
	require.Equal(t, filepath.Join(root, "pipe"), d.pipeDir)
	require.Equal(t, filepath.Join(root, "shm"), d.shmDir)
	require.Equal(t, filepath.Join(root, "log"), d.logDir)
	require.Equal(t, devices, d.devices)
	require.Same(t, m, d.manager)
}

func TestIsControlDaemonStartedAndStopped(t *testing.T) {
	const id = "claim-uid-abcde"
	const namespace = "dra-driver-nvidia-gpu"

	t.Run("the deployment exists", func(t *testing.T) {
		m := NewMpsManager(
			testConfig(k8sfake.NewSimpleClientset(mpsDeployment(namespace, id, 1, nil)), "/plugins"),
			nil, "/", "",
		)

		started, err := m.IsControlDaemonStarted(context.Background(), id)
		require.NoError(t, err)
		require.True(t, started)

		stopped, err := m.IsControlDaemonStopped(context.Background(), id)
		require.NoError(t, err)
		require.False(t, stopped)
	})

	t.Run("the deployment does not exist", func(t *testing.T) {
		m := NewMpsManager(testConfig(k8sfake.NewSimpleClientset(), "/plugins"), nil, "/", "")

		started, err := m.IsControlDaemonStarted(context.Background(), id)
		require.NoError(t, err)
		require.False(t, started)

		stopped, err := m.IsControlDaemonStopped(context.Background(), id)
		require.NoError(t, err)
		require.True(t, stopped)
	})

	t.Run("the deployment exists in a different namespace", func(t *testing.T) {
		m := NewMpsManager(
			testConfig(k8sfake.NewSimpleClientset(mpsDeployment("other", id, 1, nil)), "/plugins"),
			nil, "/", "",
		)

		started, err := m.IsControlDaemonStarted(context.Background(), id)
		require.NoError(t, err)
		require.False(t, started, "only the driver namespace is consulted")
	})

	t.Run("an API error is propagated rather than read as absence", func(t *testing.T) {
		clientset := k8sfake.NewSimpleClientset()
		clientset.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewInternalError(errors.New("boom"))
		})
		m := NewMpsManager(testConfig(clientset, "/plugins"), nil, "/", "")

		started, err := m.IsControlDaemonStarted(context.Background(), id)
		require.ErrorContains(t, err, "failed to get deployment")
		require.False(t, started)

		stopped, err := m.IsControlDaemonStopped(context.Background(), id)
		require.ErrorContains(t, err, "failed to get deployment")
		require.False(t, stopped, "an unreachable API server must not be reported as stopped")
	})
}

func TestRenderMpsControlDaemonDeployment(t *testing.T) {
	// fullData exercises every optional block of the template.
	fullData := MpsControlDaemonTemplateData{
		NodeName:                        "node-a",
		MpsControlDaemonNamespace:       "dra-driver-nvidia-gpu",
		MpsControlDaemonName:            "mps-control-daemon-claim-abcde",
		CUDA_VISIBLE_DEVICES:            "GPU-1,GPU-2",
		DefaultActiveThreadPercentage:   "50",
		DefaultPinnedDeviceMemoryLimits: map[string]string{"GPU-1": "1G"},
		MultiUser:                       true,
		NvidiaDriverRoot:                "/run/nvidia/driver",
		MpsShmDirectory:                 "/plugins/mps/claim-abcde/shm",
		MpsPipeDirectory:                "/plugins/mps/claim-abcde/pipe",
		MpsLogDirectory:                 "/plugins/mps/claim-abcde/log",
		MpsImageName:                    "registry.example.com/dra-driver:dev",
		MpsImagePullPolicy:              "IfNotPresent",
		MpsImagePullSecretNames:         []string{"regcred"},
		ServiceAccountName:              "mps-sa",
		FeatureGates:                    map[string]bool{"SomeGate": true},
		MpsShmMountPath:                 filepath.Join(driverRootMountDir, "dev", "shm"),
	}

	t.Run("the rendered deployment carries the template data", func(t *testing.T) {
		deployment, err := renderMpsControlDaemonDeployment(templatePath(), fullData)
		require.NoError(t, err)

		require.Equal(t, "mps-control-daemon-claim-abcde", deployment.Name)
		require.Equal(t, "dra-driver-nvidia-gpu", deployment.Namespace)
		require.Equal(t, map[string]string{"app": "mps-control-daemon-claim-abcde"}, deployment.Labels)
		require.Equal(t, int32(1), *deployment.Spec.Replicas)
		require.Equal(t,
			map[string]string{"app": "mps-control-daemon-claim-abcde"},
			deployment.Spec.Selector.MatchLabels,
			"AssertReady lists pods with this selector",
		)

		podSpec := deployment.Spec.Template.Spec
		require.Equal(t, "node-a", podSpec.NodeName)
		require.True(t, podSpec.HostPID)
		require.Equal(t, "mps-sa", podSpec.ServiceAccountName)

		require.Len(t, podSpec.Containers, 1)
		container := podSpec.Containers[0]
		require.Equal(t, "registry.example.com/dra-driver:dev", container.Image)
		require.Equal(t, corev1.PullIfNotPresent, container.ImagePullPolicy)
		require.True(t, *container.SecurityContext.Privileged)

		require.Equal(t, []corev1.EnvVar{
			{Name: "CUDA_VISIBLE_DEVICES", Value: "GPU-1,GPU-2"},
			{Name: "FEATURE_GATES", Value: "SomeGate=true,"},
		}, container.Env)

		// Volumes are wired to the host directories the plugin created.
		hostPaths := map[string]string{}
		for _, v := range podSpec.Volumes {
			require.NotNil(t, v.HostPath, "volume %q is expected to be a hostPath", v.Name)
			hostPaths[v.Name] = v.HostPath.Path
		}
		require.Equal(t, map[string]string{
			"driver-root":        "/run/nvidia/driver",
			"mps-shm-directory":  "/plugins/mps/claim-abcde/shm",
			"mps-pipe-directory": "/plugins/mps/claim-abcde/pipe",
			"mps-log-directory":  "/plugins/mps/claim-abcde/log",
		}, hostPaths)

		mountPaths := map[string]string{}
		for _, vm := range container.VolumeMounts {
			mountPaths[vm.Name] = vm.MountPath
		}
		require.Equal(t, filepath.Join(driverRootMountDir, "dev", "shm"), mountPaths["mps-shm-directory"])

		require.Len(t, container.Args, 1)
		args := container.Args[0]
		require.Contains(t, args, "nvidia-cuda-mps-control -d -M", "MultiUser adds the -M flag")
		require.Contains(t, args, "set_default_active_thread_percentage 50")
		require.Contains(t, args, "set_default_device_pinned_mem_limit GPU-1 1G")
	})

	t.Run("optional settings are omitted when unset", func(t *testing.T) {
		data := fullData
		data.MultiUser = false
		data.ServiceAccountName = ""
		data.MpsImagePullPolicy = ""
		data.MpsImagePullSecretNames = nil
		data.DefaultActiveThreadPercentage = ""
		data.DefaultPinnedDeviceMemoryLimits = nil
		data.FeatureGates = nil

		deployment, err := renderMpsControlDaemonDeployment(templatePath(), data)
		require.NoError(t, err)

		podSpec := deployment.Spec.Template.Spec
		require.Empty(t, podSpec.ServiceAccountName)
		require.Empty(t, podSpec.ImagePullSecrets)
		require.Empty(t, podSpec.Containers[0].ImagePullPolicy)
		require.Equal(t,
			[]corev1.EnvVar{{Name: "CUDA_VISIBLE_DEVICES", Value: "GPU-1,GPU-2"}},
			podSpec.Containers[0].Env,
		)

		args := podSpec.Containers[0].Args[0]
		require.Contains(t, args, `nvidia-cuda-mps-control -d"`)
		require.NotContains(t, args, "-M")
		require.NotContains(t, args, "set_default_active_thread_percentage")
		require.NotContains(t, args, "set_default_device_pinned_mem_limit")
	})

	t.Run("pinned memory limits are rendered for every device", func(t *testing.T) {
		data := fullData
		data.DefaultPinnedDeviceMemoryLimits = map[string]string{"GPU-1": "1G", "GPU-2": "2G"}

		deployment, err := renderMpsControlDaemonDeployment(templatePath(), data)
		require.NoError(t, err)

		args := deployment.Spec.Template.Spec.Containers[0].Args[0]
		require.Contains(t, args, "set_default_device_pinned_mem_limit GPU-1 1G")
		require.Contains(t, args, "set_default_device_pinned_mem_limit GPU-2 2G")
	})

	t.Run("a missing template file is an error", func(t *testing.T) {
		_, err := renderMpsControlDaemonDeployment(filepath.Join(t.TempDir(), "absent.tmpl.yaml"), fullData)

		require.ErrorContains(t, err, "failed to parse template file")
	})

	t.Run("a template referencing an unknown field is an error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad-field.tmpl.yaml")
		require.NoError(t, os.WriteFile(path, []byte("name: {{ .NoSuchField }}\n"), 0o644))

		_, err := renderMpsControlDaemonDeployment(path, fullData)

		require.ErrorContains(t, err, "failed to execute template")
	})

	t.Run("a template rendering invalid YAML is an error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad-yaml.tmpl.yaml")
		require.NoError(t, os.WriteFile(path, []byte("name: {{ .NodeName }}\n  bad: indent\n"), 0o644))

		_, err := renderMpsControlDaemonDeployment(path, fullData)

		require.ErrorContains(t, err, "failed to unmarshal yaml")
	})

	t.Run("a template rendering a mistyped field is an error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad-type.tmpl.yaml")
		require.NoError(t, os.WriteFile(path, []byte("apiVersion: apps/v1\nkind: Deployment\nspec:\n  replicas: \"{{ .NodeName }}\"\n"), 0o644))

		_, err := renderMpsControlDaemonDeployment(path, fullData)

		require.ErrorContains(t, err, "failed to convert unstructured data to typed object")
	})
}

func TestMpsControlDaemonGetCDIContainerEdits(t *testing.T) {
	m := NewMpsManager(testConfig(k8sfake.NewSimpleClientset(), "/plugins"), nil, "/", "")
	d := m.NewMpsControlDaemon("claim-uid", fakeUUIDProvider{gpus: []string{"GPU-1"}})

	edits := d.GetCDIContainerEdits()

	// The container always sees the MPS pipe directory at a fixed path; the env
	// var must agree with the mount for the CUDA runtime to find the daemon.
	require.Equal(t, []string{"CUDA_MPS_PIPE_DIRECTORY=/tmp/nvidia-mps"}, edits.Env)
	require.Equal(t, []*cdispec.Mount{
		{
			ContainerPath: "/dev/shm",
			HostPath:      d.shmDir,
			Options:       []string{"rw", "nosuid", "nodev", "bind"},
		},
		{
			ContainerPath: "/tmp/nvidia-mps",
			HostPath:      d.pipeDir,
			Options:       []string{"rw", "nosuid", "nodev", "bind"},
		},
	}, edits.Mounts)
}

func TestMpsControlDaemonStart(t *testing.T) {
	devices := fakeUUIDProvider{gpus: []string{"GPU-1"}}

	newDaemon := func(t *testing.T, clientset kubernetes.Interface, l *deviceLib, tmplPath string) *MpsControlDaemon {
		t.Helper()
		m := NewMpsManager(testConfig(clientset, t.TempDir()), l, "/run/nvidia/driver", tmplPath)
		return m.NewMpsControlDaemon("claim-uid", devices)
	}

	t.Run("an already started daemon is left alone", func(t *testing.T) {
		clientset := k8sfake.NewSimpleClientset()
		// An unusable template path proves no rendering is attempted.
		d := newDaemon(t, clientset, nil, "/nonexistent.tmpl.yaml")
		_, err := clientset.AppsV1().Deployments(d.namespace).Create(
			context.Background(), mpsDeployment(d.namespace, d.id, 1, nil), metav1.CreateOptions{},
		)
		require.NoError(t, err)

		require.NoError(t, d.Start(context.Background(), nil))

		require.NoDirExists(t, d.rootDir, "no control directories are created for a running daemon")
	})

	t.Run("a failing lookup of the existing deployment is reported", func(t *testing.T) {
		clientset := k8sfake.NewSimpleClientset()
		clientset.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewInternalError(errors.New("boom"))
		})
		d := newDaemon(t, clientset, nil, templatePath())

		err := d.Start(context.Background(), nil)

		require.ErrorContains(t, err, "error checking if control daemon already started")
	})

	t.Run("an unresolvable per-device memory limit is reported", func(t *testing.T) {
		d := newDaemon(t, k8sfake.NewSimpleClientset(), nil, templatePath())

		err := d.Start(context.Background(), &configapi.MpsConfig{
			DefaultPerDevicePinnedMemoryLimit: configapi.MpsPerDevicePinnedMemoryLimit{
				"GPU-does-not-exist": resource.MustParse("1G"),
			},
		})

		require.ErrorContains(t, err, "error transforming DefaultPerDevicePinnedMemoryLimit into string")
		require.NoDirExists(t, d.rootDir)
	})

	t.Run("multi-user mode is rejected on pre-Volta GPUs", func(t *testing.T) {
		l := &deviceLib{gpuInfosByUUID: map[string]*GpuInfo{
			"GPU-1": {UUID: "GPU-1", cudaComputeCapability: "6.1"},
		}}
		d := newDaemon(t, k8sfake.NewSimpleClientset(), l, templatePath())

		err := d.Start(context.Background(), &configapi.MpsConfig{MultiUser: ptr.To(true)})

		require.ErrorContains(t, err, "multiuser mode was requested but is not supported")
		require.NoDirExists(t, d.rootDir)
	})

	t.Run("an unrenderable template is reported", func(t *testing.T) {
		l := &deviceLib{gpuInfosByUUID: map[string]*GpuInfo{
			"GPU-1": {UUID: "GPU-1", cudaComputeCapability: "7.5"},
		}}
		d := newDaemon(t, k8sfake.NewSimpleClientset(), l, filepath.Join(t.TempDir(), "absent.tmpl.yaml"))

		err := d.Start(context.Background(), &configapi.MpsConfig{MultiUser: ptr.To(true)})

		require.ErrorContains(t, err, "failed to parse template file")
		require.NoDirExists(t, d.rootDir, "rendering happens before any directory is created")
	})
}

func TestMpsControlDaemonAssertReady(t *testing.T) {
	const namespace = "dra-driver-nvidia-gpu"
	selector := map[string]string{"app": "mps"}

	newDaemon := func(objects ...runtime.Object) *MpsControlDaemon {
		m := NewMpsManager(testConfig(k8sfake.NewSimpleClientset(objects...), "/plugins"), nil, "/", "")
		return m.NewMpsControlDaemon("claim-uid", fakeUUIDProvider{gpus: []string{"GPU-1"}})
	}

	readyPod := func(name string, ready bool, statuses int) *corev1.Pod {
		containerStatuses := make([]corev1.ContainerStatus, statuses)
		for i := range containerStatuses {
			containerStatuses[i] = corev1.ContainerStatus{Name: fmt.Sprintf("c%d", i), Ready: ready}
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: selector},
			Status:     corev1.PodStatus{ContainerStatuses: containerStatuses},
		}
	}

	t.Run("a ready deployment with a ready pod succeeds", func(t *testing.T) {
		id := newDaemon().id
		d := newDaemon(mpsDeployment(namespace, id, 1, selector), readyPod("mps-pod", true, 1))

		require.NoError(t, d.AssertReady(context.Background()))
	})

	// The failure paths each exhaust the retry backoff, so they run concurrently
	// to keep the overall test time close to that of a single case.
	failures := map[string]struct {
		// noDeployment omits the Deployment entirely.
		noDeployment  bool
		readyReplicas int32
		pods          []runtime.Object
		wantErr       string
	}{
		"the deployment is missing": {
			noDeployment: true,
			wantErr:      "failed to get deployment",
		},
		"no replica is ready": {
			readyReplicas: 0,
			pods:          []runtime.Object{readyPod("mps-pod", true, 1)},
			wantErr:       "waiting for MPS control daemon to come online",
		},
		"no pod matches the selector": {
			readyReplicas: 1,
			wantErr:       "unexpected number of pods in deployment: 0",
		},
		"more than one pod matches the selector": {
			readyReplicas: 1,
			pods:          []runtime.Object{readyPod("mps-pod-a", true, 1), readyPod("mps-pod-b", true, 1)},
			wantErr:       "unexpected number of pods in deployment: 2",
		},
		"the pod has more than one container": {
			readyReplicas: 1,
			pods:          []runtime.Object{readyPod("mps-pod", true, 2)},
			wantErr:       "unexpected number of container statuses in pod",
		},
		"the container is not ready": {
			readyReplicas: 1,
			pods:          []runtime.Object{readyPod("mps-pod", false, 1)},
			wantErr:       "control daemon not yet ready",
		},
	}

	for name, tc := range failures {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var objects []runtime.Object
			if !tc.noDeployment {
				objects = append(objects, mpsDeployment(namespace, newDaemon().id, tc.readyReplicas, selector))
			}
			objects = append(objects, tc.pods...)

			err := newDaemon(objects...).AssertReady(context.Background())

			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestMpsControlDaemonStop(t *testing.T) {
	const namespace = "dra-driver-nvidia-gpu"
	devices := fakeUUIDProvider{gpus: []string{"GPU-1"}}

	t.Run("stopping a daemon that was never started is a no-op", func(t *testing.T) {
		clientset := k8sfake.NewSimpleClientset()
		// The control files root is an empty temp dir, so rootDir does not exist.
		m := NewMpsManager(testConfig(clientset, t.TempDir()), nil, "/", "")
		d := m.NewMpsControlDaemon("claim-uid", devices)
		_, err := clientset.AppsV1().Deployments(namespace).Create(
			context.Background(), mpsDeployment(namespace, d.id, 1, nil), metav1.CreateOptions{},
		)
		require.NoError(t, err)

		require.NoError(t, d.Stop(context.Background()))

		// The deployment is untouched: Stop bails out before touching the API.
		_, err = clientset.AppsV1().Deployments(namespace).Get(context.Background(), d.name, metav1.GetOptions{})
		require.NoError(t, err)
	})

	t.Run("the deployment is deleted, the compute mode reset and the directories removed", func(t *testing.T) {
		if _, err := exec.LookPath("mount"); err != nil {
			t.Skip("the 'mount' executable is required to unmount the shm directory")
		}
		t.Setenv("LD_PRELOAD", "")

		smi := newFakeSMI(t, 0)
		clientset := k8sfake.NewSimpleClientset()
		m := NewMpsManager(
			testConfig(clientset, t.TempDir()),
			&deviceLib{nvidiaSMIPath: smi.path, driverLibraryPath: smi.libPath},
			"/", "",
		)
		d := m.NewMpsControlDaemon("claim-uid", devices)

		// shmDir is never mounted here, so CleanupMountPoint just removes it.
		require.NoError(t, os.MkdirAll(d.shmDir, 0o755))
		require.NoError(t, os.MkdirAll(d.pipeDir, 0o755))
		_, err := clientset.AppsV1().Deployments(namespace).Create(
			context.Background(), mpsDeployment(namespace, d.id, 1, nil), metav1.CreateOptions{},
		)
		require.NoError(t, err)

		require.NoError(t, d.Stop(context.Background()))

		_, err = clientset.AppsV1().Deployments(namespace).Get(context.Background(), d.name, metav1.GetOptions{})
		require.True(t, apierrors.IsNotFound(err), "expected the deployment to be deleted, got %v", err)
		// Start() puts the GPUs into EXCLUSIVE_PROCESS; Stop() must reset them.
		require.Equal(t, []string{smi.libPath + "|-i GPU-1 -c DEFAULT"}, smi.invocations(t))
		require.NoDirExists(t, d.rootDir)
	})

	t.Run("a missing deployment is not an error", func(t *testing.T) {
		if _, err := exec.LookPath("mount"); err != nil {
			t.Skip("the 'mount' executable is required to unmount the shm directory")
		}

		smi := newFakeSMI(t, 0)
		m := NewMpsManager(
			testConfig(k8sfake.NewSimpleClientset(), t.TempDir()),
			&deviceLib{nvidiaSMIPath: smi.path, driverLibraryPath: smi.libPath},
			"/", "",
		)
		d := m.NewMpsControlDaemon("claim-uid", devices)
		require.NoError(t, os.MkdirAll(d.shmDir, 0o755))

		require.NoError(t, d.Stop(context.Background()))

		require.NoDirExists(t, d.rootDir)
	})

	t.Run("a failing deployment deletion is reported", func(t *testing.T) {
		clientset := k8sfake.NewSimpleClientset()
		clientset.PrependReactor("delete", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewInternalError(errors.New("boom"))
		})
		m := NewMpsManager(testConfig(clientset, t.TempDir()), nil, "/", "")
		d := m.NewMpsControlDaemon("claim-uid", devices)
		require.NoError(t, os.MkdirAll(d.rootDir, 0o755))

		err := d.Stop(context.Background())

		require.ErrorContains(t, err, "failed to delete deployment")
		require.DirExists(t, d.rootDir, "the directories are kept so the caller can retry")
	})

	t.Run("a failing compute mode reset is reported", func(t *testing.T) {
		smi := newFakeSMI(t, 1)
		m := NewMpsManager(
			testConfig(k8sfake.NewSimpleClientset(), t.TempDir()),
			&deviceLib{nvidiaSMIPath: smi.path, driverLibraryPath: smi.libPath},
			"/", "",
		)
		d := m.NewMpsControlDaemon("claim-uid", devices)
		require.NoError(t, os.MkdirAll(d.rootDir, 0o755))

		err := d.Stop(context.Background())

		require.ErrorContains(t, err, "error resetting compute mode to DEFAULT")
	})
}
