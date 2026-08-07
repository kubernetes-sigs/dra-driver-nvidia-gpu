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
		Channel: d.Channel.DeepCopy(),
		Daemon:  d.Daemon.DeepCopy(),
	}
}

func (c *PreparedComputeDomainChannel) DeepCopy() *PreparedComputeDomainChannel {
	if c == nil {
		return nil
	}
	return &PreparedComputeDomainChannel{
		Info:   c.Info.DeepCopy(),
		Device: deepCopyDevice(c.Device),
	}
}

func (d *PreparedComputeDomainDaemon) DeepCopy() *PreparedComputeDomainDaemon {
	if d == nil {
		return nil
	}
	return &PreparedComputeDomainDaemon{
		Info:   d.Info.DeepCopy(),
		Device: deepCopyDevice(d.Device),
	}
}

func (d *ComputeDomainChannelInfo) DeepCopy() *ComputeDomainChannelInfo {
	if d == nil {
		return nil
	}
	return &ComputeDomainChannelInfo{ID: d.ID}
}

func (d *ComputeDomainDaemonInfo) DeepCopy() *ComputeDomainDaemonInfo {
	if d == nil {
		return nil
	}
	return &ComputeDomainDaemonInfo{ID: d.ID}
}

func (d DeviceConfigState) DeepCopy() DeviceConfigState {
	return DeviceConfigState{
		Type:          d.Type,
		ComputeDomain: d.ComputeDomain,
	}
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
