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

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
)

const (
	// DefaultMaxChannels bounds the IMEX channel IDs [0, max) assigned by the
	// controller and accepted by kubelet plugins.
	DefaultMaxChannels = MaxChannels

	// MaxChannels is the number of IMEX channels the GPU driver supports.
	MaxChannels = 2048

	// ChannelDeviceName is the one channel device a kubelet plugin publishes
	// with channel reservations. Claims share it, and the controller picks
	// their channel through the claim config.
	ChannelDeviceName = "channel"

	// ChannelDeviceType is the type attribute of ChannelDeviceName. It differs
	// from the per-channel devices, so that DeviceClasses tell them apart.
	ChannelDeviceType = "imex-channel"

	// ChannelOwnerTaintKey taints ChannelDeviceName with the ComputeDomain
	// whose IMEX daemon the node runs, under per-ComputeDomain IMEX daemons.
	// Workload claims tolerate their own ComputeDomain, so the scheduler
	// skips nodes of other ComputeDomains.
	ChannelOwnerTaintKey = "compute-domain.nvidia.com/computeDomain"

	// ChannelCliqueAttribute names the NVLink clique of the node on
	// ChannelDeviceName. Channels are unique per clique.
	ChannelCliqueAttribute = "cliqueID"

	// ChannelAdmittedCondition and ChannelRejectedCondition are the binding
	// (failure) conditions of the channel device under per-ComputeDomain IMEX
	// daemons, set by the controller.
	ChannelAdmittedCondition = "compute-domain.nvidia.com/ChannelAdmitted"
	ChannelRejectedCondition = "compute-domain.nvidia.com/ChannelRejected"
)

// ChannelAllocation is the data of an allocated ChannelDeviceName in the
// ResourceClaim status. The controller sets it when it admits the claim, and
// the kubelet plugin injects its channel.
type ChannelAllocation struct {
	// Channel is the IMEX channel of the claim.
	Channel int `json:"channel"`
	// IMEXDomain is where the channel is unique: the NVLink clique of the
	// node, or the node itself if it has none.
	IMEXDomain string `json:"imexDomain"`
}

// Encode returns the allocation as device status data.
func (a ChannelAllocation) Encode() (*runtime.RawExtension, error) {
	raw, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("error encoding channel allocation: %w", err)
	}
	return &runtime.RawExtension{Raw: raw}, nil
}

// DecodeChannelAllocation returns the allocation of device status data, or
// nil if there is none.
func DecodeChannelAllocation(data *runtime.RawExtension) (*ChannelAllocation, error) {
	if data == nil || len(data.Raw) == 0 {
		return nil, nil
	}
	var a ChannelAllocation
	if err := json.Unmarshal(data.Raw, &a); err != nil {
		return nil, fmt.Errorf("error decoding channel allocation: %w", err)
	}
	if a.Channel < 0 || a.Channel >= MaxChannels {
		return nil, fmt.Errorf("channel %d out of range [0, %d)", a.Channel, MaxChannels)
	}
	return &a, nil
}

// ValidateMaxChannels validates the imex-max-channels flag.
func ValidateMaxChannels(maxChannels int) error {
	if maxChannels < 1 || maxChannels > MaxChannels {
		return fmt.Errorf("imex-max-channels must be between 1 and %d, got %d", MaxChannels, maxChannels)
	}
	return nil
}
