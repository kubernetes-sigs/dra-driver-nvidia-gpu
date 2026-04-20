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

package conversion

import (
	"testing"

	v1beta1 "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRoundTrip(t *testing.T) {
	in := &v1beta1.ComputeDomain{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "resource.nvidia.com/v1beta1",
			Kind:       "ComputeDomain",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cd1",
			Namespace: "ns1",
		},
		Spec: v1beta1.ComputeDomainSpec{
			NumNodes: 0,
			Channel: &v1beta1.ComputeDomainChannelSpec{
				ResourceClaimTemplate: v1beta1.ComputeDomainResourceClaimTemplate{Name: "rct"},
				AllocationMode:        v1beta1.ComputeDomainChannelAllocationModeSingle,
			},
		},
		Status: v1beta1.ComputeDomainStatus{
			Status: v1beta1.ComputeDomainStatusNotReady,
			Nodes: []*v1beta1.ComputeDomainNode{
				{Name: "n1", IPAddress: "10.0.0.1", CliqueID: "c1", Index: 0, Status: v1beta1.ComputeDomainStatusReady},
			},
		},
	}

	v2, err := V1beta1ToV1beta2(in)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Annotations != nil {
		if _, ok := v2.Annotations[v1beta1.AnnotationComputeDomainNumNodes]; ok {
			t.Fatalf("expected no numNodes annotation when zero")
		}
	}
	v1, err := V1beta2ToV1beta1(v2)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Spec.Channel.ResourceClaimTemplate.Name != "rct" {
		t.Fatalf("channel name: got %q", v1.Spec.Channel.ResourceClaimTemplate.Name)
	}
	if len(v1.Status.Nodes) != 1 || v1.Status.Nodes[0].Name != "n1" {
		t.Fatalf("status nodes: %+v", v1.Status.Nodes)
	}
}

func TestNumNodesRoundTripViaHubAnnotation(t *testing.T) {
	in := &v1beta1.ComputeDomain{
		Spec: v1beta1.ComputeDomainSpec{
			NumNodes: 7,
			Channel: &v1beta1.ComputeDomainChannelSpec{
				ResourceClaimTemplate: v1beta1.ComputeDomainResourceClaimTemplate{Name: "x"},
			},
		},
	}
	hub, err := V1beta1ToV1beta2(in)
	if err != nil {
		t.Fatal(err)
	}
	if hub.Annotations[v1beta1.AnnotationComputeDomainNumNodes] != "7" {
		t.Fatalf("hub annotation: %+v", hub.Annotations)
	}
	legacy, err := V1beta2ToV1beta1(hub)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Spec.NumNodes != 7 {
		t.Fatalf("numNodes: got %d", legacy.Spec.NumNodes)
	}
	if legacy.Annotations != nil {
		if _, ok := legacy.Annotations[v1beta1.AnnotationComputeDomainNumNodes]; ok {
			t.Fatalf("annotation should not appear on v1beta1 view")
		}
	}
	back, err := V1beta1ToV1beta2(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if back.Annotations[v1beta1.AnnotationComputeDomainNumNodes] != "7" {
		t.Fatalf("round-trip hub annotation: %+v", back.Annotations)
	}
}
