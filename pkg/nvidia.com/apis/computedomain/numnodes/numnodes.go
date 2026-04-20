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

// Package numnodes reads the expected node count from the stored (hub) ComputeDomain.
package numnodes

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1beta1 "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	v1beta2 "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta2"
)

// V1beta2Getter can perform a Get on ComputeDomain (e.g. namespaced v1beta2 client).
type V1beta2Getter interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*v1beta2.ComputeDomain, error)
}

// FromStorage returns the configured expected node count from the hub object.
// The value originates from deprecated v1beta1 spec.numNodes and is stored on the hub
// as metadata.annotations["resource.nvidia.com/computedomain-num-nodes"].
func FromStorage(ctx context.Context, client V1beta2Getter, name string) (int, error) {
	cd, err := client.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, err
	}
	return FromObject(&cd.ObjectMeta), nil
}

// FromObject returns the expected node count from hub metadata: the annotation
// resource.nvidia.com/computedomain-num-nodes (v1beta1 spec.numNodes round-trips here).
// Missing or invalid values default to 0.
func FromObject(meta *metav1.ObjectMeta) int {
	return v1beta1.NumNodesFromAnnotation(meta)
}
