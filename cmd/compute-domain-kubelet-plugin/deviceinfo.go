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

package main

import (
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"
)

type ComputeDomainChannelInfo struct {
	ID int `json:"id"`
}

type ComputeDomainDaemonInfo struct {
	ID int `json:"id"`
}

func (d *ComputeDomainChannelInfo) CanonicalName() string {
	return fmt.Sprintf("%s-%d", ComputeDomainChannelType, d.ID)
}

func (d *ComputeDomainDaemonInfo) CanonicalName() string {
	// There is only ever one daemon device per node (and, with
	// SharedDaemonResourceClaim, only one cluster-wide via the static
	// ResourceSlice). The static slice's device name must match this string.
	return ComputeDomainDaemonType
}

func (d *ComputeDomainChannelInfo) CanonicalIndex() string {
	return fmt.Sprintf("%d", d.ID)
}

func (d *ComputeDomainDaemonInfo) CanonicalIndex() string {
	return fmt.Sprintf("%d", d.ID)
}

func (d *ComputeDomainChannelInfo) GetDevice() resourceapi.Device {
	device := resourceapi.Device{
		Name: d.CanonicalName(),
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"type": {
				StringValue: ptr.To(ComputeDomainChannelType),
			},
			"id": {
				IntValue: ptr.To(int64(d.ID)),
			},
		},
	}
	return device
}

func (d *ComputeDomainDaemonInfo) GetDevice() resourceapi.Device {
	device := resourceapi.Device{
		Name: d.CanonicalName(),
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"type": {
				StringValue: ptr.To(ComputeDomainDaemonType),
			},
			"id": {
				IntValue: ptr.To(int64(d.ID)),
			},
		},
	}
	return device
}
