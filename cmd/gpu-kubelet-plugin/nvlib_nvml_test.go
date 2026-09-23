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

package main

import (
	"context"
	"testing"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

type fakeNVMLDeviceLib struct {
	nvdev.Interface
	device nvdev.Device
}

func (l fakeNVMLDeviceLib) NewDevice(nvml.Device) (nvdev.Device, error) {
	return l.device, nil
}

type fakeNVDevice struct {
	nvdev.Device
	migEnabled bool
}

func (d fakeNVDevice) IsMigEnabled() (bool, error) {
	return d.migEnabled, nil
}

type fakeNVMLGPU struct {
	nvml.Device
	migDevice       nvml.Device
	gi              nvml.GpuInstance
	giProfile       nvml.GpuInstanceProfileInfo
	setMigModeCalls []int
	events          *[]string
}

func (*fakeNVMLGPU) GetMaxMigDeviceCount() (int, nvml.Return) {
	return 1, nvml.SUCCESS
}

func (d *fakeNVMLGPU) GetMigDeviceHandleByIndex(int) (nvml.Device, nvml.Return) {
	return d.migDevice, nvml.SUCCESS
}

func (d *fakeNVMLGPU) GetGpuInstanceById(int) (nvml.GpuInstance, nvml.Return) {
	return d.gi, nvml.SUCCESS
}

func (d *fakeNVMLGPU) GetGpuInstanceProfileInfo(int) (nvml.GpuInstanceProfileInfo, nvml.Return) {
	return d.giProfile, nvml.SUCCESS
}

func (d *fakeNVMLGPU) CreateGpuInstanceWithPlacement(*nvml.GpuInstanceProfileInfo, *nvml.GpuInstancePlacement) (nvml.GpuInstance, nvml.Return) {
	return d.gi, nvml.SUCCESS
}

func (d *fakeNVMLGPU) GetArchitecture() (nvml.DeviceArchitecture, nvml.Return) {
	return nvml.DEVICE_ARCH_HOPPER, nvml.SUCCESS
}

func (d *fakeNVMLGPU) SetMigMode(mode int) (nvml.Return, nvml.Return) {
	d.setMigModeCalls = append(d.setMigModeCalls, mode)
	if d.events != nil {
		*d.events = append(*d.events, "set-mig-mode")
	}
	return nvml.SUCCESS, nvml.SUCCESS
}

type fakeNVMLGpuInstance struct {
	nvml.GpuInstance
	info                     nvml.GpuInstanceInfo
	ci                       nvml.ComputeInstance
	ciProfile                nvml.ComputeInstanceProfileInfo
	createComputeInstanceRet nvml.Return
	getComputeInstanceRet    nvml.Return
	events                   *[]string
}

func (g *fakeNVMLGpuInstance) GetInfo() (nvml.GpuInstanceInfo, nvml.Return) {
	return g.info, nvml.SUCCESS
}

func (g *fakeNVMLGpuInstance) GetComputeInstanceById(int) (nvml.ComputeInstance, nvml.Return) {
	return g.ci, g.getComputeInstanceRet
}

func (g *fakeNVMLGpuInstance) GetComputeInstanceProfileInfo(int, int) (nvml.ComputeInstanceProfileInfo, nvml.Return) {
	return g.ciProfile, nvml.SUCCESS
}

func (g *fakeNVMLGpuInstance) CreateComputeInstance(*nvml.ComputeInstanceProfileInfo) (nvml.ComputeInstance, nvml.Return) {
	return g.ci, g.createComputeInstanceRet
}

func (g *fakeNVMLGpuInstance) Destroy() nvml.Return {
	if g.events != nil {
		*g.events = append(*g.events, "destroy-gi")
	}
	return nvml.SUCCESS
}

type fakeNVMLComputeInstance struct {
	nvml.ComputeInstance
	info   nvml.ComputeInstanceInfo
	events *[]string
}

func (c *fakeNVMLComputeInstance) GetInfo() (nvml.ComputeInstanceInfo, nvml.Return) {
	return c.info, nvml.SUCCESS
}

func (c *fakeNVMLComputeInstance) Destroy() nvml.Return {
	if c.events != nil {
		*c.events = append(*c.events, "destroy-ci")
	}
	return nvml.SUCCESS
}

type fakeNVMLMigDevice struct {
	nvml.Device
	giID                    int
	ciID                    int
	uuid                    string
	getComputeInstanceIDRet nvml.Return
	getUUIDRet              nvml.Return
}

func (d *fakeNVMLMigDevice) GetGpuInstanceId() (int, nvml.Return) {
	return d.giID, nvml.SUCCESS
}

func (d *fakeNVMLMigDevice) GetComputeInstanceId() (int, nvml.Return) {
	return d.ciID, d.getComputeInstanceIDRet
}

func (d *fakeNVMLMigDevice) GetUUID() (string, nvml.Return) {
	return d.uuid, d.getUUIDRet
}

func enableDynamicMIGForTest(t *testing.T) {
	t.Helper()
	previous := featuregates.Enabled(featuregates.DynamicMIG)
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.DynamicMIG): true,
	}))
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
			string(featuregates.DynamicMIG): previous,
		}))
	})
}

func testMigProfile() nvdev.MigProfile {
	return &nvdev.MigProfileInfo{
		C:              1,
		G:              1,
		GB:             5,
		GIProfileID:    0,
		CIProfileID:    0,
		CIEngProfileID: 0,
	}
}

func TestDeviceLibGetMigDevices(t *testing.T) {
	enableDynamicMIGForTest(t)

	migDevice := &fakeNVMLMigDevice{giID: 3, ciID: 4, uuid: "MIG-1"}
	ci := &fakeNVMLComputeInstance{info: nvml.ComputeInstanceInfo{Id: 4, ProfileId: 7}}
	gi := &fakeNVMLGpuInstance{
		info:      nvml.GpuInstanceInfo{Id: 3, ProfileId: 19, Placement: nvml.GpuInstancePlacement{Start: 2, Size: 1}},
		ci:        ci,
		ciProfile: nvml.ComputeInstanceProfileInfo{Id: 7},
	}
	gpu := &fakeNVMLGPU{
		migDevice: migDevice,
		gi:        gi,
		giProfile: nvml.GpuInstanceProfileInfo{Id: 19},
	}
	gpuInfo := &GpuInfo{
		UUID:        "GPU-1",
		minor:       1,
		migEnabled:  true,
		migProfiles: []*MigProfileInfo{{profile: testMigProfile()}},
	}
	l := deviceLib{devhandleByUUID: map[string]nvml.Device{gpuInfo.UUID: gpu}}

	got, err := l.getMigDevices(gpuInfo)
	require.NoError(t, err)
	require.Equal(t, &MigDeviceInfo{
		UUID:           "MIG-1",
		Profile:        "1g.5gb",
		ParentMinor:    1,
		ParentUUID:     "GPU-1",
		CIID:           4,
		GIID:           3,
		PlacementStart: 2,
		PlacementSize:  1,
		GiProfileID:    19,
	}, withoutMigDeviceRuntimeFields(got["MIG-1"]))
}

func TestDeviceLibCreateMigDevice(t *testing.T) {
	enableDynamicMIGForTest(t)

	migDevice := &fakeNVMLMigDevice{uuid: "MIG-1"}
	ci := &fakeNVMLComputeInstance{info: nvml.ComputeInstanceInfo{Id: 4, ProfileId: 7, Device: migDevice}}
	gi := &fakeNVMLGpuInstance{
		info:      nvml.GpuInstanceInfo{Id: 3},
		ci:        ci,
		ciProfile: nvml.ComputeInstanceProfileInfo{Id: 7},
	}
	gpu := &fakeNVMLGPU{gi: gi, giProfile: nvml.GpuInstanceProfileInfo{Id: 19}}
	parent := &GpuInfo{UUID: "GPU-1", minor: 1}
	l := deviceLib{
		Interface:       fakeNVMLDeviceLib{device: fakeNVDevice{}},
		devhandleByUUID: map[string]nvml.Device{parent.UUID: gpu},
	}

	got, err := l.createMigDevice(&MigSpec{
		Parent:    parent,
		Profile:   testMigProfile(),
		Placement: nvml.GpuInstancePlacement{Start: 2, Size: 1},
	})
	require.NoError(t, err)
	require.Equal(t, []int{nvml.DEVICE_MIG_ENABLE}, gpu.setMigModeCalls)
	require.Equal(t, &MigDeviceInfo{
		UUID:           "MIG-1",
		CIID:           4,
		GIID:           3,
		ParentMinor:    1,
		ParentUUID:     "GPU-1",
		Profile:        "1g.5gb",
		PlacementStart: 2,
		PlacementSize:  1,
		GiProfileID:    19,
	}, withoutMigDeviceRuntimeFields(got))
}

func TestDeviceStateRollbackAfterComputeInstanceCreationFailure(t *testing.T) {
	enableDynamicMIGForTest(t)

	var events []string
	migDevice := &fakeNVMLMigDevice{
		giID:                    3,
		getComputeInstanceIDRet: nvml.ERROR_NOT_FOUND,
		getUUIDRet:              nvml.ERROR_NOT_FOUND,
	}
	gi := &fakeNVMLGpuInstance{
		info:                     nvml.GpuInstanceInfo{Id: 3, ProfileId: 19, Placement: nvml.GpuInstancePlacement{Start: 2, Size: 1}},
		ciProfile:                nvml.ComputeInstanceProfileInfo{Id: 7},
		createComputeInstanceRet: nvml.ERROR_UNKNOWN,
		getComputeInstanceRet:    nvml.ERROR_NOT_FOUND,
		events:                   &events,
	}
	gpu := &fakeNVMLGPU{
		migDevice: migDevice,
		gi:        gi,
		giProfile: nvml.GpuInstanceProfileInfo{Id: 19},
		events:    &events,
	}
	parent := &GpuInfo{UUID: "GPU-1", minor: 1, pciBusID: "0000:01:00.0"}
	nvmllib := &mockNVMLLibrary{
		deviceGetHandleByUUIDFunc: func(uuid string) (nvml.Device, nvml.Return) {
			if uuid != parent.UUID {
				return nil, nvml.ERROR_NOT_FOUND
			}
			return gpu, nvml.SUCCESS
		},
	}
	l := &deviceLib{
		Interface:         fakeNVMLDeviceLib{device: fakeNVDevice{migEnabled: true}},
		nvmllib:           nvmllib,
		gpuInfosByUUID:    map[string]*GpuInfo{parent.UUID: parent},
		gpuUUIDbyPCIBusID: map[PCIBusID]string{parent.pciBusID: parent.UUID},
		devhandleByUUID:   make(map[string]nvml.Device),
	}
	migSpec := &MigSpec{
		Parent:        parent,
		Profile:       testMigProfile(),
		GIProfileInfo: nvml.GpuInstanceProfileInfo{Id: 19},
		Placement:     nvml.GpuInstancePlacement{Start: 2, Size: 1},
	}

	_, err := l.createMigDevice(migSpec)
	require.ErrorContains(t, err, "error creating Compute instance")

	deviceName := migSpec.CanonicalName()
	parsedSpec, err := NewMigSpecTupleFromCanonicalName(deviceName)
	require.NoError(t, err)
	require.Empty(t, parsedSpec.ParentPCIBusID)

	preparedClaim := PreparedClaim{
		CheckpointState: ClaimCheckpointStatePrepareStarted,
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{Results: []resourceapi.DeviceRequestAllocationResult{
					{Driver: DriverName, Device: deviceName},
				}},
			},
		},
	}
	checkpoint := &Checkpoint{V2: &CheckpointV2{PreparedClaims: PreparedClaimsByUID{
		"claim-uid": preparedClaim,
	}}}
	state := &DeviceState{
		nvdevlib: l,
		perGPUAllocatable: &PerGPUAllocatableDevices{allocatablesMap: map[PCIBusID]AllocatableDevices{
			parent.pciBusID: {
				deviceName: {MigDynamic: migSpec},
			},
		}},
	}

	err = state.rollbackPartiallyPreparedClaim(context.Background(), "claim-uid", preparedClaim, checkpoint)
	require.NoError(t, err)
	require.Equal(t, []string{"destroy-gi", "set-mig-mode"}, events)
	require.Equal(t, 1, nvmllib.deviceGetHandleByUUIDCalls)
}

func TestDeviceLibDeleteMigDevice(t *testing.T) {
	enableDynamicMIGForTest(t)

	var events []string
	ci := &fakeNVMLComputeInstance{events: &events}
	gi := &fakeNVMLGpuInstance{ci: ci, events: &events}
	gpu := &fakeNVMLGPU{gi: gi, events: &events}
	parent := &GpuInfo{UUID: "GPU-1", minor: 1}
	l := deviceLib{
		devhandleByUUID: map[string]nvml.Device{parent.UUID: gpu},
		gpuInfosByUUID:  map[string]*GpuInfo{parent.UUID: parent},
	}

	err := l.deleteMigDevice(&MigLiveTuple{ParentUUID: parent.UUID, GIID: 3, CIID: 4})
	require.NoError(t, err)
	require.Equal(t, []string{"destroy-ci", "destroy-gi", "set-mig-mode"}, events)
	require.Equal(t, []int{nvml.DEVICE_MIG_DISABLE}, gpu.setMigModeCalls)
}

func withoutMigDeviceRuntimeFields(info *MigDeviceInfo) *MigDeviceInfo {
	if info == nil {
		return nil
	}
	copy := *info
	copy.parent = nil
	copy.giProfileInfo = nil
	copy.gIInfo = nil
	copy.ciProfileInfo = nil
	copy.cIInfo = nil
	return &copy
}
