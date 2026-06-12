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
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

const (
	kernelIommuGroupPath         = "/sys/kernel/iommu_groups"
	vfioPciModule                = "vfio_pci"
	vfioPciDriver                = "vfio-pci"
	nvidiaDriver                 = "nvidia"
	hostRoot                     = "/host-root"
	sysModulePath                = "/sys/module"
	vfioDevicesRoot              = "/dev/vfio"
	vfioDevicesPath              = "/dev/vfio/devices"
	iommuDevicePath              = "/dev/iommu"
	nvidiaPersistencedSocketPath = "/run/nvidia-persistenced/socket"
	unbindFromDriverScript       = "/usr/bin/unbind_from_driver.sh"
	bindToDriverScript           = "/usr/bin/bind_to_driver.sh"
	pciDriversProbePath          = "/sys/bus/pci/drivers_probe"
	gpuFreeCheckInterval         = 1 * time.Second
	gpuFreeCheckTimeout          = 60 * time.Second
)

// pciDevicesPath is a variable (not const) to allow test overrides with a mock sysfs.
var pciDevicesPath = "/sys/bus/pci/devices"

type VfioPciManager struct {
	sync.Mutex
	containerDriverRoot string
	hostDriverRoot      string
	driver              string
	nvlib               *deviceLib
	nvidiaEnabled       bool
	// companionMu protects companionDrivers from concurrent access.
	companionMu sync.Mutex
	// companionDrivers tracks the original driver of each non-GPU companion device
	// that was switched to vfio-pci during Configure(), keyed by GPU PCI address.
	// Outer key: GPU PCI address, inner key: companion PCI address, value: original driver.
	companionDrivers map[string]map[string]string
}

func NewVfioPciManager(containerDriverRoot string, hostDriverRoot string, nvlib *deviceLib, nvidiaEnabled bool) (*VfioPciManager, error) {
	if loaded, err := checkVfioPCIModuleLoaded(); err == nil {
		if !loaded {
			err = loadVfioPciModule()
			if err != nil {
				return nil, fmt.Errorf("failed to load vfio_pci module: %w", err)
			}
		}
	} else {
		return nil, fmt.Errorf("error checking if vfio_pci module is loaded: %w", err)
	}

	iommuEnabled, err := checkIommuEnabled()
	if err != nil {
		return nil, fmt.Errorf("error checking if IOMMU is enabled: %w", err)
	}
	if !iommuEnabled {
		return nil, fmt.Errorf("IOMMU is not enabled in the kernel")
	}

	vm := &VfioPciManager{
		containerDriverRoot: containerDriverRoot,
		hostDriverRoot:      hostDriverRoot,
		driver:              vfioPciDriver,
		nvlib:               nvlib,
		nvidiaEnabled:       nvidiaEnabled,
		companionDrivers:    make(map[string]map[string]string),
	}

	return vm, nil
}

// WaitForGPUFree does a best effort scan of the GPU clients running on the host and
// waits for them to exit on their own.
//
// This polls the GPU's /dev/nvidia* device node in the driver installation path on
// the host periodically to see if any process has open fds to it. This acts as a
// limited safety net to ensure that we don't mistakenly try to unbind a GPU from
// the nvidia driver while it is busy.
// Note: Here, we can only check if there are any GPU clients running on the host rootfs
// where the driver is installed. If you have containerized GPU clients that work
// with their own view of the device nodes, we will not able to detect it.
func (vm *VfioPciManager) WaitForGPUFree(ctx context.Context, info *VfioDeviceInfo) error {
	if info.parent == nil {
		return nil
	}
	timeout := time.After(gpuFreeCheckTimeout)
	ticker := time.NewTicker(gpuFreeCheckInterval)
	defer ticker.Stop()

	gpuDeviceNode := filepath.Join(vm.hostDriverRoot, "dev", fmt.Sprintf("nvidia%d", info.parent.minor))
	var err error
	for {
		select {
		case <-timeout:
			return fmt.Errorf("timed out waiting for gpu to be free: %w", err)
		case <-ticker.C:
			out, cmdErr := execCommandWithChroot(hostRoot, "fuser", []string{gpuDeviceNode}) //nolint:gosec
			if cmdErr != nil {
				// fuser returns exit code 1 if no process is using the device.
				if exitErr, ok := cmdErr.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
					return nil
				}
				err = fmt.Errorf("unexpected error checking if gpu device %q is free: %w", info.PciBusID, cmdErr)
				klog.V(6).Infof("[DEBUG] %s", err.Error())
				continue
			}
			err = fmt.Errorf("gpu device %q has open fds by process(es): %q", info.PciBusID, string(out))
			klog.V(6).Infof("[DEBUG] %s", err.Error())
		}
	}
}

// Verify there are no VFs on the GPU.
func (vm *VfioPciManager) verifyDisabledVFs(pciBusID string) error {
	gpu, err := vm.nvlib.nvpci.GetGPUByPciBusID(pciBusID)
	if err != nil {
		return err
	}
	if gpu == nil {
		return fmt.Errorf("no GPU found at PCI bus ID %q", pciBusID)
	}
	// PhysicalFunction is nil for GPUs that do not support SR-IOV (e.g. T400).
	// A nil PhysicalFunction means no VFs can exist, so it is safe to proceed.
	if gpu.SriovInfo.PhysicalFunction == nil {
		return nil
	}
	numVFs := gpu.SriovInfo.PhysicalFunction.NumVFs
	if numVFs > 0 {
		return fmt.Errorf("gpu has %d VFs, cannot unbind", numVFs)
	}
	return nil
}

// Configure binds the GPU to the vfio-pci driver.
func (vm *VfioPciManager) Configure(ctx context.Context, info *VfioDeviceInfo) error {
	driver, err := getDriver(pciDevicesPath, info.PciBusID)
	if err != nil {
		return fmt.Errorf("error getting driver details for GPU %q: %w", info.PciBusID, err)
	}

	// Skip if the GPU is already bound to the vfio-pci driver.
	if driver == vm.driver {
		return nil
	}

	// Only support vfio-pci or nvidia (if vm.nvidiaEnabled) driver.
	if !vm.nvidiaEnabled || driver != nvidiaDriver {
		return fmt.Errorf("GPU %q is bound to %q driver, expected %q or %q", info.PciBusID, driver, vm.driver, nvidiaDriver)
	}

	// Disable GPU Persistence Mode.
	err = vm.disableGPUPersistenceMode(info.PciBusID)
	if err != nil {
		return fmt.Errorf("error disabling persistence mode for GPU %q: %w", info.PciBusID, err)
	}

	// Wait for other GPU clients to evacuate.
	err = vm.WaitForGPUFree(ctx, info)
	if err != nil {
		return fmt.Errorf("error waiting for GPU %q to be free: %w", info.PciBusID, err)
	}

	// Verify SRIOV VFs are disabled on the GPU.
	err = vm.verifyDisabledVFs(info.PciBusID)
	if err != nil {
		return fmt.Errorf("error verifying disabled VFs: %w", err)
	}

	// Change the GPU driver to vfio-pci.
	err = vm.changeDriver(info.PciBusID, vm.driver)
	if err != nil {
		return fmt.Errorf("error changing driver for GPU %q: %w", info.PciBusID, err)
	}

	// Bind all companion devices in the same IOMMU group to vfio-pci.
	// VFIO requires every non-bridge endpoint in the group to be bound to vfio-pci
	// before QEMU can open the group (viability check).
	if err := vm.bindIommuGroupCompanions(info.PciBusID); err != nil {
		// Log but don't fail — the QEMU viability error will surface if this matters.
		klog.Warningf("Could not bind all IOMMU group companions for %s: %v", info.PciBusID, err)
	}

	return nil
}

// Unconfigure binds the GPU to the nvidia driver.
func (vm *VfioPciManager) Unconfigure(ctx context.Context, info *VfioDeviceInfo) error {
	// Do nothing if we dont expect to switch to nvidia driver.
	if !vm.nvidiaEnabled {
		return nil
	}

	// Restore all IOMMU group companion devices to their original drivers before
	// switching the GPU back, so the group is in a consistent state.
	vm.restoreIommuGroupCompanions(info.PciBusID)

	// Change the GPU driver to nvidia.
	err := vm.changeDriver(info.PciBusID, nvidiaDriver)
	if err != nil {
		return fmt.Errorf("error changing driver for GPU %q: %w", info.PciBusID, err)
	}

	// Enable GPU Persistence Mode.
	err = vm.enableGPUPersistenceMode(info.PciBusID)
	if err != nil {
		return fmt.Errorf("error enabling persistence mode for GPU %q: %w", info.PciBusID, err)
	}

	return nil
}

// Get the current driver the GPU is bound to.
func getDriver(pciDevicesPath, pciAddress string) (string, error) {
	driverPath, err := os.Readlink(filepath.Join(pciDevicesPath, pciAddress, "driver"))
	if err != nil {
		return "", err
	}
	_, driver := filepath.Split(driverPath)
	return driver, nil
}

// Change the driver the GPU is bound to.
func (vm *VfioPciManager) changeDriver(pciAddress, driver string) error {
	currentDriver, err := getDriver(pciDevicesPath, pciAddress)
	if err != nil {
		return fmt.Errorf("error getting driver details for GPU %q: %w", pciAddress, err)
	}

	// Skip if the GPU is already bound to the desired driver.
	if currentDriver == driver {
		return nil
	}

	err = vm.unbindFromDriver(pciAddress)
	if err != nil {
		return err
	}
	err = vm.bindToDriver(pciAddress, driver)
	if err != nil {
		return err
	}
	return nil
}

// Unbind the GPU from the driver it is bound to.
func (vm *VfioPciManager) unbindFromDriver(pciAddress string) error {
	out, err := execCommand(unbindFromDriverScript, []string{pciAddress}) //nolint:gosec
	if err != nil {
		klog.Errorf("Attempting to unbind %s from its driver failed; stdout: %s, err: %v", pciAddress, string(out), err)
		return err
	}
	return nil
}

// Bind the GPU to the given driver.
func (vm *VfioPciManager) bindToDriver(pciAddress, driver string) error {
	out, err := execCommand(bindToDriverScript, []string{pciAddress, driver}) //nolint:gosec
	if err != nil {
		klog.Errorf("Attempting to bind %s to %s driver failed; stdout: %s, err: %v", pciAddress, driver, string(out), err)
		return err
	}
	return nil
}

// Enable GPU Persistence Mode.
func (vm *VfioPciManager) enableGPUPersistenceMode(pciAddress string) error {
	// Obtain a lock to serialize persistence mode operations.
	// This is a cautious approach to avoid any NVML race conditions.
	vm.Lock()
	defer vm.Unlock()
	return vm.nvlib.enableGPUPersistenceMode(pciAddress)
}

// Disable GPU Persistence Mode.
func (vm *VfioPciManager) disableGPUPersistenceMode(pciAddress string) error {
	// Obtain a lock to serialize persistence mode operations.
	// This is a cautious approach to avoid any NVML race conditions.
	vm.Lock()
	defer vm.Unlock()
	// We dont need to toggle persistence mode if nvidia-persistenced is not running.
	klog.V(4).Infof("Checking if nvidia-persistenced is running: %s", filepath.Join(vm.containerDriverRoot, nvidiaPersistencedSocketPath))
	_, err := os.Stat(filepath.Join(vm.containerDriverRoot, nvidiaPersistencedSocketPath))
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("error checking if nvidia-persistenced is running: %w", err)
		}
		klog.V(4).Infof("nvidia-persistenced is not running; nothing to do...")
		return nil
	}

	err = vm.nvlib.disableGPUPersistenceMode(pciAddress)
	if err != nil {
		return fmt.Errorf("error disabling persistence mode for GPU %q: %w", pciAddress, err)
	}
	return nil
}

// Check if the vfio_pci module is loaded.
func checkVfioPCIModuleLoaded() (bool, error) {
	f, err := os.Stat(filepath.Join(hostRoot, sysModulePath, vfioPciModule))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check if vfio_pci module is loaded: %w", err)
	}

	if !f.IsDir() {
		return false, nil
	}

	return true, nil
}

// Load the vfio_pci module.
func loadVfioPciModule() error {
	_, err := execCommandWithChroot(hostRoot, "modprobe", []string{vfioPciModule}) //nolint:gosec
	if err != nil {
		return err
	}

	return nil
}

// Check if IOMMU is enabled.
func checkIommuEnabled() (bool, error) {
	f, err := os.Open(filepath.Join(hostRoot, kernelIommuGroupPath))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	if err == io.EOF {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return true, nil
}

// Check if IOMMUFD is enabled.
// We correlate the IOMMUFD support with the presence of the /dev/iommu API device.
func checkIommuFDEnabled() (bool, error) {
	_, err := os.Stat(filepath.Join(hostRoot, iommuDevicePath))
	if err != nil {
		if os.IsNotExist(err) {
			klog.Infof("IOMMUFD is not enabled, /dev/iommu device node does not exist")
			return false, nil
		}
		return false, fmt.Errorf("error checking if iommu device node exists: %w", err)
	}
	return true, nil
}

// Execute a command with chroot.
func execCommandWithChroot(fsRoot, cmd string, args []string) ([]byte, error) {
	chrootArgs := []string{fsRoot, cmd}
	chrootArgs = append(chrootArgs, args...)
	return exec.Command("chroot", chrootArgs...).CombinedOutput()
}

// Execute a command.
func execCommand(cmd string, args []string) ([]byte, error) {
	return exec.Command(cmd, args...).CombinedOutput()
}

// getIommuGroupCompanions returns PCI addresses of all non-bridge endpoint devices
// in the same IOMMU group as gpuPciBusID, excluding the GPU itself.
// PCIe root ports and bridges (class 0x06xx) are skipped as they do not need to
// be bound to vfio-pci for VFIO group viability.
func getIommuGroupCompanions(gpuPciBusID string) ([]string, error) {
	iommuGroupPath, err := resolveIommuGroupDevicesPath(gpuPciBusID)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(iommuGroupPath)
	if err != nil {
		return nil, fmt.Errorf("error reading IOMMU group devices at %q: %w", iommuGroupPath, err)
	}

	var companions []string
	for _, entry := range entries {
		devName := entry.Name()
		if devName == gpuPciBusID {
			continue // skip the GPU itself
		}

		isBridge, err := isPCIBridgeDevice(devName)
		if err != nil {
			klog.Warningf("Could not read PCI class for %s, skipping: %v", devName, err)
			continue
		}
		if isBridge {
			klog.V(4).Infof("Skipping PCIe bridge/port %s in IOMMU group", devName)
			continue
		}

		companions = append(companions, devName)
	}
	return companions, nil
}

// resolveIommuGroupDevicesPath resolves the IOMMU group devices directory for a PCI device.
func resolveIommuGroupDevicesPath(pciBusID string) (string, error) {
	iommuGroupLink := filepath.Join(pciDevicesPath, pciBusID, "iommu_group")
	iommuGroupTarget, err := os.Readlink(iommuGroupLink)
	if err != nil {
		return "", fmt.Errorf("error reading iommu_group symlink for %q: %w", pciBusID, err)
	}
	return filepath.Join(pciDevicesPath, pciBusID, iommuGroupTarget, "devices"), nil
}

// isPCIBridgeDevice returns true if the PCI device is a bridge or root port (class 0x06xx).
func isPCIBridgeDevice(pciBusID string) (bool, error) {
	classPath := filepath.Join(pciDevicesPath, pciBusID, "class")
	classBytes, err := os.ReadFile(classPath)
	if err != nil {
		return false, err
	}
	return strings.HasPrefix(strings.TrimSpace(string(classBytes)), "0x06"), nil
}

// bindIommuGroupCompanions switches every non-bridge companion device in the GPU's
// IOMMU group to vfio-pci, saving their original drivers for later restoration.
func (vm *VfioPciManager) bindIommuGroupCompanions(gpuPciBusID string) error {
	companions, err := getIommuGroupCompanions(gpuPciBusID)
	if err != nil {
		return err
	}

	vm.companionMu.Lock()
	defer vm.companionMu.Unlock()

	gpuCompanions := make(map[string]string)

	for _, dev := range companions {
		origDriver := ""
		if d, err := getDriver(pciDevicesPath, dev); err == nil {
			origDriver = d
		}

		if origDriver == vfioPciDriver {
			klog.V(4).Infof("IOMMU companion %s already bound to vfio-pci", dev)
			gpuCompanions[dev] = origDriver
			continue
		}

		klog.Infof("Binding IOMMU group companion %s (was: %q) to vfio-pci for GPU %s", dev, origDriver, gpuPciBusID)

		// Unbind from current driver if any.
		if origDriver != "" {
			if out, err := execCommand(unbindFromDriverScript, []string{dev}); err != nil {
				return fmt.Errorf("error unbinding companion %s from %q: %w (output: %s)", dev, origDriver, err, string(out))
			}
		}

		// Bind to vfio-pci.
		if out, err := execCommand(bindToDriverScript, []string{dev, vfioPciDriver}); err != nil {
			return fmt.Errorf("error binding companion %s to vfio-pci: %w (output: %s)", dev, err, string(out))
		}

		gpuCompanions[dev] = origDriver
	}

	vm.companionDrivers[gpuPciBusID] = gpuCompanions
	return nil
}

// isCompanionStillNeeded returns true if the companion device is still referenced
// by another configured GPU (i.e. a GPU other than excludeGPU).
func (vm *VfioPciManager) isCompanionStillNeeded(companionDev, excludeGPU string) bool {
	for gpu, companions := range vm.companionDrivers {
		if gpu == excludeGPU {
			continue
		}
		if _, ok := companions[companionDev]; ok {
			return true
		}
	}
	return false
}

// restoreIommuGroupCompanions unbinds companion devices from vfio-pci and
// triggers re-probe so the kernel re-attaches their original drivers.
// Only companions that are not still needed by other configured GPUs are restored.
func (vm *VfioPciManager) restoreIommuGroupCompanions(gpuPciBusID string) {
	vm.companionMu.Lock()
	defer vm.companionMu.Unlock()

	gpuCompanions, ok := vm.companionDrivers[gpuPciBusID]
	if !ok || len(gpuCompanions) == 0 {
		return
	}

	for dev, origDriver := range gpuCompanions {
		// Skip if another configured GPU still needs this companion bound to vfio-pci.
		if vm.isCompanionStillNeeded(dev, gpuPciBusID) {
			klog.V(4).Infof("Companion %s still needed by another GPU, skipping restore", dev)
			continue
		}

		klog.Infof("Restoring IOMMU group companion %s to %q for GPU %s", dev, origDriver, gpuPciBusID)

		// Unbind from vfio-pci.
		if out, err := execCommand(unbindFromDriverScript, []string{dev}); err != nil {
			klog.Warningf("Error unbinding companion %s from vfio-pci: %v (output: %s)", dev, err, string(out))
		}

		// Clear driver_override so the kernel will use its normal probe logic.
		// Writing "\n" triggers the kernel's driver_override_store to free the override.
		overridePath := filepath.Join(pciDevicesPath, dev, "driver_override")
		if err := os.WriteFile(overridePath, []byte("\n"), 0600); err != nil {
			klog.Warningf("Could not clear driver_override for %s: %v", dev, err)
		}

		// Trigger re-probe so the kernel re-attaches the original driver.
		if err := os.WriteFile(pciDriversProbePath, []byte(dev), 0600); err != nil {
			klog.Warningf("Could not trigger re-probe for %s: %v — companion may need manual rebind", dev, err)
		}
	}

	delete(vm.companionDrivers, gpuPciBusID)
}
