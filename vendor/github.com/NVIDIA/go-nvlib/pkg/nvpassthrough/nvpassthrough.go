/*
 * Copyright (c) NVIDIA CORPORATION.  All rights reserved.
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

package nvpassthrough

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/NVIDIA/go-nvlib/pkg/nvpci"
)

const (
	pciRootDir        = "/sys/bus/pci/"
	pciDevicesRoot    = pciRootDir + "devices"
	pciDriversRoot    = pciRootDir + "drivers"
	vfioPCIDriverName = "vfio-pci"
	consumerPrefix    = "consumer:pci:"
	libModulesRoot    = "/lib/modules/"
)

type Interface interface {
	FindBestVFIOVariant(string) (string, error)
	BindToVFIODriver(string) error
	BindToDriver(string, string) error
	Unbind(string) error
}

type nvpassthrough struct {
	logger            basicLogger
	hostRoot          string
	nvpciLib          nvpci.Interface
	loadKernelModules bool
}

var _ Interface = (*nvpassthrough)(nil)

type nvidiaPCIAuxDevice struct {
	Path    string
	Address string
	Driver  string
}

func New(opts ...Option) Interface {
	n := &nvpassthrough{}
	for _, opt := range opts {
		opt(n)
	}
	if n.logger == nil {
		n.logger = &nullLogger{}
	}
	if n.hostRoot == "" {
		n.hostRoot = "/"
	}
	if n.nvpciLib == nil {
		n.nvpciLib = nvpci.New()
	}

	return n
}

// Option defines a function for passing options to the New() call.
type Option func(*nvpassthrough)

// WithLogger provides an Option to set the logger for the library.
func WithLogger(logger basicLogger) Option {
	return func(w *nvpassthrough) {
		w.logger = logger
	}
}

// WithHostRoot provides an Option to set the path to the host root filesystem.
// The path is only used when the WithLoadKernelModules option is also enabled.
// The path is assumed to be a chroot-able filesystem.
func WithHostRoot(hostRoot string) Option {
	return func(w *nvpassthrough) {
		w.hostRoot = hostRoot
	}
}

// WithNvpciLib provides an Option to set the nvpci lib used.
func WithNvpciLib(lib nvpci.Interface) Option {
	return func(w *nvpassthrough) {
		w.nvpciLib = lib
	}
}

// WithLoadKernelModules provides an Option for opting-in to loading
// kernel modules before binding NVIDIA PCI devices to them. By default,
// this behavior is disabled.
func WithLoadKernelModules(loadKernelModules bool) Option {
	return func(w *nvpassthrough) {
		w.loadKernelModules = loadKernelModules
	}
}

// FindBestVFIOVariant finds the "best" match of all vfio_pci aliases for
// device in the host modules.alias file. This uses the algorithm of
// finding every modules.alias line that begins with "alias vfio_pci:",
// then picking the one that matches the device's own modalias value
// (from the file of that name in the device's sysfs directory) with the
// fewest "wildcards" (* character, meaning "match any value for this
// attribute").
//
// (cdesiniotis) this code is inspired by:
// https://gitlab.com/libvirt/libvirt/-/commit/82e2fac297105f554f57fb589002933231b4f711
func (n *nvpassthrough) FindBestVFIOVariant(address string) (string, error) {
	device, err := n.nvpciLib.GetNvidiaDeviceByPciBusID(address)
	if err != nil {
		return "", fmt.Errorf("failed to get NVIDIA PCI device by bus id %q: %w", address, err)
	}
	if device == nil {
		return "", fmt.Errorf("device at %q is not an NVIDIA PCI device", address)
	}

	vfioAliases, err := getVFIOAliases()
	if err != nil {
		return "", fmt.Errorf("failed to get vfio_pci aliases in modules.alias file: %w", err)
	}
	if len(vfioAliases) == 0 {
		n.logger.Debugf("No vfio_pci entries found in modules.alias file, falling back to default vfio-pci driver")
		return vfioPCIDriverName, nil
	}

	modAliasPath := filepath.Join(device.Path, "modalias")
	modAliasContent, err := os.ReadFile(modAliasPath)
	if err != nil {
		return "", fmt.Errorf("failed to read modalias file for %s: %w", device.Address, err)
	}

	modAliasStr := strings.TrimSpace(string(modAliasContent))
	modAlias, err := parseModAliasString(modAliasStr)
	if err != nil {
		return "", fmt.Errorf("failed to parse modalias string %q for device %q: %w", modAliasStr, device.Address, err)
	}

	// Find the best matching VFIO driver for this device
	bestMatch := findBestMatch(modAlias, vfioAliases)
	if bestMatch == "" {
		n.logger.Debugf("No matching vfio driver found for device %s in modules.alias file, falling back to default vfio-pci driver", device.Address)
		return vfioPCIDriverName, nil
	}

	return bestMatch, nil
}

// BindToVFIODriver binds the provided NVIDIA PCI device to the
// vfio-pci driver (or a variant VFIO driver if one is preferred).
// This function takes care of additional logic, like making sure
// the vfio-pci driver is loaded first and that every other PCI function
// of the same card also gets bound to the vfio-pci driver.
func (n *nvpassthrough) BindToVFIODriver(address string) error {
	device, err := n.nvpciLib.GetNvidiaDeviceByPciBusID(address)
	if err != nil {
		return fmt.Errorf("failed to get NVIDIA PCI device by bus id %q: %w", address, err)
	}
	if device == nil {
		return fmt.Errorf("device at %q is not an NVIDIA PCI device", address)
	}

	vfioDriverName, err := n.FindBestVFIOVariant(address)
	if err != nil {
		return fmt.Errorf("failed to find best vfio variant driver: %w", err)
	}

	if n.loadKernelModules {
		km := newKernelModules(n.hostRoot)
		if err := km.load(vfioDriverName); err != nil {
			return fmt.Errorf("failed to load %q driver: %w", vfioDriverName, err)
		}
	}

	// (cdesiniotis) Module names in the modules.alias file will only ever contain
	// underscores characters and not dashes -- this aligns with how the linux kernel
	// stores module names internally. This can sometimes differ from the name of the
	// directory in /sys/bus/pci/driver/ for a given module. For example, this
	// contradiction exists for the standard vfio-pci module:
	//
	// $ file /sys/bus/pci/drivers/vfio-pci
	// sys/bus/pci/drivers/vfio-pci: directory
	//
	// $ modinfo vfio-pci | grep ^name:
	// name:           vfio_pci
	//
	// To account for this difference, we check if the module name returned by
	// findBestVFIOVariant() exists in /sys/bus/pci/drivers, and if not, we try
	// again but with any underscore characters converted to dashes.
	driverDir := filepath.Join(pciDriversRoot, vfioDriverName)
	if _, err := os.Stat(driverDir); err != nil {
		vfioDriverNameNormalized := strings.ReplaceAll(vfioDriverName, "_", "-")
		driverDir = filepath.Join(pciDriversRoot, vfioDriverNameNormalized)
		if _, err := os.Stat(driverDir); err != nil {
			return fmt.Errorf("failed to find directory for vfio driver %s at %s, is the module loaded?", vfioDriverName, pciDriversRoot)
		}
		vfioDriverName = vfioDriverNameNormalized
	}

	n.logger.Infof("Binding device %s to driver: %s", device.Address, vfioDriverName)

	if device.Driver != vfioDriverName {
		if err := unbind(device.Address); err != nil {
			return fmt.Errorf("failed to unbind device %s: %w", device.Address, err)
		}
		if err := bind(device.Address, vfioDriverName); err != nil {
			return fmt.Errorf("failed to bind device %s to %s: %w", device.Address, vfioDriverName, err)
		}
	}

	// Bind every other function of the same card. The whole IOMMU group must be bound to a
	// vfio driver (or to nothing) before VFIO will hand it to a guest.
	auxDevs, err := getAuxDevices(pciDevicesRoot, device)
	if err != nil {
		return fmt.Errorf("failed to get auxiliary devices for %s: %w", device.Address, err)
	}
	for _, auxDev := range auxDevs {
		if auxDev.Driver == vfioDriverName {
			continue
		}

		n.logger.Infof("Binding auxiliary device %s to driver: %s", auxDev.Address, vfioDriverName)

		if err := unbind(auxDev.Address); err != nil {
			return fmt.Errorf("failed to unbind auxiliary device %s: %w", auxDev.Address, err)
		}
		if err := bind(auxDev.Address, vfioDriverName); err != nil {
			return fmt.Errorf("failed to bind auxiliary device %s to %s: %w", auxDev.Address, vfioDriverName, err)
		}
	}

	return nil
}

// BindToDriver binds an NVIDIA PCI device to the driver supplied as input.
func (n *nvpassthrough) BindToDriver(address string, driver string) error {
	device, err := n.nvpciLib.GetNvidiaDeviceByPciBusID(address)
	if err != nil {
		return fmt.Errorf("failed to get NVIDIA PCI device by bus id %q: %w", address, err)
	}
	if device == nil {
		return fmt.Errorf("device at %q is not an NVIDIA PCI device", address)
	}

	return bind(address, driver)
}

// Unbind unbinds the provided NVIDIA PCI Device from
// any driver it is currently bound to. This function also ensures
// that every other PCI function of the same card is unbound.
func (n *nvpassthrough) Unbind(address string) error {
	device, err := n.nvpciLib.GetNvidiaDeviceByPciBusID(address)
	if err != nil {
		return fmt.Errorf("failed to get NVIDIA PCI device by bus id %q: %w", address, err)
	}
	if device == nil {
		return fmt.Errorf("device at %q is not an NVIDIA PCI device", address)
	}

	if err := unbind(address); err != nil {
		return fmt.Errorf("failed to unbind device %s: %w", address, err)
	}

	// Unbind every other function of the same card, mirroring BindToVFIODriver.
	auxDevs, err := getAuxDevices(pciDevicesRoot, device)
	if err != nil {
		return fmt.Errorf("failed to get auxiliary devices for %s: %w", address, err)
	}
	for _, auxDev := range auxDevs {
		if err := unbind(auxDev.Address); err != nil {
			return fmt.Errorf("failed to unbind auxiliary device %s: %w", auxDev.Address, err)
		}
	}

	return nil
}

func bind(address string, driver string) error {
	driverOverridePath := filepath.Join(pciDevicesRoot, address, "driver_override")
	if err := writeFile(driverOverridePath, driver); err != nil {
		return fmt.Errorf("failed to set driver_override for %s: %w", address, err)
	}

	bindPath := filepath.Join(pciDriversRoot, driver, "bind")
	if err := writeFile(bindPath, address); err != nil {
		return fmt.Errorf("failed to bind %s to %s: %w", address, driver, err)
	}

	return nil
}

func unbind(address string) error {
	driverOverridePath := filepath.Join(pciDevicesRoot, address, "driver_override")
	if err := writeFile(driverOverridePath, "\n"); err != nil {
		return fmt.Errorf("failed to clear driver_override for %s: %w", address, err)
	}

	driverPath := filepath.Join(pciDevicesRoot, address, "driver")
	if _, err := os.Stat(driverPath); os.IsNotExist(err) {
		return nil
	}

	driverLink, err := os.Readlink(driverPath)
	if err != nil {
		return fmt.Errorf("failed to read driver link for %s: %w", address, err)
	}
	driverName := filepath.Base(driverLink)

	unbindPath := filepath.Join(driverPath, "unbind")
	if err := writeFile(unbindPath, address); err != nil {
		return fmt.Errorf("failed to unbind %s from %s: %w", address, driverName, err)
	}

	return nil
}

// getAuxDevices returns every other PCI function that belongs to the same physical card as
// device.
//
// VFIO assigns an entire IOMMU group to a guest, and refuses the group unless every device in
// it is bound to a vfio driver or to no driver at all. A discrete NVIDIA GPU is a
// multi-function PCI device: .0 VGA/3D, .1 HDMI audio, and on Turing-era boards .2
// (VirtualLink USB xHCI) and .3 (USB-C UCSI). If any of those is left bound to a host driver
// such as xhci_hcd, the group is not viable and passthrough fails with
// "vfio: group N is not viable".
//
// Functions are discovered two ways, and the results are unioned:
//
//   - Sibling PCI functions sharing the same domain:bus:device. This is the only way to find
//     the VirtualLink xHCI and USB-C UCSI functions, which do not create a device link back
//     to the GPU.
//   - "consumer:pci:" device links, which the HDA audio function does create.
//
// Siblings are scoped to the physical card rather than to the IOMMU group on purpose: where
// ACS is unavailable a group can span an entire root port, and unbinding unrelated devices
// from their drivers would be harmful.
func getAuxDevices(devicesRoot string, device *nvpci.NvidiaPCIDevice) ([]*nvidiaPCIAuxDevice, error) {
	if !device.IsGPU() {
		return nil, nil
	}

	seen := map[string]bool{device.Address: true}
	var auxDevs []*nvidiaPCIAuxDevice

	add := func(address string) error {
		if address == "" || seen[address] {
			return nil
		}
		path := filepath.Join(devicesRoot, address)
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// The function is not present on this host; there is nothing to bind.
				return nil
			}
			return fmt.Errorf("failed to stat auxiliary device %s: %w", address, err)
		}
		// Auxiliary functions of a GPU are by definition the same vendor. Checking guards
		// against acting on a device we did not intend to touch.
		isNvidia, err := isNvidiaVendor(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// The function was removed between the stat above and this read.
			return nil
		case err != nil:
			return fmt.Errorf("failed to check vendor for auxiliary device %s: %w", address, err)
		case !isNvidia:
			return nil
		}
		driver, err := getDriver(path)
		if err != nil {
			return fmt.Errorf("failed to get driver for auxiliary device %s: %w", address, err)
		}
		seen[address] = true
		auxDevs = append(auxDevs, &nvidiaPCIAuxDevice{
			Path:    path,
			Address: address,
			Driver:  driver,
		})
		return nil
	}

	// Sibling functions: same domain:bus:device, differing only in function number.
	if slotPrefix, _, ok := strings.Cut(device.Address, "."); ok {
		slotPrefix += "."
		entries, err := os.ReadDir(devicesRoot)
		if err != nil {
			return nil, fmt.Errorf("failed to list PCI devices in %s: %w", devicesRoot, err)
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), slotPrefix) {
				continue
			}
			if err := add(entry.Name()); err != nil {
				return nil, err
			}
		}
	}

	// Consumer device links, e.g. the HDA audio function.
	entries, err := os.ReadDir(device.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to list %s: %w", device.Path, err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "consumer") {
			continue
		}
		_, address, ok := strings.Cut(entry.Name(), consumerPrefix)
		if !ok {
			continue
		}
		if err := add(address); err != nil {
			return nil, err
		}
	}

	return auxDevs, nil
}

// isNvidiaVendor reports whether the PCI device at devicePath is an NVIDIA device.
func isNvidiaVendor(devicePath string) (bool, error) {
	contents, err := os.ReadFile(filepath.Join(devicePath, "vendor"))
	if err != nil {
		return false, fmt.Errorf("failed to read vendor of %s: %w", devicePath, err)
	}
	vendorID, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(string(contents)), "0x"), 16, 16)
	if err != nil {
		return false, fmt.Errorf("failed to parse vendor of %s: %w", devicePath, err)
	}
	return uint16(vendorID) == nvpci.PCINvidiaVendorID, nil
}

func getDriver(devicePath string) (string, error) {
	driver, err := filepath.EvalSymlinks(filepath.Join(devicePath, "driver"))
	switch {
	case os.IsNotExist(err):
		return "", nil
	case err == nil:
		return filepath.Base(driver), nil
	}
	return "", err
}

func writeFile(path string, s string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to open file for writing: %w", err)
	}
	defer f.Close()
	if _, err = f.WriteString(s); err != nil {
		return fmt.Errorf("failed writing to file: %w", err)
	}
	return nil
}
