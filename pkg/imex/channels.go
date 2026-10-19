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

package imex

import "fmt"

const (
	// DefaultMaxChannels bounds the IMEX channel IDs [0, max) published by
	// kubelet plugins and reserved by the controller.
	DefaultMaxChannels = 128

	// MaxChannels is the number of IMEX channels the GPU driver supports.
	MaxChannels = 2048

	// ChannelOwnerTaintKey is the device taint key whose value is the UID of
	// the ComputeDomain allowed to use the device.
	ChannelOwnerTaintKey = "compute-domain.nvidia.com/owner"

	// ChannelAdmittedCondition and ChannelRejectedCondition are the binding
	// (failure) conditions of channel devices, set by the controller.
	ChannelAdmittedCondition = "compute-domain.nvidia.com/ChannelAdmitted"
	ChannelRejectedCondition = "compute-domain.nvidia.com/ChannelRejected"
)

// ValidateMaxChannels validates the imex-max-channels flag.
func ValidateMaxChannels(maxChannels int) error {
	if maxChannels < 1 || maxChannels > MaxChannels {
		return fmt.Errorf("imex-max-channels must be between 1 and %d, got %d", MaxChannels, maxChannels)
	}
	return nil
}
