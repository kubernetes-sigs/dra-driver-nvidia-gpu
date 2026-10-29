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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
)

// The kubelet resolves the CDI device IDs returned by Prepare against the
// claim spec, so both must name the shared channel device alike, although
// its prepared Info records the channel of the claim.
func TestClaimSpecResolvesSharedChannelDevice(t *testing.T) {
	cache, err := cdiapi.NewCache(cdiapi.WithSpecDirs(t.TempDir()), cdiapi.WithAutoRefresh(false))
	require.NoError(t, err)
	h := &CDIHandler{cache: cache, vendor: cdiVendor, claimClass: cdiClaimClass}

	edits := &cdiapi.ContainerEdits{ContainerEdits: &cdispec.ContainerEdits{Env: []string{"A=1"}}}
	allocatable := &AllocatableDevice{Channel: &ComputeDomainChannelInfo{ID: sharedChannelID}}
	id := h.GetClaimDevice("uid", allocatable, edits)
	require.NotEmpty(t, id)

	prepared := PreparedDevices{{
		ConfigState: DeviceConfigState{containerEdits: edits, channelID: 5},
		Devices: PreparedDeviceList{{Channel: &PreparedComputeDomainChannel{
			Info:   &ComputeDomainChannelInfo{ID: 5},
			Device: &CheckpointedDevice{DeviceName: imex.ChannelDeviceName, CDIDeviceIDs: []string{id}},
		}}},
	}}
	require.NoError(t, h.CreateClaimSpecFile("uid", prepared))
	require.NoError(t, cache.Refresh())

	assert.NotNil(t, cache.GetDevice(id), "CDI device %s must resolve", id)
}
