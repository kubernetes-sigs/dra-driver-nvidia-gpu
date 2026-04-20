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

package v1beta1

import (
	"fmt"
	"strconv"

	v1beta2 "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta2"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConvertTo implements the hub (v1beta2) side of multi-version conversion (kubebuilder pattern).
func (src *ComputeDomain) ConvertTo(dst *v1beta2.ComputeDomain) error {
	if src == nil || dst == nil {
		return fmt.Errorf("ConvertTo: nil ComputeDomain")
	}
	dst.TypeMeta = metav1.TypeMeta{
		APIVersion: v1beta2.SchemeGroupVersion.String(),
		Kind:       "ComputeDomain",
	}
	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()
	syncNumNodesAnnotationOnHub(dst, src.Spec.NumNodes)
	dst.Spec = v1beta2.ComputeDomainSpec{}
	if src.Spec.Channel != nil {
		dst.Spec.Channel = &v1beta2.ComputeDomainChannelSpec{
			ResourceClaimTemplate: v1beta2.ComputeDomainResourceClaimTemplate{
				Name: src.Spec.Channel.ResourceClaimTemplate.Name,
			},
			AllocationMode: src.Spec.Channel.AllocationMode,
		}
	}
	dst.Status = v1beta1StatusToV1beta2(&src.Status)
	return nil
}

// ConvertFrom restores a deprecated v1beta1 view from the hub (v1beta2) representation.
func (dst *ComputeDomain) ConvertFrom(src *v1beta2.ComputeDomain) error {
	if src == nil || dst == nil {
		return fmt.Errorf("ConvertFrom: nil ComputeDomain")
	}
	dst.TypeMeta = metav1.TypeMeta{
		APIVersion: SchemeGroupVersion.String(),
		Kind:       "ComputeDomain",
	}
	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()
	dst.Spec = ComputeDomainSpec{
		NumNodes: numNodesFromAnnotationsMap(src.Annotations),
	}
	// Hide hub-only storage key from the deprecated API surface.
	if dst.Annotations != nil {
		delete(dst.Annotations, AnnotationComputeDomainNumNodes)
	}
	if src.Spec.Channel != nil {
		dst.Spec.Channel = &ComputeDomainChannelSpec{
			ResourceClaimTemplate: ComputeDomainResourceClaimTemplate{
				Name: src.Spec.Channel.ResourceClaimTemplate.Name,
			},
			AllocationMode: src.Spec.Channel.AllocationMode,
		}
	}
	dst.Status = v1beta2StatusToV1beta1(&src.Status)
	return nil
}

// syncNumNodesAnnotationOnHub writes the v1beta1 numNodes carry annotation on the hub (v1beta2) object.
func syncNumNodesAnnotationOnHub(cd *v1beta2.ComputeDomain, n int) {
	if cd.Annotations == nil {
		cd.Annotations = map[string]string{}
	}
	if n == 0 {
		delete(cd.Annotations, AnnotationComputeDomainNumNodes)
		return
	}
	cd.Annotations[AnnotationComputeDomainNumNodes] = strconv.Itoa(n)
}

func numNodesFromAnnotationsMap(annotations map[string]string) int {
	if annotations == nil {
		return 0
	}
	s, ok := annotations[AnnotationComputeDomainNumNodes]
	if !ok || s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// NumNodesFromAnnotation returns the v1beta1 numNodes value carried on the hub object.
func NumNodesFromAnnotation(meta *metav1.ObjectMeta) int {
	if meta == nil {
		return 0
	}
	return numNodesFromAnnotationsMap(meta.Annotations)
}

func v1beta1StatusToV1beta2(in *ComputeDomainStatus) v1beta2.ComputeDomainStatus {
	if in == nil {
		return v1beta2.ComputeDomainStatus{}
	}
	out := v1beta2.ComputeDomainStatus{
		Status: in.Status,
	}
	if len(in.Nodes) > 0 {
		out.Nodes = make([]*v1beta2.ComputeDomainNode, len(in.Nodes))
		for i, n := range in.Nodes {
			if n == nil {
				continue
			}
			out.Nodes[i] = &v1beta2.ComputeDomainNode{
				Name:      n.Name,
				IPAddress: n.IPAddress,
				CliqueID:  n.CliqueID,
				Index:     n.Index,
				Status:    n.Status,
			}
		}
	}
	return out
}

func v1beta2StatusToV1beta1(in *v1beta2.ComputeDomainStatus) ComputeDomainStatus {
	if in == nil {
		return ComputeDomainStatus{}
	}
	out := ComputeDomainStatus{
		Status: in.Status,
	}
	if len(in.Nodes) > 0 {
		out.Nodes = make([]*ComputeDomainNode, len(in.Nodes))
		for i, n := range in.Nodes {
			if n == nil {
				continue
			}
			out.Nodes[i] = &ComputeDomainNode{
				Name:      n.Name,
				IPAddress: n.IPAddress,
				CliqueID:  n.CliqueID,
				Index:     n.Index,
				Status:    n.Status,
			}
		}
	}
	return out
}
