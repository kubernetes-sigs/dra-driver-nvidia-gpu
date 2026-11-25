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

import "fmt"

// These constants represent the different IOMMU backend selection policies.
const (
	IOMMUBackendPolicyLegacyOnly    IOMMUBackendPolicy = "LegacyOnly"
	IOMMUBackendPolicyPreferIommuFD IOMMUBackendPolicy = "PreferIommuFD"
)

// IOMMUBackendPolicy encodes the IOMMU backend selection policy as a string.
type IOMMUBackendPolicy string

// Validate ensures that IOMMUBackendPolicy has a valid set of values.
func (p IOMMUBackendPolicy) Validate() error {
	switch p {
	case IOMMUBackendPolicyLegacyOnly, IOMMUBackendPolicyPreferIommuFD:
		return nil
	default:
		return fmt.Errorf("unknown IOMMU backend policy: %v, supported IOMMU backend policies: %s, %s", p, IOMMUBackendPolicyLegacyOnly, IOMMUBackendPolicyPreferIommuFD)
	}
}

// IOMMUConfig holds the set of parameters for configuring IOMMU backend
// for the vfio devices.
type IOMMUConfig struct {
	// BackendPolicy represents the policy for selecting the IOMMU backend.
	// Supported values are:
	// - LegacyOnly: Only use legacy IOMMU backend.
	// - PreferIommuFD: Prefer IOMMUFD backend if present on the host.
	BackendPolicy IOMMUBackendPolicy `json:"backendPolicy"`
	// EnableAPIDevice represents whether to include the iommu API device.
	// If set to true, either `/dev/iommu` or `/dev/vfio/vfio` is included in the
	// claim CDI spec, depending on the selected iommu backend.
	// Defaults to true when unset; set to false to opt out (for example, for Kata).
	EnableAPIDevice *bool `json:"enableAPIDevice,omitempty"`
}

// ShouldPreferIommuFD returns true if the IOMMU backend policy is PreferIommuFD.
func (c *IOMMUConfig) ShouldPreferIommuFD() bool {
	return c.BackendPolicy == IOMMUBackendPolicyPreferIommuFD
}

// ShouldEnableAPIDevice returns true if the IOMMU API device should be made
// available to the workload.
func (c *IOMMUConfig) ShouldEnableAPIDevice() bool {
	if c.EnableAPIDevice != nil && *c.EnableAPIDevice {
		return true
	}
	return false
}

// Validate ensures that IOMMUConfig has a valid set of values.
func (c *IOMMUConfig) Validate() error {
	if err := c.BackendPolicy.Validate(); err != nil {
		return err
	}
	return nil
}
