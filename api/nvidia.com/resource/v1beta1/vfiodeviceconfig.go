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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// VfioDeviceConfig holds the set of parameters for configuring a VFIO device.
type VfioDeviceConfig struct {
	metav1.TypeMeta `json:",inline"`
	Iommu           *IOMMUConfig `json:"iommu,omitempty"`
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
			EnableAPIDevice: ptr.To(false),
		},
	}
}

// Normalize updates a VfioDeviceConfig config with implied default values based on other settings.
func (c *VfioDeviceConfig) Normalize() error {
	if c.Iommu == nil {
		c.Iommu = &IOMMUConfig{
			BackendPolicy:   IOMMUBackendPolicyLegacyOnly,
			EnableAPIDevice: ptr.To(false),
		}
		return nil
	}

	// Don't enable API device if not specified.
	if c.Iommu.EnableAPIDevice == nil {
		c.Iommu.EnableAPIDevice = ptr.To(false)
	}
	return nil
}

// Validate ensures that VfioDeviceConfig has a valid set of values.
func (c *VfioDeviceConfig) Validate() error {
	if c.Iommu == nil {
		return nil
	}
	return c.Iommu.Validate()
}
