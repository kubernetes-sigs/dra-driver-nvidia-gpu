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

package fabricmanager

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync/atomic"

	"k8s.io/klog/v2"

	"sigs.k8s.io/dra-driver-nvidia-gpu/internal/common"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// vgpuHostModule is loaded by the NVIDIA vGPU host stack. Partition management
// with vGPU is pending FABRIC_MODE=2 support.
const vgpuHostModule = "nvidia_vgpu_vfio"

var checkFMSupport atomic.Bool

// CheckFabricManagerSupport decides whether Fabric Manager partition management
// is effectively enabled on this node and records the result for
// SupportsFabricManagerPartitioning. Call it once at kubelet plugin startup.
func CheckFabricManagerSupport(hostRoot string) error {
	enabled, err := checkFabricManagerSupport(hostRoot)
	if err != nil {
		return err
	}
	checkFMSupport.Store(enabled)
	return nil
}

// SupportsFabricManagerPartitioning reports whether this process manages Fabric
// Manager partitions on this node. It is false until CheckFabricManagerSupport
// enables it.
func SupportsFabricManagerPartitioning() bool {
	return checkFMSupport.Load()
}

func checkFabricManagerSupport(hostRoot string) (bool, error) {
	if !featuregates.Enabled(featuregates.FabricManagerPartitioning) {
		return false, nil
	}

	if common.UsingAltProcDevices() {
		klog.Infof("Mock NVML proc is in use; disabling Fabric Manager partitioning")
		return false, nil
	}

	vgpuLoaded, err := kernelModuleLoaded(hostRoot, vgpuHostModule)
	if err != nil {
		return false, fmt.Errorf("checking for vGPU kernel module %q: %w", vgpuHostModule, err)
	}
	if vgpuLoaded {
		klog.Warningf("vGPU kernel module %q is loaded; disabling Fabric Manager partitioning on this node", vgpuHostModule)
		return false, nil
	}

	hasFabric, err := HasFabricManagerFabric(hostRoot)
	if err != nil {
		return false, fmt.Errorf("detecting NVSwitch/NVLink5 fabric: %w", err)
	}
	if !hasFabric {
		klog.Infof("No NVSwitch/NVLink5 fabric detected on this node; disabling Fabric Manager partitioning")
		return false, nil
	}

	return true, nil
}

// kernelModuleLoaded requires <hostRoot>/sys/module to exist so that a missing
// host-root mount is not mistaken for the module being absent.
func kernelModuleLoaded(hostRoot, module string) (bool, error) {
	moduleRoot := hostPath(hostRoot, "sys", "module")
	info, err := os.Stat(moduleRoot)
	if err != nil {
		return false, fmt.Errorf("inspecting %s: %w", moduleRoot, err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%s is not a directory", moduleRoot)
	}

	info, err = os.Stat(hostPath(hostRoot, "sys", "module", module))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspecting kernel module %q: %w", module, err)
	}
	return info.IsDir(), nil
}
