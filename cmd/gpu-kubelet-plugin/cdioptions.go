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
	nvdevice "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/sirupsen/logrus"
)

// cdiOption represents a functional option for constructing a CDI handler.
type cdiOption func(*CDIHandler)

// WithDriverRoot provides an cdiOption to set the driver root used by the 'cdi' interface.
func WithDriverRoot(root string) cdiOption {
	return func(c *CDIHandler) {
		c.driverRoot = root
	}
}

// WithDevRoot provides a cdiOption to set the device root used by the 'cdi' interface.
func WithDevRoot(root string) cdiOption {
	return func(c *CDIHandler) {
		c.devRoot = root
	}
}

// WithTargetDriverRoot provides an cdiOption to set the target driver root used by the 'cdi' interface.
func WithTargetDriverRoot(root string) cdiOption {
	return func(c *CDIHandler) {
		c.targetDriverRoot = root
	}
}

// WithCDIRoot provides an cdiOption to set the CDI root used by the 'cdi' interface.
func WithCDIRoot(cdiRoot string) cdiOption {
	return func(c *CDIHandler) {
		c.cdiRoot = cdiRoot
	}
}

// WithNVIDIACDIHookPath provides an cdiOption to set the nvidia-cdi-hook path used by the 'cdi' interface.
func WithNVIDIACDIHookPath(path string) cdiOption {
	return func(c *CDIHandler) {
		c.nvidiaCDIHookPath = path
	}
}

// WithNvml provides an cdiOption to set the NVML library used by the 'cdi' interface.
func WithNvml(nvml nvml.Interface) cdiOption {
	return func(c *CDIHandler) {
		c.nvml = nvml
	}
}

// WithDeviceLib provides and Optin to set the device enumeration and query library.
func WithDeviceLib(nvdevice nvdevice.Interface) cdiOption {
	return func(c *CDIHandler) {
		c.nvdevice = nvdevice
	}
}

// WithVendor provides an cdiOption to set the logger used by the 'cdi' interface.
func WithLogger(logger *logrus.Logger) cdiOption {
	return func(c *CDIHandler) {
		c.logger = logger
	}
}

// WithVfioCDIHandler provides an cdiOption to set the vfio CDI handler used by the 'cdi' interface.
func WithVfioCDIHandler(vfioCDIHandler *vfioCDIHandler) cdiOption {
	return func(c *CDIHandler) {
		c.vfiocdi = vfioCDIHandler
	}
}

// WithVgpuCDIHandler provides a cdiOption to set the vgpu CDI handler used by the 'cdi' interface.
func WithVgpuCDIHandler(vgpuCDIHandler *vgpuCDIHandler) cdiOption {
	return func(c *CDIHandler) {
		c.vgpucdi = vgpuCDIHandler
	}
}
