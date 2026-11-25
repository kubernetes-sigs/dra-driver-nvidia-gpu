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

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/stretchr/testify/require"
)

// fakeMigDevice implements the subset of nvml.Device used by
// getMigDeviceUUID(). Calling any other method panics (nil embedded interface).
type fakeMigDevice struct {
	nvml.Device
	uuid    string
	giID    int
	ciID    int
	giIDRet nvml.Return
	ciIDRet nvml.Return
	uuidRet nvml.Return
}

func (d *fakeMigDevice) GetUUID() (string, nvml.Return) {
	return d.uuid, d.uuidRet
}

func (d *fakeMigDevice) GetGpuInstanceId() (int, nvml.Return) {
	return d.giID, d.giIDRet
}

func (d *fakeMigDevice) GetComputeInstanceId() (int, nvml.Return) {
	return d.ciID, d.ciIDRet
}

func (d *fakeMigDevice) GetMaxMigDeviceCount() (int, nvml.Return) {
	return 0, nvml.ERROR_NOT_SUPPORTED
}

func (d *fakeMigDevice) GetMigDeviceHandleByIndex(int) (nvml.Device, nvml.Return) {
	return nil, nvml.ERROR_NOT_SUPPORTED
}

// fakeParentDevice models a MIG-enabled parent GPU. A nil entry in `migs`
// models an empty MIG device slot (ERROR_NOT_FOUND unless overridden in
// handleRetByIndex).
type fakeParentDevice struct {
	nvml.Device
	uuid             string
	migs             []*fakeMigDevice
	maxCountRet      nvml.Return
	handleRetByIndex map[int]nvml.Return
}

func (d *fakeParentDevice) GetUUID() (string, nvml.Return) { return d.uuid, nvml.SUCCESS }

func (d *fakeParentDevice) GetMaxMigDeviceCount() (int, nvml.Return) {
	if d.maxCountRet != nvml.SUCCESS {
		return 0, d.maxCountRet
	}
	return len(d.migs), nvml.SUCCESS
}

func (d *fakeParentDevice) GetMigDeviceHandleByIndex(i int) (nvml.Device, nvml.Return) {
	if ret, ok := d.handleRetByIndex[i]; ok {
		return nil, ret
	}
	if i < 0 || i >= len(d.migs) || d.migs[i] == nil {
		return nil, nvml.ERROR_NOT_FOUND
	}
	return d.migs[i], nvml.SUCCESS
}

func TestGetMigDeviceUUID(t *testing.T) {
	parent := &fakeParentDevice{
		uuid: "GPU-parent",
		migs: []*fakeMigDevice{
			{uuid: "MIG-aaaa", giID: 7, ciID: 0},
			nil, // empty slot (ERROR_NOT_FOUND)
			nil, // slot with non-ERROR_NOT_FOUND error
			{uuid: "MIG-bad-gi", giID: 8, ciID: 0, giIDRet: nvml.ERROR_UNKNOWN},
			{uuid: "MIG-bad-ci", giID: 8, ciID: 0, ciIDRet: nvml.ERROR_UNKNOWN},
			{uuid: "MIG-bbbb", giID: 8, ciID: 0},
			{uuid: "MIG-cccc", giID: 8, ciID: 1},
			{uuid: "MIG-bad-uuid", giID: 8, ciID: 2, uuidRet: nvml.ERROR_UNKNOWN},
		},
		maxCountRet: nvml.SUCCESS,
		handleRetByIndex: map[int]nvml.Return{
			2: nvml.ERROR_INVALID_ARGUMENT,
		},
	}

	uuid, err := getMigDeviceUUID(parent, 8, 0)
	require.NoError(t, err)
	require.Equal(t, "MIG-bbbb", uuid)

	uuid, err = getMigDeviceUUID(parent, 8, 1)
	require.NoError(t, err)
	require.Equal(t, "MIG-cccc", uuid)

	// The result must never be the parent GPU UUID.
	uuid, err = getMigDeviceUUID(parent, 7, 0)
	require.NoError(t, err)
	require.NotEqual(t, parent.uuid, uuid)

	// GetUUID failure on a matching (GI, CI) handle surfaces an error.
	_, err = getMigDeviceUUID(parent, 8, 2)
	require.ErrorContains(t, err, "error getting UUID of MIG device at index 7")

	_, err = getMigDeviceUUID(parent, 9, 0)
	require.Error(t, err)

	parent.maxCountRet = nvml.ERROR_UNKNOWN
	_, err = getMigDeviceUUID(parent, 7, 0)
	require.Error(t, err)
}
