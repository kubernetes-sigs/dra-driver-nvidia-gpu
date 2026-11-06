// Copyright The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package framework

import resourceapi "k8s.io/api/resource/v1"

func DRADeviceAttributeString(device resourceapi.Device, name resourceapi.QualifiedName) string {
	if attr := device.Attributes[name]; attr.StringValue != nil {
		return *attr.StringValue
	}
	return ""
}
