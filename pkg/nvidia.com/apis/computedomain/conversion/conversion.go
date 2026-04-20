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

// Package conversion implements conversions between ComputeDomain API versions.
package conversion

import (
	v1beta1 "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	v1beta2 "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta2"
)

// V1beta1ToV1beta2 converts a v1beta1 ComputeDomain to v1beta2 (storage version).
func V1beta1ToV1beta2(in *v1beta1.ComputeDomain) (*v1beta2.ComputeDomain, error) {
	if in == nil {
		return nil, nil
	}
	out := &v1beta2.ComputeDomain{}
	if err := in.ConvertTo(out); err != nil {
		return nil, err
	}
	return out, nil
}

// V1beta2ToV1beta1 converts a v1beta2 ComputeDomain to v1beta1 for clients
// requesting the deprecated served version.
func V1beta2ToV1beta1(in *v1beta2.ComputeDomain) (*v1beta1.ComputeDomain, error) {
	if in == nil {
		return nil, nil
	}
	out := &v1beta1.ComputeDomain{}
	if err := out.ConvertFrom(in); err != nil {
		return nil, err
	}
	return out, nil
}
