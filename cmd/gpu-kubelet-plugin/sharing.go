/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"html/template"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

const (
	MpsControlFilesDirName       = "mps"
	MpsControlDaemonTemplatePath = "/templates/mps-control-daemon.tmpl.yaml"
	MpsDefaultShmMountPath       = "/dev/shm"

	// driverRootMountDir is the directory where the driver root is mounted inside the kubelet plugin container.
	driverRootMountDir = "/driver-root"
)

// fileChecker checks whether a file exists at the given path.
type fileChecker interface {
	Stat(path string) error
}

type osFileChecker struct{}

func (osFileChecker) Stat(path string) error {
	_, err := os.Stat(path)
	return err
}

type TimeSlicingManager struct {
	nvdevlib *deviceLib
}

type MpsManager struct {
	sync.Mutex
	config           *Config
	controlFilesRoot string
	hostDriverRoot   string
	templatePath     string
	daemonName       string
	pipeDir          string
	shmDir           string
	logDir           string
	nvdevlib         *deviceLib
}

type MpsControlDaemonTemplateData struct {
	NodeName                        string
	MpsControlDaemonNamespace       string
	MpsControlDaemonName            string
	CUDA_VISIBLE_DEVICES            string //nolint:stylecheck
	DefaultActiveThreadPercentage   string
	DefaultPinnedDeviceMemoryLimits map[string]string
	MultiUser                       bool
	NvidiaDriverRoot                string
	MpsShmDirectory                 string
	MpsPipeDirectory                string
	MpsLogDirectory                 string
	MpsImageName                    string
	MpsImagePullPolicy              string
	MpsImagePullSecretNames         []string
	ServiceAccountName              string
	FeatureGates                    map[string]bool
	MpsShmMountPath                 string
}

// setMpsShmMountPath returns the container path at which the MPS shm should be mounted in the MPS control daemon pod.
// If <driverRootMountDir>/dev/shm exists, the MPS daemon runs inside a chroot and shm must be mounted there.
// Otherwise (e.g. GKE COS) the daemon runs directly in the container namespace and expects /dev/shm.
func setMpsShmMountPath(checker fileChecker) string {
	chrootShmPath := filepath.Join(driverRootMountDir, "dev", "shm")
	if checker.Stat(chrootShmPath) == nil {
		return chrootShmPath
	}
	return MpsDefaultShmMountPath
}

func NewTimeSlicingManager(deviceLib *deviceLib) *TimeSlicingManager {
	return &TimeSlicingManager{
		nvdevlib: deviceLib,
	}
}

// `uuids` must be full-GPU (non-MIG) UUIDs. The caller must ensure that.
func (t *TimeSlicingManager) SetTimeSlice(uuids []string, config *configapi.TimeSlicingConfig) error {
	// Set the compute mode of the GPU to DEFAULT.
	err := t.nvdevlib.setComputeMode(uuids, "DEFAULT")
	if err != nil {
		return fmt.Errorf("error setting compute mode: %w", err)
	}

	// Set the time slice based on the config provided.
	err = t.nvdevlib.setTimeSlice(uuids, config.Interval.Int())
	if err != nil {
		return fmt.Errorf("error setting time slice: %w", err)
	}

	return nil
}

func NewMpsManager(config *Config, deviceLib *deviceLib, hostDriverRoot, templatePath string) *MpsManager {
	controlFilesRoot := filepath.Join(config.DriverPluginPath(), MpsControlFilesDirName)

	return &MpsManager{
		controlFilesRoot: controlFilesRoot,
		hostDriverRoot:   hostDriverRoot,
		templatePath:     templatePath,
		daemonName:       fmt.Sprintf("mps-control-daemon-%s", config.flags.nodeName),
		pipeDir:          filepath.Join(controlFilesRoot, "pipe"),
		shmDir:           filepath.Join(controlFilesRoot, "shm"),
		logDir:           filepath.Join(controlFilesRoot, "log"),
		config:           config,
		nvdevlib:         deviceLib,
	}
}

func (m *MpsManager) getSupportedGpuUUIDs() []string {
	var supported []string
	if m.nvdevlib != nil {
		for _, info := range m.nvdevlib.gpuInfosByUUID {
			if info != nil && info.UUID != "" {
				if err := ensureCapability(m.nvdevlib.gpuInfosByUUID, []string{info.UUID}, voltaCudaComputeCapability); err == nil {
					supported = append(supported, info.UUID)
				}
			}
		}
	}
	slices.Sort(supported)
	return supported
}

func (m *MpsManager) IsControlDaemonStarted(ctx context.Context) (bool, error) {
	_, err := m.config.clientsets.Core.AppsV1().Deployments(m.config.flags.namespace).Get(ctx, m.daemonName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to get deployment: %w", err)
	}
	return true, nil
}

func (m *MpsManager) EnsureStarted(ctx context.Context) error {
	m.Lock()
	defer m.Unlock()

	isStarted, err := m.IsControlDaemonStarted(ctx)
	if err != nil {
		return fmt.Errorf("error checking if control daemon already started: %w", err)
	}
	if isStarted {
		return nil
	}

	deviceUUIDs := m.getSupportedGpuUUIDs()
	if len(deviceUUIDs) == 0 {
		return fmt.Errorf("no Volta (>= 7.0) or newer GPUs available on node for MPS multi-user server")
	}

	klog.Infof("Starting node-level MPS control daemon '%v' for GPUs: %v", m.daemonName, deviceUUIDs)

	templateData := MpsControlDaemonTemplateData{
		NodeName:                  m.config.flags.nodeName,
		MpsControlDaemonNamespace: m.config.flags.namespace,
		MpsControlDaemonName:      m.daemonName,
		CUDA_VISIBLE_DEVICES:      strings.Join(deviceUUIDs, ","),
		MultiUser:                 true,
		NvidiaDriverRoot:          m.hostDriverRoot,
		MpsShmDirectory:           m.shmDir,
		MpsPipeDirectory:          m.pipeDir,
		MpsLogDirectory:           m.logDir,
		MpsImageName:              m.config.flags.imageName,
		MpsImagePullPolicy:        m.config.imagePullPolicy,
		MpsImagePullSecretNames:   m.config.imagePullSecretNames,
		ServiceAccountName:        m.config.flags.serviceAccountName,
		FeatureGates:              featuregates.ToMap(),
		MpsShmMountPath:           setMpsShmMountPath(osFileChecker{}),
	}

	deployment, err := renderMpsControlDaemonDeployment(m.templatePath, templateData)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(m.shmDir, 0755); err != nil {
		return fmt.Errorf("error creating directory %v: %w", m.shmDir, err)
	}
	if err := os.MkdirAll(m.pipeDir, 0755); err != nil {
		return fmt.Errorf("error creating directory %v: %w", m.pipeDir, err)
	}
	if err := os.MkdirAll(m.logDir, 0755); err != nil {
		return fmt.Errorf("error creating directory %v: %w", m.logDir, err)
	}

	mountExecutable, err := exec.LookPath("mount")
	if err != nil {
		return fmt.Errorf("error finding 'mount' executable: %w", err)
	}

	mounter := mount.New(mountExecutable)
	sizeArg := fmt.Sprintf("size=%v", getDefaultShmSize())
	mountOptions := []string{"rw", "nosuid", "nodev", "noexec", "relatime", sizeArg}
	if err := mounter.Mount("shm", m.shmDir, "tmpfs", mountOptions); err != nil {
		return fmt.Errorf("error mounting %v as tmpfs: %w", m.shmDir, err)
	}

	if len(deviceUUIDs) > 0 && m.nvdevlib != nil {
		if err := m.nvdevlib.setComputeMode(deviceUUIDs, "EXCLUSIVE_PROCESS"); err != nil {
			return fmt.Errorf("error setting compute mode for MPS: %w", err)
		}
	}

	_, err = m.config.clientsets.Core.AppsV1().Deployments(m.config.flags.namespace).Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create deployment: %w", err)
	}

	return m.AssertReady(ctx)
}

func (m *MpsManager) EnsureStoppedIfIdle(ctx context.Context, checkpoint *Checkpoint, currentClaimUID string) error {
	if isMpsInUseByOtherClaims(checkpoint, currentClaimUID) {
		klog.V(4).Infof("Node MPS control daemon still in use by other claims, skipping stop")
		return nil
	}
	return m.EnsureStopped(ctx)
}

func (m *MpsManager) ReconcileOnStartup(ctx context.Context, checkpoint *Checkpoint) error {
	if isMpsInUseByOtherClaims(checkpoint, "") {
		return m.EnsureStarted(ctx)
	}
	return m.EnsureStopped(ctx)
}

func isMpsInUseByOtherClaims(checkpoint *Checkpoint, excludeClaimUID string) bool {
	if checkpoint == nil || checkpoint.V2 == nil || checkpoint.V2.PreparedClaims == nil {
		return false
	}
	for cuid, claim := range checkpoint.V2.PreparedClaims {
		if cuid == excludeClaimUID {
			continue
		}
		if claim.CheckpointState != ClaimCheckpointStatePrepareCompleted {
			continue
		}
		for _, group := range claim.PreparedDevices {
			if ptr.Deref(group.ConfigState.MpsApplied, false) || group.ConfigState.MpsControlDaemonID != "" {
				return true
			}
		}
	}
	return false
}

func renderMpsControlDaemonDeployment(templatePath string, templateData MpsControlDaemonTemplateData) (*appsv1.Deployment, error) {
	tmpl, err := template.ParseFiles(templatePath)
	if err != nil {
		return nil, fmt.Errorf("failed to parse template file: %w", err)
	}

	var deploymentYaml bytes.Buffer
	if err := tmpl.Execute(&deploymentYaml, templateData); err != nil {
		return nil, fmt.Errorf("failed to execute template: %w", err)
	}

	var unstructuredObj unstructured.Unstructured
	err = yaml.Unmarshal(deploymentYaml.Bytes(), &unstructuredObj)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal yaml: %w", err)
	}

	var deployment appsv1.Deployment
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredObj.UnstructuredContent(), &deployment)
	if err != nil {
		return nil, fmt.Errorf("failed to convert unstructured data to typed object: %w", err)
	}

	return &deployment, nil
}

func (m *MpsManager) AssertReady(ctx context.Context) error {
	backoff := wait.Backoff{
		Duration: time.Second,
		Factor:   2,
		Jitter:   1,
		Steps:    4,
		Cap:      10 * time.Second,
	}

	return retry.OnError(
		backoff,
		func(error) bool {
			return true
		},
		func() error {
			deployment, err := m.config.clientsets.Core.AppsV1().Deployments(m.config.flags.namespace).Get(
				ctx,
				m.daemonName,
				metav1.GetOptions{},
			)
			if err != nil {
				return fmt.Errorf("failed to get deployment: %w", err)
			}

			if deployment.Status.ReadyReplicas != 1 {
				return fmt.Errorf("waiting for MPS control daemon to come online")
			}

			selector := deployment.Spec.Selector.MatchLabels

			pods, err := m.config.clientsets.Core.CoreV1().Pods(m.config.flags.namespace).List(
				ctx,
				metav1.ListOptions{
					LabelSelector: labels.Set(selector).AsSelector().String(),
				},
			)
			if err != nil {
				return fmt.Errorf("error listing pods from deployment: %w", err)
			}

			if len(pods.Items) != 1 {
				return fmt.Errorf("unexpected number of pods in deployment: %v", len(pods.Items))
			}

			if len(pods.Items[0].Status.ContainerStatuses) != 1 {
				return fmt.Errorf("unexpected number of container statuses in pod")
			}

			if !pods.Items[0].Status.ContainerStatuses[0].Ready {
				return fmt.Errorf("control daemon not yet ready")
			}

			return nil
		},
	)
}

func (m *MpsManager) GetCDIContainerEdits(config *configapi.MpsConfig, deviceUUIDs []string) (*cdiapi.ContainerEdits, error) {
	env := []string{
		fmt.Sprintf("CUDA_MPS_PIPE_DIRECTORY=%s", "/tmp/nvidia-mps"),
	}
	if config != nil && config.DefaultActiveThreadPercentage != nil {
		env = append(env, fmt.Sprintf("CUDA_MPS_ACTIVE_THREAD_PERCENTAGE=%d", *config.DefaultActiveThreadPercentage))
	}
	if config != nil && (config.DefaultPinnedDeviceMemoryLimit != nil || len(config.DefaultPerDevicePinnedMemoryLimit) > 0) {
		limits, err := config.DefaultPerDevicePinnedMemoryLimit.Normalize(deviceUUIDs, config.DefaultPinnedDeviceMemoryLimit)
		if err != nil {
			return nil, fmt.Errorf("error calculating pinned device memory limits: %w", err)
		}
		if len(limits) > 0 {
			var limitEntries []string
			for _, uuid := range deviceUUIDs {
				if lim, ok := limits[uuid]; ok {
					limitEntries = append(limitEntries, fmt.Sprintf("%s=%s", uuid, lim))
				}
			}
			if len(limitEntries) > 0 {
				env = append(env, fmt.Sprintf("CUDA_MPS_PINNED_DEVICE_MEM_LIMIT=%s", strings.Join(limitEntries, ",")))
			}
		}
	}
	return &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			Env: env,
			Mounts: []*cdispec.Mount{
				{
					ContainerPath: "/dev/shm",
					HostPath:      m.shmDir,
					Options:       []string{"rw", "nosuid", "nodev", "bind"},
				},
				{
					ContainerPath: "/tmp/nvidia-mps",
					HostPath:      m.pipeDir,
					Options:       []string{"rw", "nosuid", "nodev", "bind"},
				},
			},
		},
	}, nil
}

func (m *MpsManager) EnsureStopped(ctx context.Context) error {
	m.Lock()
	defer m.Unlock()

	_, err := os.Stat(m.controlFilesRoot)
	if os.IsNotExist(err) {
		return nil
	}

	klog.Infof("Stopping node-level MPS control daemon '%v'", m.daemonName)

	deletePolicy := metav1.DeletePropagationForeground
	deleteOptions := metav1.DeleteOptions{
		PropagationPolicy: &deletePolicy,
	}

	err = m.config.clientsets.Core.AppsV1().Deployments(m.config.flags.namespace).Delete(ctx, m.daemonName, deleteOptions)
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to delete deployment: %w", err)
	}

	// Start() sets the compute mode of these GPUs to EXCLUSIVE_PROCESS as
	// required by MPS. Reset it back to DEFAULT here so the GPUs are not left
	// stuck in EXCLUSIVE_PROCESS after teardown.
	if err := m.nvdevlib.setComputeMode(m.getSupportedGpuUUIDs(), "DEFAULT"); err != nil {
		return fmt.Errorf("error resetting compute mode to DEFAULT: %w", err)
	}

	mountExecutable, err := exec.LookPath("mount")
	if err != nil {
		return fmt.Errorf("error finding 'mount' executable: %w", err)
	}

	mounter := mount.New(mountExecutable)
	if err := mount.CleanupMountPoint(m.shmDir, mounter, true); err != nil {
		return fmt.Errorf("error unmounting %v: %w", m.shmDir, err)
	}

	if err := os.RemoveAll(m.controlFilesRoot); err != nil {
		return fmt.Errorf("error removing directory %v: %w", m.controlFilesRoot, err)
	}

	deviceUUIDs := m.getSupportedGpuUUIDs()
	if len(deviceUUIDs) > 0 && m.nvdevlib != nil {
		_ = m.nvdevlib.setComputeMode(deviceUUIDs, "DEFAULT")
	}

	return nil
}

// getDefaultShmSize returns the default size for the tmpfs to be created.
// This reads /proc/meminfo to get the total memory to calculate this. If this
// fails a fallback size of 65536k is used.
func getDefaultShmSize() string {
	const fallbackSize = "65536k"

	meminfo, err := os.Open("/proc/meminfo")
	if err != nil {
		klog.ErrorS(err, "failed to open /proc/meminfo")
		return fallbackSize
	}
	defer func() {
		_ = meminfo.Close()
	}()

	scanner := bufio.NewScanner(meminfo)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}

		parts := strings.SplitN(strings.TrimSpace(strings.TrimPrefix(line, "MemTotal:")), " ", 2)
		memTotal, err := strconv.Atoi(parts[0])
		if err != nil {
			klog.ErrorS(err, "could not convert MemTotal to an integer")
			return fallbackSize
		}

		var unit string
		if len(parts) == 2 {
			unit = string(parts[1][0])
		}

		return fmt.Sprintf("%d%s", memTotal/2, unit)
	}
	return fallbackSize
}
