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
	IOMMUBackendPolicyPreferIommuFD IOMMUBackendPolicy = "PreferIommuFD"
	IOMMUBackendPolicyLegacyOnly    IOMMUBackendPolicy = "LegacyOnly"
)

// IOMMUBackendPolicy encodes the IOMMU backend selection policy as a string.
type IOMMUBackendPolicy string

// Validate ensures that IOMMUBackendPolicy has a valid set of values.
func (p IOMMUBackendPolicy) Validate() error {
	switch p {
	case IOMMUBackendPolicyPreferIommuFD, IOMMUBackendPolicyLegacyOnly:
		return nil
	default:
		return fmt.Errorf("unknown IOMMU backend policy: %q", p)
	}
}

// IOMMUConfig holds the set of parameters for configuring IOMMU backend
// for the vfio devices.
type IOMMUConfig struct {
	// BackendPolicy represents the policy for selecting the IOMMU backend.
	// Supported values are:
	// - PreferIommuFD: Prefer IOMMUFD backend if present on the host.
	// - LegacyOnly: Only use legacy IOMMU backend.
	BackendPolicy IOMMUBackendPolicy `json:"backendPolicy"`
	// EnableAPIDevice represents whether to include the iommu API device.
	// If set to true, either `/dev/iommu` or `/dev/vfio/vfio` is included in the
	// claim CDI spec, depending on the selected iommu backend.
	EnableAPIDevice *bool `json:"enableAPIDevice,omitempty"`
}

// Validate ensures that IOMMUConfig has a valid set of values.
func (c *IOMMUConfig) Validate() error {
	if err := c.BackendPolicy.Validate(); err != nil {
		return err
	}
	return nil
}
