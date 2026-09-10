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
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

func (d PreparedDevices) DeepCopy() PreparedDevices {
	if d == nil {
		return nil
	}
	out := make(PreparedDevices, len(d))
	for i, group := range d {
		out[i] = group.DeepCopy()
	}
	return out
}

func (g *PreparedDeviceGroup) DeepCopy() *PreparedDeviceGroup {
	if g == nil {
		return nil
	}
	return &PreparedDeviceGroup{
		Devices:     g.Devices.DeepCopy(),
		ConfigState: g.ConfigState.DeepCopy(),
	}
}

func (l PreparedDeviceList) DeepCopy() PreparedDeviceList {
	if l == nil {
		return nil
	}
	out := make(PreparedDeviceList, len(l))
	for i, d := range l {
		out[i] = d.DeepCopy()
	}
	return out
}

func (d PreparedDevice) DeepCopy() PreparedDevice {
	return PreparedDevice{
		Gpu:  d.Gpu.DeepCopy(),
		Mig:  d.Mig.DeepCopy(),
		Vfio: d.Vfio.DeepCopy(),
	}
}

func (g *PreparedGpu) DeepCopy() *PreparedGpu {
	if g == nil {
		return nil
	}
	return &PreparedGpu{
		Info:   g.Info.DeepCopy(),
		Device: deepCopyDevice(g.Device),
	}
}

func (m *PreparedMigDevice) DeepCopy() *PreparedMigDevice {
	if m == nil {
		return nil
	}
	return &PreparedMigDevice{
		Concrete: m.Concrete.DeepCopy(),
		Device:   deepCopyDevice(m.Device),
	}
}

func (v *PreparedVfioDevice) DeepCopy() *PreparedVfioDevice {
	if v == nil {
		return nil
	}
	return &PreparedVfioDevice{
		Info:   v.Info.DeepCopy(),
		Device: deepCopyDevice(v.Device),
	}
}

// DeepCopy for device info types. Only JSON-serialised fields are copied since
// unexported fields are not persisted in the checkpoint.

func (g *GpuInfo) DeepCopy() *GpuInfo {
	if g == nil {
		return nil
	}
	return &GpuInfo{UUID: g.UUID}
}

func (m *MigLiveTuple) DeepCopy() *MigLiveTuple {
	if m == nil {
		return nil
	}
	cp := *m
	return &cp
}

func (v *VfioDeviceInfo) DeepCopy() *VfioDeviceInfo {
	if v == nil {
		return nil
	}
	return &VfioDeviceInfo{UUID: v.UUID, PciBusID: v.PciBusID}
}

func (d DeviceConfigState) DeepCopy() DeviceConfigState {
	return DeviceConfigState{MpsControlDaemonID: d.MpsControlDaemonID}
}

func deepCopyDevice(d *kubeletplugin.Device) *kubeletplugin.Device {
	if d == nil {
		return nil
	}
	cp := &kubeletplugin.Device{
		PoolName:   d.PoolName,
		DeviceName: d.DeviceName,
	}
	if len(d.Requests) > 0 {
		cp.Requests = make([]string, len(d.Requests))
		copy(cp.Requests, d.Requests)
	}
	if len(d.CDIDeviceIDs) > 0 {
		cp.CDIDeviceIDs = make([]string, len(d.CDIDeviceIDs))
		copy(cp.CDIDeviceIDs, d.CDIDeviceIDs)
	}
	if d.ShareID != nil {
		uid := *d.ShareID
		cp.ShareID = &uid
	}
	return cp
}
