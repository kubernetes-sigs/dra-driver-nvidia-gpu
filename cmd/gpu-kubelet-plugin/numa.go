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
	"os"
	"strings"

	"k8s.io/dynamic-resource-allocation/deviceattribute"
	"k8s.io/klog/v2"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

const (
	defaultSysfsRoot = "/sys"
	sysfsRootEnvvar  = "NVIDIA_DRA_SYSFS_ROOT"
)

func configuredNUMAAttributeForm() deviceattribute.AttributeForm {
	if featuregates.Enabled(featuregates.DRAListTypeAttributes) {
		return deviceattribute.ListAttribute
	}
	return deviceattribute.ScalarAttribute
}

func discoverNUMANodeAttribute(pciBusID string) *deviceattribute.DeviceAttribute {
	attr, err := getNUMANodeAttributeByPCIBusID(pciBusID, configuredNUMAAttributeForm(), configuredSysfsRoot())
	if err != nil {
		if strings.Contains(err.Error(), "no NUMA affinity") {
			klog.V(4).Infof("NUMA node unavailable for PCI bus ID %s, continuing without attribute: %v", pciBusID, err)
		} else {
			klog.Warningf("error getting NUMA node for PCI bus ID %s, continuing without attribute: %v", pciBusID, err)
		}
		return nil
	}
	return &attr
}

func configuredSysfsRoot() string {
	if root := os.Getenv(sysfsRootEnvvar); root != "" {
		return root
	}
	return defaultSysfsRoot
}

func getNUMANodeAttributeByPCIBusID(pciBusID string, attrForm deviceattribute.AttributeForm, sysfsRoot string) (deviceattribute.DeviceAttribute, error) {
	return deviceattribute.GetNUMANodeAttributeByPCIBusID(pciBusID, attrForm, deviceattribute.WithFSFromRoot(sysfsRoot))
}
