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

package v1beta1

import (
	"errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// GpuDriverConfig is the node-local startup configuration of the GPU kubelet
// plugin. It is not valid as opaque device configuration in a ResourceClaim.
type GpuDriverConfig struct {
	metav1.TypeMeta `json:",inline"`
	Advertisement   GpuAdvertisement `json:"advertisement"`
}

// GpuAdvertisement selects which discovered device types are published.
// Omitted types are not advertised; Mig covers static and dynamic MIG.
type GpuAdvertisement struct {
	Gpu  bool `json:"gpu,omitempty"`
	Mig  bool `json:"mig,omitempty"`
	Vfio bool `json:"vfio,omitempty"`
}

// Normalize is a no-op: every field defaults to its zero value.
func (c *GpuDriverConfig) Normalize() error {
	return nil
}

// Validate requires at least one advertised device type.
func (c *GpuDriverConfig) Validate() error {
	a := c.Advertisement
	if !a.Gpu && !a.Mig && !a.Vfio {
		return errors.New("advertisement must enable at least one device type")
	}
	return nil
}
