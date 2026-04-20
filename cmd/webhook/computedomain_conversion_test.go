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
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1beta1 "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	v1beta2 "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta2"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestServeComputeDomainConversionV1beta1ToV1beta2(t *testing.T) {
	cd := v1beta1.ComputeDomain{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1beta1.SchemeGroupVersion.String(),
			Kind:       "ComputeDomain",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns"},
		Spec: v1beta1.ComputeDomainSpec{
			Channel: &v1beta1.ComputeDomainChannelSpec{
				ResourceClaimTemplate: v1beta1.ComputeDomainResourceClaimTemplate{Name: "t"},
			},
		},
	}
	raw, err := json.Marshal(cd)
	if err != nil {
		t.Fatal(err)
	}
	review := conversionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "apiextensions.k8s.io/v1", Kind: "ConversionReview"},
		Request: conversionReviewRequest{
			UID:               "uid1",
			DesiredAPIVersion: v1beta2.SchemeGroupVersion.String(),
			Objects:           []runtime.RawExtension{{Raw: raw}},
		},
	}
	body, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/convert-computedomain", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	serveComputeDomainConversion(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	var out conversionReview
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Response == nil || out.Response.Result == nil {
		t.Fatalf("unexpected response: %+v", out.Response)
	}
	if out.Response.Result.Status != metav1.StatusSuccess {
		t.Fatalf("result status %q want %q", out.Response.Result.Status, metav1.StatusSuccess)
	}
	if len(out.Response.ConvertedObjects) != 1 {
		t.Fatalf("got %d objects", len(out.Response.ConvertedObjects))
	}
	var got v1beta2.ComputeDomain
	if err := json.Unmarshal(out.Response.ConvertedObjects[0].Raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.APIVersion != v1beta2.SchemeGroupVersion.String() {
		t.Fatalf("apiVersion %q", got.APIVersion)
	}
}
