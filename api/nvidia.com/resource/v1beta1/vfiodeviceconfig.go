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

package v1beta1

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// VfioDeviceConfig holds the set of parameters for configuring a VFIO device.
type VfioDeviceConfig struct {
	metav1.TypeMeta `json:",inline"`
	// DriverOverride specifies the passthrough driver to use for the VFIO device.
	// This is relevant in systems like the NVIDIA Grace where we may want to
	// override the default `nvgrace_gpu_vfio_pci` passthrough driver variant
	// Supported values are:
	// - vfio-pci: Use the plain `vfio-pci` driver.
	DriverOverride *PassthroughDriver `json:"driverOverride,omitempty"`
	Iommu          *IOMMUConfig       `json:"iommu,omitempty"`
}

// DefaultVfioDeviceConfig provides the default configuration of a VFIO device.
func DefaultVfioDeviceConfig() *VfioDeviceConfig {
	if !featuregates.Enabled(featuregates.PassthroughSupport) {
		return nil
	}
	return &VfioDeviceConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: GroupName + "/" + Version,
			Kind:       VfioDeviceConfigKind,
		},
		Iommu: &IOMMUConfig{
			BackendPolicy:   IOMMUBackendPolicyLegacyOnly,
			EnableAPIDevice: ptr.To(true),
		},
	}
}

// Normalize updates a VfioDeviceConfig config with implied default values based on other settings.
func (c *VfioDeviceConfig) Normalize() error {
	if c.Iommu == nil {
		c.Iommu = &IOMMUConfig{
			BackendPolicy:   IOMMUBackendPolicyLegacyOnly,
			EnableAPIDevice: ptr.To(true),
		}
		return nil
	}

	// Default to LegacyOnly backend policy if not specified.
	if c.Iommu.BackendPolicy == "" {
		c.Iommu.BackendPolicy = IOMMUBackendPolicyLegacyOnly
	}

	// Enable the IOMMU API device by default if not specified, since KubeVirt
	// (the primary vfio consumer) requires it. Explicitly setting it to false
	// opts out.
	if c.Iommu.EnableAPIDevice == nil {
		c.Iommu.EnableAPIDevice = ptr.To(true)
	}
	return nil
}

// Validate ensures that VfioDeviceConfig has a valid set of values.
func (c *VfioDeviceConfig) Validate() error {
	if c.DriverOverride != nil {
		if err := c.DriverOverride.Validate(); err != nil {
			return err
		}
	}
	if c.Iommu == nil {
		return nil
	}
	return c.Iommu.Validate()
}

// PassthroughDriver is the gpu driver to use for passthrough.
type PassthroughDriver string

const (
	// PassthroughDriverVfioPci is the vfio-pci driver.
	PassthroughDriverVfioPci PassthroughDriver = "vfio-pci"
)

// Validate ensures the passthrough driver is valid.
// Only vfio-pci is supported.
func (d PassthroughDriver) Validate() error {
	switch d {
	case PassthroughDriverVfioPci:
		return nil
	default:
		return fmt.Errorf("invalid passthrough driver: %q", d)
	}
}
