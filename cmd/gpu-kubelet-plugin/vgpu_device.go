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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/NVIDIA/go-nvlib/pkg/nvmdev"
	"github.com/NVIDIA/go-nvlib/pkg/nvpci"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
)

// VgpuConcrete identifies the concrete vGPU device created for a claim at
// Prepare time: either a mediated device (mdev UUID) or, for SR-IOV-backed
// vGPU, a VF whose vGPU type was programmed. Serialized to the checkpoint.
type VgpuConcrete struct {
	// Framework is "mdev" or "vf" (the vgpuFramework* constants).
	Framework string `json:"framework"`
	// ProfileID and Profile describe what was created (for diagnostics and for
	// validating checkpointed state against republished partitions).
	ProfileID uint32 `json:"profileID"`
	Profile   string `json:"profile"`

	// ParentPCIBusID is the BDF of the physical GPU PF hosting this device.
	ParentPCIBusID string `json:"parentPCIBusID"`

	// MdevUUID is the mediated device UUID (mdev framework only).
	MdevUUID string `json:"mdevUUID,omitempty"`
	// VFPCIBusID is the BDF of the programmed VF (vdev framework only).
	VFPCIBusID string `json:"vfPCIBusID,omitempty"`
}

// id returns the stable host-level identifier of the concrete device, used
// for ownership marker file names.
func (c *VgpuConcrete) id() string {
	if c.Framework == vgpuFrameworkVf {
		return c.VFPCIBusID
	}
	return c.MdevUUID
}

// vgpuWriteFile is a seam for tests: on real hosts, writing a UUID to an
// mdev type's sysfs "create" file makes the kernel instantiate device state
// (a side effect a plain temp filesystem cannot emulate); likewise writing
// to "remove" tears it down.
var vgpuWriteFile = os.WriteFile

// vgpuMdevSysfsTypeName matches the NVIDIA vGPU Manager's mdev type naming
// convention: mdev_supported_types directory entries are named
// "nvidia-<profileID>".
func vgpuMdevSysfsTypeName(profileID uint32) string {
	return fmt.Sprintf("nvidia-%d", profileID)
}

// vgpuMdevTypeName maps a full NVML vGPU type name (e.g. "GRID A100-40C",
// "NVIDIA L40S-12Q") to the short mdev type name the NVIDIA vGPU Manager
// registers in sysfs ("A100-40C", "L40S-12Q"). Mirrors nvmdev's convention
// of taking the last space-separated token.
func vgpuMdevTypeName(profile string) string {
	fields := strings.Fields(profile)
	if len(fields) == 0 {
		return profile
	}
	return fields[len(fields)-1]
}

// vgpuMdevDevicesRoot is the kernel's canonical mdev instance directory
// (same path nvmdev hardcodes in go-nvlib v0.12). Read directly from the
// host sysfs view inside the privileged plugin container.
const vgpuMdevDevicesRoot = "/sys/bus/mdev/devices"

// nvidiaVgpuVfioModule is the kernel module of the NVIDIA vGPU mediated
// device (vfio-mdev) driver. It is loaded by the NVIDIA vGPU Manager; an
// mdev framework host without it cannot create vGPU instances, and failing
// late (mdev type dir missing) is much harder to diagnose than failing here
// (mirrors the PassthroughSupport module check in VfioPciManager.Configure).
const nvidiaVgpuVfioModule = "nvidia_vgpu_vfio"

// isVgpuVfioModuleLoaded checks whether nvidia_vgpu_vfio is loaded. It
// checks the module's directory under /sys/module via the same convention as
// checkKernelModuleLoaded (vfio-device.go), preferring the host root mount
// and falling back to the plugin container's own sysfs view (shared with the
// host).
func (l deviceLib) isVgpuVfioModuleLoaded() (bool, error) {
	for _, root := range []string{l.hostRoot, "/"} {
		if root == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, sysModulePath)); errors.Is(err, fs.ErrNotExist) {
			continue // this root does not carry a sysfs view
		} else if err != nil {
			return false, err
		}
		f, err := os.Stat(filepath.Join(root, sysModulePath, nvidiaVgpuVfioModule))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return false, nil
		case err != nil:
			return false, fmt.Errorf("failed to check if module %q is loaded: %w", nvidiaVgpuVfioModule, err)
		default:
			return f.IsDir(), nil
		}
	}
	return false, fmt.Errorf("cannot determine whether module %q is loaded (no /sys/module under %v)", nvidiaVgpuVfioModule, []string{l.hostRoot, "/"})
}

// sysfsMdevDeviceDir returns the sysfs directory of a potential mediated
// device instance.
func sysfsMdevDeviceDir(sysfsRoot string, devUUID string) string {
	return filepath.Join(sysfsRoot, "sys", "bus", "mdev", "devices", devUUID)
}

// Sysfs attribute names used by the NVIDIA vGPU Manager. Under the
// mediated ("mdev") framework: mdev_supported_types/nvidia-<profileID>/{create,
// available_instances} on the PF (legacy GPUs) or on a VF (SR-IOV GPUs), and
// one instance dir per device under /sys/bus/mdev/devices/<uuid>. Under the
// vendor-specific VFIO ("vf") framework: per-VF attributes in
// <VF>/nvidia/{current_vgpu_type, creatable_vgpu_types}.
//
// vgpuCurrentTypeFile is declared alongside the framework detection in
// vgpu.go, which probes the same attribute.
const (
	// vgpuCreatableTypesFile lists the vGPU type names and sysfs type IDs
	// creatable on a VF ("NVIDIA A40-2Q    558"); it becomes empty once the
	// one permitted vGPU has been created on that VF.
	vgpuCreatableTypesFile = "creatable_vgpu_types"
)

// createVgpuDevice creates the concrete vGPU device for an allocated
// partition: an mdev instance under the mdev framework, or a programmed VF
// under the vdev (SR-IOV) framework. Returns the concrete identity for
// checkpointing. Callers must serialize create/delete against Unprepare
// (the driver's global prepare/unprepare lock guarantees this).
func (l deviceLib) createVgpuDevice(partition *VgpuPartitionInfo, params map[string]string) (*VgpuConcrete, error) {
	concrete := &VgpuConcrete{
		Framework:      partition.Framework,
		ProfileID:      partition.Profile.ProfileID,
		Profile:        partition.Profile.Name,
		ParentPCIBusID: partition.Parent.pciBusID,
	}

	sysfsRoot := l.sysfsRoot
	if sysfsRoot == "" {
		sysfsRoot = "/"
	}

	var err error
	switch partition.Framework {
	case vgpuFrameworkMdev:
		err = l.createMdevVgpu(sysfsRoot, concrete, params)
	case vgpuFrameworkVf:
		err = l.setVFVgpuType(sysfsRoot, concrete, params)
	default:
		err = fmt.Errorf("unknown vGPU framework %q", partition.Framework)
	}
	if err != nil {
		return nil, err
	}

	return concrete, nil
}

// deleteVgpuDevice destroys a concrete vGPU device previously created by
// createVgpuDevice. Idempotent: a device that no longer exists is already
// gone, which is the correct outcome for retried Unprepare calls.
func (l deviceLib) deleteVgpuDevice(concrete *VgpuConcrete) error {
	sysfsRoot := l.sysfsRoot
	if sysfsRoot == "" {
		sysfsRoot = "/"
	}

	switch concrete.Framework {
	case vgpuFrameworkMdev:
		return l.deleteMdevVgpu(concrete)
	case vgpuFrameworkVf:
		return l.clearVFVgpuType(sysfsRoot, concrete)
	}
	return fmt.Errorf("unknown vGPU framework %q", concrete.Framework)
}

// vgpuMdevParent describes one candidate mdev parent: either the PF itself
// (legacy GPUs) or one of its VFs (SR-IOV mdev hosts).
type vgpuMdevParent struct {
	// Address is the PCI bus ID of the parent device hosting the mdev type.
	Address string
	// AvailableInstances of the requested mdev type on this parent; zero
	// means occupied/exhausted, negative means the parent does not advertise
	// the type at all.
	AvailableInstances int
}

// vgpuMdevStore abstracts the mediated-device host operations so the
// nvmdev-backed implementation can be replaced in unit tests.
type vgpuMdevStore interface {
	// ParentsWithAvailability lists the PF and all its VFs that may host
	// the given mdev type, with their current availability counts.
	ParentsWithAvailability(pciBusID string, mdevType string) ([]vgpuMdevParent, error)
	CreateMdev(parentAddress string, mdevType string, devUUID string) error
	// DeleteMdev must be idempotent.
	DeleteMdev(devUUID string) error
}

// nvmdevMdevStore is the production vgpuMdevStore implementation, delegating
// the sysfs mechanics to NVIDIA's go-nvlib/pkg/nvmdev package (the same
// library backing the NVIDIA vgpu-device-manager).
type nvmdevMdevStore struct {
	nvmdev nvmdev.Interface
}

func newNvmdevMdevStore(nvpci nvpci.Interface) *nvmdevMdevStore {
	return &nvmdevMdevStore{nvmdev: nvmdev.New(nvmdev.WithNvpciLib(nvpci))}
}

func (s *nvmdevMdevStore) ParentsWithAvailability(pciBusID string, mdevType string) ([]vgpuMdevParent, error) {
	parents, err := s.nvmdev.GetAllParentDevices()
	if err != nil {
		return nil, fmt.Errorf("error listing mdev parent devices: %w", err)
	}

	var out []vgpuMdevParent
	for _, p := range parents {
		pf := p.GetPhysicalFunction()
		if pf == nil || pf.Address != pciBusID {
			continue
		}
		avail, err := p.GetAvailableMDEVInstances(mdevType)
		if err != nil {
			return nil, fmt.Errorf("error getting available instances of %q on %q: %w", mdevType, p.Address, err)
		}
		out = append(out, vgpuMdevParent{Address: p.Address, AvailableInstances: avail})
	}
	return out, nil
}

func (s *nvmdevMdevStore) CreateMdev(parentAddress string, mdevType string, devUUID string) error {
	parent, err := s.parentByAddress(parentAddress)
	if err != nil {
		return err
	}
	return parent.CreateMDEVDevice(mdevType, devUUID)
}

func (s *nvmdevMdevStore) DeleteMdev(devUUID string) error {
	// nvmdev reads /sys/bus/mdev/devices; when no mdev subsystem exists on
	// the host at all, deletion is trivially complete (idempotent).
	if _, err := os.Stat(vgpuMdevDevicesRoot); os.IsNotExist(err) {
		return nil
	}
	devices, err := s.nvmdev.GetAllDevices()
	if err != nil {
		return fmt.Errorf("error listing mdev devices: %w", err)
	}
	for _, d := range devices {
		if d.UUID == devUUID {
			return d.Delete()
		}
	}
	klog.V(4).Infof("vGPU instance %s already gone; nothing to delete", devUUID)
	return nil
}

func (s *nvmdevMdevStore) parentByAddress(address string) (*nvmdev.ParentDevice, error) {
	parents, err := s.nvmdev.GetAllParentDevices()
	if err != nil {
		return nil, fmt.Errorf("error listing mdev parent devices: %w", err)
	}
	for _, p := range parents {
		if p.Address == address {
			return p, nil
		}
	}
	return nil, fmt.Errorf("mdev parent device %q not found", address)
}

// createMdevVgpu creates a mediated device of the given vGPU type on the
// parent physical GPU. On legacy (non-SR-IOV) GPUs the vGPU Manager exposes
// mdev_supported_types on the PF; on SR-IOV GPUs using the mdev framework it
// exposes them per VF, with at most one vGPU per VF. Params are applied to
// the instance's vgpu_params file when the vGPU Manager exposes one.
func (l deviceLib) createMdevVgpu(sysfsRoot string, concrete *VgpuConcrete, params map[string]string) error {
	if l.vgpuMdev == nil {
		return fmt.Errorf("mdev store not initialized")
	}
	// Fail fast with an actionable error when the host lacks the NVIDIA vGPU
	// mediated-device driver; otherwise mdev creation would fail later with a
	// bare "type not found".
	if loaded, err := l.isVgpuVfioModuleLoaded(); err != nil {
		return err
	} else if !loaded {
		return fmt.Errorf("kernel module %q is not loaded on this node; ensure the NVIDIA vGPU Manager is installed and its services are running", nvidiaVgpuVfioModule)
	}
	mdevType := vgpuMdevTypeName(concrete.Profile)

	parents, err := l.vgpuMdev.ParentsWithAvailability(concrete.ParentPCIBusID, mdevType)
	if err != nil {
		return fmt.Errorf("error enumerating vGPU parents of GPU %q: %w", concrete.ParentPCIBusID, err)
	}

	// Prefer the PF (legacy GPUs); on SR-IOV mdev hosts a VF is free exactly
	// when its single permitted instance is available.
	var chosen *vgpuMdevParent
	for i := range parents {
		if parents[i].AvailableInstances <= 0 {
			continue
		}
		switch {
		case parents[i].Address == concrete.ParentPCIBusID:
			chosen = &parents[i]
		case chosen == nil && parents[i].AvailableInstances == 1:
			chosen = &parents[i]
		}
		if chosen != nil && chosen.Address == concrete.ParentPCIBusID {
			break
		}
	}
	if chosen == nil {
		return fmt.Errorf("vGPU type %q (%s) not found or busy on GPU %q and its VFs (vGPU Manager may not be running, the type may not be creatable, or all VFs are occupied)",
			concrete.Profile, vgpuMdevSysfsTypeName(concrete.ProfileID), concrete.ParentPCIBusID)
	}
	if chosen.Address != concrete.ParentPCIBusID {
		concrete.VFPCIBusID = chosen.Address
	}

	devUUID := uuid.New().String()
	klog.V(4).Infof("Creating vGPU instance (type %q, mdev UUID %s) on parent %s (GPU %q)",
		concrete.Profile, devUUID, chosen.Address, concrete.ParentPCIBusID)
	if err := l.vgpuMdev.CreateMdev(chosen.Address, mdevType, devUUID); err != nil {
		return fmt.Errorf("error creating vGPU instance of type %q on GPU %q: %w",
			concrete.Profile, concrete.ParentPCIBusID, err)
	}

	deviceDir := sysfsMdevDeviceDir(sysfsRoot, devUUID)
	if _, err := os.Lstat(deviceDir); err != nil {
		return fmt.Errorf("created vGPU instance %s not visible in sysfs at %q: %w", devUUID, deviceDir, err)
	}
	concrete.MdevUUID = devUUID

	if err := applyVgpuParams(deviceDir, params); err != nil {
		// Roll back the just-created instance: a partition created with
		// partial parameters must not escape Prepare.
		if derr := l.deleteMdevVgpu(concrete); derr != nil {
			klog.Warningf("failed to roll back vGPU instance %s after param application failure: %v", devUUID, derr)
		}
		return err
	}

	return nil
}

// deleteMdevVgpu removes a mediated device instance. Deletion is idempotent
// (see nvmdevMdevStore.DeleteMdev), which is the correct outcome for retried
// Unprepare calls.
func (l deviceLib) deleteMdevVgpu(concrete *VgpuConcrete) error {
	if concrete.MdevUUID == "" {
		return fmt.Errorf("cannot delete mdev-based vGPU device: no mdev UUID recorded")
	}
	if l.vgpuMdev == nil {
		return fmt.Errorf("mdev store not initialized")
	}
	return l.vgpuMdev.DeleteMdev(concrete.MdevUUID)
}

// applyVgpuParams writes parameters into the instance's vgpu_params sysfs
// file (one "key=value" per write), if the vGPU Manager exposes one.
func applyVgpuParams(deviceDir string, params map[string]string) error {
	if len(params) == 0 {
		return nil
	}
	paramsPath := filepath.Join(deviceDir, "nvidia", "vgpu_params")
	if _, err := os.Lstat(paramsPath); err != nil {
		return fmt.Errorf("vgpu_params file not available for vGPU instance %q: %w", deviceDir, err)
	}
	for key, value := range params {
		klog.V(4).Infof("Setting vGPU param %q=%q on %s", key, value, deviceDir)
		if err := os.WriteFile(paramsPath, []byte(fmt.Sprintf("%s=%s", key, value)), 0200); err != nil {
			return fmt.Errorf("error setting vGPU param %q on %s: %w", key, deviceDir, err)
		}
	}
	return nil
}

// setVFVgpuType programs the given vGPU type onto a free VF of the parent
// GPU's PF (vendor-specific VFIO, or "vf", framework.
func (l deviceLib) setVFVgpuType(sysfsRoot string, concrete *VgpuConcrete, params map[string]string) error {
	if len(params) > 0 {
		return fmt.Errorf("vgpu params are not supported for the vdev framework (profile %q)", concrete.Profile)
	}

	pfDir := sysfsPciDeviceDir(sysfsRoot, concrete.ParentPCIBusID)

	numvfs, err := l.sriovNumVFs(pfDir)
	if err != nil {
		return err
	}
	if numvfs == 0 {
		return fmt.Errorf("no SR-IOV VFs enabled on GPU %q; enable them on the host with NVIDIA's sriov-manage script (/usr/lib/nvidia/sriov-manage -e %s)", concrete.ParentPCIBusID, concrete.ParentPCIBusID)
	}

	vfs, err := l.listVFs(pfDir)
	if err != nil {
		return err
	}

	for _, vf := range vfs {
		nvidiaDir := vgpuVFNVidiaDir(sysfsRoot, vf)
		typePath := filepath.Join(nvidiaDir, vgpuCurrentTypeFile)

		current, err := readTrimmed(typePath)
		if err != nil {
			// The vGPU Manager only exposes these attributes on VFs it
			// manages; skip VFs without them.
			continue
		}
		if current != "" && current != "0" {
			continue // a vGPU already lives on this VF
		}

		// The sysfs type ID must be creatable on this VF; writing an invalid
		// or uncreatable ID fails and resets current_vgpu_type to 0
		// (documented behavior), so validate first for a clear error.
		creatable, err := l.vgpuTypeCreatableOnVF(nvidiaDir, concrete)
		if err != nil || !creatable {
			continue
		}

		if err := vgpuWriteFile(typePath, []byte(fmt.Sprintf("%d", concrete.ProfileID)), 0200); err != nil {
			return fmt.Errorf("error setting vGPU type %d (%q) on VF %q: %w", concrete.ProfileID, concrete.Profile, vf, err)
		}
		got, err := readTrimmed(typePath)
		if err != nil || got != strconv.Itoa(int(concrete.ProfileID)) {
			return fmt.Errorf("verify failed: VF %q reports %s=%q after programming type %d (%q)", vf, vgpuCurrentTypeFile, got, concrete.ProfileID, concrete.Profile)
		}

		klog.V(4).Infof("Programmed vGPU type %q (id %d) on VF %s of GPU %q", concrete.Profile, concrete.ProfileID, vf, concrete.ParentPCIBusID)
		concrete.VFPCIBusID = vf
		return nil
	}

	return fmt.Errorf("no free VF with a creatable vGPU type %q (id %d) found on GPU %q", concrete.Profile, concrete.ProfileID, concrete.ParentPCIBusID)
}

// vgpuTypeCreatableOnVF reads the VF's creatable_vgpu_types file and reports
// whether the concrete's vGPU type is creatable there. The file contains
// lines of the form "NVIDIA A40-2Q         558"; match on the profile name
// AND the integer ID to guard against name/ID skew between NVML and sysfs.
func (l deviceLib) vgpuTypeCreatableOnVF(nvidiaDir string, concrete *VgpuConcrete) (bool, error) {
	data, err := os.ReadFile(filepath.Join(nvidiaDir, vgpuCreatableTypesFile))
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		id, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			continue
		}
		name := strings.Join(fields[:len(fields)-1], " ")
		if uint32(id) == concrete.ProfileID && name == concrete.Profile {
			return true, nil
		}
	}
	return false, nil
}

// clearVFVgpuType resets a VF's vGPU type to 0 (no vGPU), the documented
// means of deleting a vGPU under the vendor-specific VFIO framework.
// Idempotent so retried Unprepare calls are safe.
func (l deviceLib) clearVFVgpuType(sysfsRoot string, concrete *VgpuConcrete) error {
	if concrete.VFPCIBusID == "" {
		return fmt.Errorf("cannot clear vdev-based vGPU device: no VF PCI bus ID recorded")
	}
	typePath := filepath.Join(vgpuVFNVidiaDir(sysfsRoot, concrete.VFPCIBusID), vgpuCurrentTypeFile)
	if _, err := os.Lstat(typePath); err != nil {
		if os.IsNotExist(err) {
			klog.V(4).Infof("VF %s has no %s attribute; vGPU already cleared", concrete.VFPCIBusID, vgpuCurrentTypeFile)
			return nil
		}
		return err
	}
	return vgpuWriteFile(typePath, []byte("0"), 0200)
}

func (l deviceLib) sriovNumVFs(pfDir string) (int, error) {
	data, err := os.ReadFile(filepath.Join(pfDir, "sriov_numvfs"))
	if err != nil {
		return 0, fmt.Errorf("cannot read SR-IOV state of %q: %w", pfDir, err)
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

// listVFs returns the PCI bus IDs of all VFs of the PF, by resolving the
// PF's virtfn* symlinks.
func (l deviceLib) listVFs(pfDir string) ([]string, error) {
	entries, err := filepath.Glob(filepath.Join(pfDir, "virtfn*"))
	if err != nil {
		return nil, err
	}
	var vfs []string
	for _, symlink := range entries {
		target, err := filepath.EvalSymlinks(symlink)
		if err != nil {
			return nil, fmt.Errorf("error resolving VF symlink %q: %w", symlink, err)
		}
		vfs = append(vfs, filepath.Base(target))
	}
	return vfs, nil
}

func readTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// prepareVgpuDevice creates the concrete vGPU device for an allocated
// partition. Return values are checkpointed by the caller.
//
// TODO: the concrete device exists on the host before the caller commits it
// to the checkpoint. A crash or a failed Prepare in that window leaves an
// orphaned mdev instance (or a VF with a programmed vGPU type) that nothing
// currently reclaims: the checkpoint never learned about it, and it survives
// until the node reboots. Record the concrete identity in the checkpoint as
// soon as it is created -- i.e. while the claim is still in PrepareStarted --
// so Prepare-retry rollback and a startup sweep can both free it from a
// single source of truth. Note that persisting during PrepareStarted breaks
// the current invariant that PreparedDevices is only meaningful for claims in
// PrepareCompleted;
func (s *DeviceState) prepareVgpuDevice(partition *VgpuPartitionInfo) (*VgpuConcrete, error) {
	return s.nvdevlib.createVgpuDevice(partition, nil)
}

// unprepareVgpuDevices destroys the concrete vGPU devices created for a
// claim. Retried Unprepare is safe: deletions are idempotent.
func (s *DeviceState) unprepareVgpuDevices(devices PreparedDeviceList) error {
	for _, device := range devices {
		concrete := device.Vgpu.Concrete
		if err := s.nvdevlib.deleteVgpuDevice(concrete); err != nil {
			return fmt.Errorf("error deleting vGPU device %s: %w", concrete.id(), err)
		}
	}
	return nil
}

// vgpuMetadataAttributes builds the Device Metadata attributes (KEP-5304)
// for a prepared vGPU device: the partition's published attributes plus the
// concrete identity that KubeVirt binds. Key names follow KubeVirt's
// convention: `mdevUUID` for the mdev framework and `pciBusID` (the VF's
// address, not the parent's) for the vendor-VFIO framework.
func vgpuMetadataAttributes(partition *VgpuPartitionInfo, concrete *VgpuConcrete) map[string]resourceapi.DeviceAttribute {
	attrs := make(map[string]resourceapi.DeviceAttribute)
	for k, v := range partition.Attributes() {
		attrs[string(k)] = v
	}
	if concrete.MdevUUID != "" {
		attrs["mdevUUID"] = resourceapi.DeviceAttribute{StringValue: &concrete.MdevUUID}
	}
	if concrete.VFPCIBusID != "" {
		attrs["pciBusID"] = resourceapi.DeviceAttribute{StringValue: &concrete.VFPCIBusID}
	}
	return attrs
}
