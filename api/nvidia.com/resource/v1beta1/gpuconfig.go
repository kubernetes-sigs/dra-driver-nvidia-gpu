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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// GpuConfig holds the set of parameters for configuring a GPU.
type GpuConfig struct {
	metav1.TypeMeta `json:",inline"`
	Sharing         *GpuSharing `json:"sharing,omitempty"`
}

// DefaultGpuConfig provides the default GPU configuration.
func DefaultGpuConfig() *GpuConfig {
	return &GpuConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: GroupName + "/" + Version,
			Kind:       GpuConfigKind,
		},
	}
}

// Normalize updates a GpuConfig config with implied default values based on other settings.
func (c *GpuConfig) Normalize() error {
	if c.Sharing == nil {
		return nil
	}

	if featuregates.Enabled(featuregates.TimeSlicingSettings) {
		if c.Sharing.Strategy == TimeSlicingStrategy && c.Sharing.TimeSlicingConfig == nil {
			c.Sharing.TimeSlicingConfig = &TimeSlicingConfig{
				Interval: ptr.To(DefaultTimeSlice),
			}
		}
	}

	if featuregates.Enabled(featuregates.MPSSupport) {
		if c.Sharing.Strategy == MpsStrategy && c.Sharing.MpsConfig == nil {
			c.Sharing.MpsConfig = &MpsConfig{}
		}
	}

	return nil
}

// Validate ensures that GpuConfig has a valid set of values.
func (c *GpuConfig) Validate() error {
	if c.Sharing == nil {
		return nil
	}
	return c.Sharing.Validate()
}
