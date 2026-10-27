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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// Name of the counter published in the per-GPU CounterSet for the vGPU
// partitioning scheme.
const (
	vgpuFramebufferCounterName = "framebuffer"
)

// Opaque compatibility group names (KEP-5963) this driver declares on
// consumesCounters entries. Group names are meaningful only within this
// driver's pool.
const (
	// vgpuCompatibilityGroupPrefix prefixes per-profile-family groups: slots
	// of the same vGPU profile family are co-allocatable on one physical GPU,
	// slots of different families are not.
	vgpuCompatibilityGroupPrefix = "vgpu-"

	// Full-GPU and whole-PF VFIO devices carry these disjoint-from-everything
	// groups. Capacity exclusion is provided on top by consuming the entire
	// CounterSet.
	gpuFullCompatibilityGroup = "gpu-full"
	vfioCompatibilityGroup    = "vfio"
)

// vgpuFramework identifies how concrete vGPU devices are managed on the host.
const (
	vgpuFrameworkMdev = "mdev"
	vgpuFrameworkVf   = "vf"
)

// VgpuProfileSpec describes one vGPU profile (type) advertised on a physical
// GPU: the intersection of the vGPU types the host reports as supported and
// the administrator's profile allowlist (--vgpu-profiles).
type VgpuProfileSpec struct {
	// Full vGPU type name as reported by NVML, e.g. "NVIDIA L40S-12Q".
	Name string
	// Numeric vGPU type ID as reported by NVML.
	ProfileID uint32
	// Framebuffer budget this profile consumes from the parent GPU, in bytes.
	FramebufferBytes uint64
	// Number of slot partitions advertised for this profile.
	MaxInstances int
}

// Slug returns a stable, RFC1123-compliant short identifier for the profile,
// used in device names (vgpu-gpu-<minor>-<slug>-<slot>) and in the
// compatibility group name. The convention is the last hyphen-separated token
// of the profile name ("NVIDIA L40S-12Q" -> "12q"), falling back to the
// sanitized full name. Deriving it purely from the profile name keeps device
// names stable across plugin restarts.
func (s *VgpuProfileSpec) Slug() string {
	if idx := strings.LastIndex(s.Name, "-"); idx >= 0 && idx+1 < len(s.Name) {
		if slug := toRFC1123Compliant(s.Name[idx+1:]); slug != "" {
			return slug
		}
	}
	return toRFC1123Compliant(s.Name)
}

// VgpuPartitionInfo describes an abstract, not-yet-incarnated vGPU partition
// of a physical GPU: one slot of one profile. The concrete device (an mdev
// UUID or a VF with a programmed vGPU type) is created at NodePrepare time.
type VgpuPartitionInfo struct {
	Parent  *GpuInfo
	Profile *VgpuProfileSpec
	// Slot indexes the advertised instances of this profile on the parent,
	// 0..MaxInstances-1. It makes each abstract partition individually
	// allocatable; it does not imply placement on hardware.
	Slot int
	// Framework is "mdev" or "vf" (vgpuFramework* constants).
	Framework string
	// SriovSupported reports whether the parent PCI function is SR-IOV
	// capable at all (sriov_totalvfs present). Capability bit, not mode:
	// it does not change when VFs are sliced off or vGPU types are
	// programmed. It spans the GPU-capability axis of the NVIDIA vGPU
	// mdev/vendor-VFIO table (legacy vs. SR-IOV rows), while Framework
	// covers the active VFIO framework axis.
	SriovSupported bool

	// EccMode mirrors the parent GPU's current ECC state from NVML
	// (DeviceGetEccMode). A nil pointer means NVML could not report it —
	// the attribute is then omitted rather than published as a misleading
	// false.
	EccMode *bool
}

func (i *VgpuPartitionInfo) CanonicalName() string {
	return fmt.Sprintf("vgpu-gpu-%d-%s-%d", i.Parent.minor, i.Profile.Slug(), i.Slot)
}

func (i *VgpuPartitionInfo) Attributes() map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"type": {
			StringValue: ptr.To(VgpuDeviceType),
		},
		"profile": {
			StringValue: ptr.To(i.Profile.Name),
		},
		// shortProfileName is the last hyphen-separated token of the profile
		// name, lowercased and sanitized (stable across restarts). It keys
		// slot device names (vgpu-gpu-<minor>-<shortProfileName>-<slot>) and
		// the "vgpu-<shortProfileName>" compatibility group.
		"shortProfileName": {
			StringValue: ptr.To(i.Profile.Slug()),
		},
		"slot": {
			IntValue: ptr.To(int64(i.Slot)),
		},
		// Parent GPU identity, per docs/design/vgpu-support.md section 6.2.2.
		"uuid": {
			StringValue: &i.Parent.UUID,
		},
		"productName": {
			StringValue: &i.Parent.productName,
		},
		"vgpuFramework": {
			StringValue: ptr.To(i.Framework),
		},
		"sriovSupported": {
			BoolValue: ptr.To(i.SriovSupported),
		},
		"profileID": {
			IntValue: ptr.To(int64(i.Profile.ProfileID)),
		},
	}

	if i.EccMode != nil {
		attrs["eccMode"] = resourceapi.DeviceAttribute{BoolValue: i.EccMode}
	}

	addDeviceAttribute(attrs, i.Parent.pciBusIDAttr)
	addDeviceAttribute(attrs, i.Parent.pcieRootAttr)

	return attrs
}

// compatibilityGroup returns the KEP-5963 group name for this partition's
// profile family: slots of the same family are co-allocatable, and any
// different scheme/family (other vgpu-* families, mig, gpu-full, vfio) is
// rejected at schedule time.
func (i *VgpuPartitionInfo) compatibilityGroup() string {
	return vgpuCompatibilityGroupPrefix + i.Profile.Slug()
}

// PartConsumesCounters returns the KEP-4815 counter consumption of this
// abstract partition against the parent GPU's CounterSet: the profile's
// framebuffer cost. The partition device itself is the instance-slot bound:
// only MaxInstances slot devices per profile are advertised.
//
// CompatibilityGroups (KEP-5963) is declared only when the driver's
// DRADeviceCompatibilityGroups gate is enabled; publishing groups while the
// cluster-side gate is off would make kube-scheduler ignore these devices
// entirely (docs/design/vgpu-support.md, section 6.1.1 skew rule 2).
func (i *VgpuPartitionInfo) PartConsumesCounters() []resourceapi.DeviceCounterConsumption {
	consumption := resourceapi.DeviceCounterConsumption{
		CounterSet: i.Parent.VgpuSharedCounterSetName(),
		Counters: map[string]resourceapi.Counter{
			vgpuFramebufferCounterName: {Value: *resource.NewQuantity(int64(i.Profile.FramebufferBytes), resource.BinarySI)},
		},
	}
	if featuregates.Enabled(featuregates.DRADeviceCompatibilityGroups) {
		consumption.CompatibilityGroups = []string{i.compatibilityGroup()}
	}
	return []resourceapi.DeviceCounterConsumption{consumption}
}

// Return the full KEP-4815 representation of an abstract vGPU partition.
func (i *VgpuPartitionInfo) PartGetDevice() resourceapi.Device {
	return resourceapi.Device{
		Name:             i.CanonicalName(),
		Attributes:       i.Attributes(),
		ConsumesCounters: i.PartConsumesCounters(),
	}
}

// parseVgpuProfileAllowlist parses the --vgpu-profiles flag: a comma-separated
// allowlist of full vGPU type names (e.g. "NVIDIA L40S-12Q,NVIDIA L40S-24Q").
func parseVgpuProfileAllowlist(raw string) map[string]bool {
	allowed := make(map[string]bool)
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			allowed[name] = true
		}
	}
	return allowed
}

// Sysfs attribute name of the vendor-specific VFIO vGPU type attribute
// (<VF>/nvidia/current_vgpu_type); only present on RHEL 10 KVM /
// Ubuntu 24.04+ vGPU manager hosts. Naming follows NVIDIA's docs.
const vgpuCurrentTypeFile = "current_vgpu_type"

// sysfsPciDeviceDir returns the sysfs directory of the PCI device with the
// given BDF, rooted at sysfsRoot (the host's sysfs).
func sysfsPciDeviceDir(sysfsRoot string, bdf string) string {
	return filepath.Join(sysfsRoot, "sys", "bus", "pci", "devices", bdf)
}

// detectSriovSupported reports whether the physical function exposes
// sriov_totalvfs (i.e. the GPU is SR-IOV capable), independent of whether
// VFs are currently enabled.
func detectSriovSupported(sysfsRoot string, pciBusID string) bool {
	_, err := os.Stat(filepath.Join(sysfsPciDeviceDir(sysfsRoot, pciBusID), "sriov_totalvfs"))
	return err == nil
}

// detectVgpuFramework determines whether the host manages vGPU devices
// through the mediated framework ("mdev") or the vendor-specific VFIO
// framework ("vf").
//
// The mdev/vf split is hypervisor-driven, not GPU-driven: on SR-IOV-capable
// GPUs, the mediated framework (mdev_supported_types on the VFs, max one
// vGPU per VF) is used on any hypervisor except the vendor-specific VFIO
// flow (per-VF nvidia/current_vgpu_type etc.), which NVIDIA vGPU software
// only exposes on RHEL 10 KVM / Ubuntu 24.04 and newer. So the framework is
// detected by probing the actual attribute the Prepare path would write:
//   - any VF with <VF>/nvidia/current_vgpu_type   → "vf"
//   - <VF>/mdev_supported_types (SR-IOV mdev) or
//     <PF>/mdev_supported_types (legacy)          → "mdev"
//
// When no VF exists yet (admin has not sliced the PF), sysfs probing is
// inconclusive; fall back to NVML's nvmlDeviceGetHostVgpuMode as a hint:
// SR-IOV host vGPU mode → "vf", otherwise "mdev".
func detectVgpuFramework(getHostVgpuMode func() (nvml.HostVgpuMode, nvml.Return), sysfsRoot string, pciBusID string) (framework string) {
	deviceDir := sysfsPciDeviceDir(sysfsRoot, pciBusID)

	// Definitive: any enabled VF exposes the vendor-VFIO type attribute.
	for _, vf := range vgpuVFsOfPF(deviceDir) {
		if _, err := os.Stat(filepath.Join(vgpuVFNVidiaDir(sysfsRoot, vf), vgpuCurrentTypeFile)); err == nil {
			return vgpuFrameworkVf
		}
	}
	// Definitive: mediated devices advertised on the PF or a VF.
	if entries, err := os.ReadDir(filepath.Join(deviceDir, "mdev_supported_types")); err == nil && len(entries) > 0 {
		return vgpuFrameworkMdev
	}
	for _, vf := range vgpuVFsOfPF(deviceDir) {
		if entries, err := os.ReadDir(filepath.Join(sysfsPciDeviceDir(sysfsRoot, vf), "mdev_supported_types")); err == nil && len(entries) > 0 {
			return vgpuFrameworkMdev
		}
	}

	// Inconclusive (no VFs yet): NVML hint.
	mode, ret := getHostVgpuMode()
	if ret == nvml.SUCCESS && mode == nvml.HOST_VGPU_MODE_SRIOV {
		return vgpuFrameworkVf
	}
	return vgpuFrameworkMdev
}

// vgpuVFsOfPF returns the PCI bus IDs of the currently enabled VFs of the PF
// whose sysfs directory is pfDir, or nil when none are enabled.
func vgpuVFsOfPF(pfDir string) []string {
	entries, err := filepath.Glob(filepath.Join(pfDir, "virtfn*"))
	if err != nil {
		return nil
	}
	var vfs []string
	for _, symlink := range entries {
		target, err := filepath.EvalSymlinks(symlink)
		if err != nil {
			continue
		}
		vfs = append(vfs, filepath.Base(target))
	}
	return vfs
}

// vgpuVFNVidiaDir returns the sysfs directory holding the vGPU manager
// attributes for a VF under the vendor-specific VFIO framework:
// <VF>/nvidia/{current_vgpu_type, creatable_vgpu_types, gpu_instance_id}.
func vgpuVFNVidiaDir(sysfsRoot string, vfBusID string) string {
	return filepath.Join(sysfsRoot, "sys", "bus", "pci", "devices", vfBusID, "nvidia")
}
