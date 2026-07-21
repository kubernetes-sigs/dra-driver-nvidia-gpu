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
	"regexp"
	"strconv"
	"strings"

	"github.com/Masterminds/semver/v3"
	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"
)

// MigSpecTuple is a 3-tuple precisely describing a physical MIG device
// configuration: parent (defined by UUID or minor), placement start index, and
// (GI) MIG profile ID. The profile ID implies memory slice count. This
// representation is abstract. It does not carry MIG device identity (UUID), and
// does not express whether that device currently exists.
type MigSpecTuple struct {
	ParentMinor    GPUMinor
	ParentPCIBusID PCIBusID
	// What is commonly called "MIG profile ID" typically refers to the GPU
	// Instance Profile ID. The GI profile ID is also what's emitted by
	// `nvidia-smi mig -lgip` for each profile -- it directly corresponds to a
	// specific human-readable profile string such as "1g.5gb". For programmatic
	// management, it is the better profile representation than the string. Most
	// importantly, the profile defines slice count and memory size. Another way
	// to describe GI profile ID: this is the ID that one passes into
	// nvmlDeviceCreateGpuInstance() to create the partition.
	ProfileID      int
	PlacementStart int
}

// Minimal, precise representation of a specific, created MIG device.
//
// After creation and during its lifetime, a specific MIG device can be
// identified by the following 3-tuple: the parent GPU (UUID/minor), the GPU
// Instance (GI) identifier, and the Compute Instance (CI) identifier. The
// GIID/CIID-based tracking is however only safe for as long as the very same
// device is known to be alive (otherwise those IDs may refer to a different
// device than assumed because they may be re-used for a potentially different
// physical configuration -- at least, there doesn't seem to be any guarantee
// that that is not the case). Hence, another parameter is tracked: the MIG
// device UUID. A MIG device UUID changes across destruction/re-creation of the
// same physical configuration. This uuid can therefore be used for example to
// distinguish actual vs. expected MIG device UUID after looking up a MIG device
// by a (parent, CIID, GIID) tuple. What's expressed above, in other words: as
// far as I understand, there is no guaranteed relationship between GIID+CIID on
// the one hand and profileID+placementStart on the other hand.
type MigLiveTuple struct {
	ParentMinor GPUMinor `json:"parentMinor"`
	GIID        int      `json:"giId"`
	CIID        int      `json:"ciId"`
	MigUUID     string   `json:"migUUID"`

	// Not orthogonal to `ParentMinor`, but convenient for consumers.
	ParentUUID string `json:"parentUUID"`
	// `omitempty`: postdates 25.12.0; emitting "parentPCIBusID":"" would
	// trip CorruptCheckpointError on upgrade. See issue 1080.
	ParentPCIBusID PCIBusID `json:"parentPCIBusID,omitempty"`
}

// MigSpec is similar to `MigSpecTuple` as it also fundamentally encodes the
// three Ps: parent, profile, and placement. In that sense, it is an abstract
// description of a specific MIG device configuration. Compared to
// `MigSpecTuple`, though, the properties in this struct are richer objects for
// convenience.
type MigSpec struct {
	Parent        *GpuInfo
	Profile       nvdev.MigProfile
	GIProfileInfo nvml.GpuInstanceProfileInfo
	Placement     nvml.GpuInstancePlacement
}

func (m *MigSpec) Tuple() *MigSpecTuple {
	return &MigSpecTuple{
		ParentMinor:    m.Parent.minor,
		ParentPCIBusID: m.Parent.pciBusID,
		ProfileID:      int(m.GIProfileInfo.Id),
		PlacementStart: int(m.Placement.Start),
	}
}

// Turns MigSpecTuple into a canonical MIG device name. Currently, this needs
// additional input: the MIG profile name (deliberately not stored on the type
// because it is not a fundamental dimension; but directly implied by profile
// ID). Note that there is no absolute standard for constructing a profile name.
// This is currently done by go-nvlib in `func (p MigProfileInfo) String()` to
// resemble the output of nvidia-smi. For now, the profile name (in the DRA
// device's canonical name) is relied upon for _sorting_ devices within a
// resource slice, and that order is relevant for the k8s scheduler which
// iterates through devices from top to bottom and picks the first match.
func (m *MigSpecTuple) ToCanonicalName(profileName string) DeviceName {
	pname := toRFC1123Compliant(strings.ReplaceAll(profileName, ".", ""))
	return fmt.Sprintf("gpu-%d-mig-%s-%d-%d", m.ParentMinor, pname, m.ProfileID, m.PlacementStart)
}

// CommonCapacitiesMig returns the relevant GPU instance profile properties. The
// device capacity map returned here does however not contain memory slices.
func CommonCapacitiesMig(p *nvml.GpuInstanceProfileInfo) map[resourceapi.QualifiedName]resourceapi.DeviceCapacity {
	return map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
		"multiprocessors": intcap(p.MultiprocessorCount),
		"copyEngines":     intcap(p.CopyEngineCount),
		"decoders":        intcap(p.DecoderCount),
		"encoders":        intcap(p.EncoderCount),
		"jpegEngines":     intcap(p.JpegCount),
		"ofaEngines":      intcap(p.OfaCount),
		// `memory` here is by convention announced in "Bytes". The MIG
		// profile's MemorySizeMB` property comes straight from NVML and is
		// documented in the public API docs with "Memory size in MBytes". As it
		// says "MB" and not MiB", one could assume that unit to be 10^6 Bytes.
		// However, in nvml.h this type's property is documented with
		// `memorySizeMB; //!< Device memory size (in MiB)`. Hence, the unit is
		// 2^20 Bytes (1024 * 1024 Bytes).
		"memory": intcap(int64(p.MemorySizeMB * 1024 * 1024)),
	}
}

// CommonAttributesMig returns device attributes for MIG devices that are
// in common among the 'static MIG' and 'dynamic MIG' cases.
func CommonAttributesMig(parent *GpuInfo, profileName string) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"type": {
			StringValue: ptr.To("mig"),
		},
		"parentUUID": {
			StringValue: &parent.UUID,
		},
		"profile": {
			StringValue: ptr.To(profileName),
		},
		"productName": {
			StringValue: &parent.productName,
		},
		"brand": {
			StringValue: &parent.brand,
		},
		"architecture": {
			StringValue: &parent.architecture,
		},
		"cudaComputeCapability": {
			VersionValue: ptr.To(semver.MustParse(parent.cudaComputeCapability).String()),
		},
		"driverVersion": {
			VersionValue: ptr.To(semver.MustParse(parent.driverVersion).String()),
		},
		"cudaDriverVersion": {
			VersionValue: ptr.To(semver.MustParse(parent.cudaDriverVersion).String()),
		},
	}

	if parent.addressingMode != nil {
		attrs["addressingMode"] = resourceapi.DeviceAttribute{
			StringValue: parent.addressingMode,
		}
	}

	addDeviceAttribute(attrs, parent.pciBusIDAttr)
	addDeviceAttribute(attrs, parent.pcieRootAttr)
	addDeviceAttribute(attrs, parent.numaNodeAttr)

	return attrs
}

// NewMigSpecTupleFromCanonicalName() attempts to parse a canonical MIG device
// name into a MigSpecTuple struct.
func NewMigSpecTupleFromCanonicalName(n DeviceName) (*MigSpecTuple, error) {
	matches := canonicalMigNameRegex.FindStringSubmatch(string(n))
	if matches == nil {
		return nil, fmt.Errorf("failed to match MIG device name regex: '%s'", n)
	}

	// matches[0]: the whole string
	// matches[1]: ParentMinor
	// matches[2]: MIG profile name (ignore this for building the struct)
	// matches[3]: ProfileID
	// matches[4]: PlacementStart

	// The regex guarantees that these groups are digits, and the expected
	// values are small. That is, Atoi() errors are not expected. Handle them
	// anyway for correctness.
	parentMinor, err1 := strconv.Atoi(matches[1])
	profileID, err2 := strconv.Atoi(matches[3])
	placementStart, err3 := strconv.Atoi(matches[4])

	if err1 != nil || err2 != nil || err3 != nil {
		return nil, fmt.Errorf("integer parsing failed (dev name: %s)", n)
	}

	return &MigSpecTuple{
		ParentMinor:    GPUMinor(parentMinor),
		ProfileID:      profileID,
		PlacementStart: placementStart,
	}, nil
}

func (m *MigSpec) CanonicalName() DeviceName {
	return m.Tuple().ToCanonicalName(m.Profile.String())
}

type MigProfileInfo struct {
	profile    nvdev.MigProfile
	placements []*MigDevicePlacement
}

func (p MigProfileInfo) String() string {
	return p.profile.String()
}

type MigDevicePlacement struct {
	nvml.GpuInstancePlacement
}

// Capture 4 groups:
// 1. ParentMinor (digits)
// 2. Profile name (greedy match of anything in the middle)
// 3. ProfileID (digits)
// 4. PlacementStart (digits)
// Anchors ^ and $ ensure that the exact, full string must match.
var canonicalMigNameRegex = regexp.MustCompile(`^gpu-(\d+)-mig-(.+)-(\d+)-(\d+)$`)

// toRFC1123Compliant converts the input to a DNS name compliant with RFC 1123.
// Note that a device name in DRA must not contain dots either (this function
// does not always return a string that can be used as a device name).
func toRFC1123Compliant(name string) string {
	name = strings.ToLower(name)
	re := regexp.MustCompile(`[^a-z0-9-.]`)
	name = re.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	name = strings.TrimSuffix(name, ".")

	// Can this ever hurt? Should we error out?
	if len(name) > 253 {
		name = name[:253]
	}

	return name
}

func camelToDNSName(s string) string {
	// Insert hyphen before uppercase letters that follow lowercase/digits
	// or before the last uppercase in a sequence of uppercase letters
	re1 := regexp.MustCompile("([a-z0-9])([A-Z])")
	result := re1.ReplaceAllString(s, "$1-$2")

	// Insert hyphen before an uppercase letter followed by lowercase (handles HTTPServer -> HTTP-Server)
	re2 := regexp.MustCompile("([A-Z]+)([A-Z][a-z])")
	result = re2.ReplaceAllString(result, "$1-$2")

	return toRFC1123Compliant(result)
}
