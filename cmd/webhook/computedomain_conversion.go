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
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	v1beta1 "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	v1beta2 "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta2"
	computedomainconversion "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/apis/computedomain/conversion"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
)

// conversionReview mirrors apiextensions.k8s.io/v1 ConversionReview (subset used by the apiserver).
type conversionReview struct {
	metav1.TypeMeta `json:",inline"`
	Request         conversionReviewRequest   `json:"request,omitempty"`
	Response        *conversionReviewResponse `json:"response,omitempty"`
}

type conversionReviewRequest struct {
	UID               string                 `json:"uid"`
	DesiredAPIVersion string                 `json:"desiredAPIVersion"`
	Objects           []runtime.RawExtension `json:"objects"`
}

type conversionReviewResponse struct {
	UID              string                 `json:"uid"`
	ConvertedObjects []runtime.RawExtension `json:"convertedObjects,omitempty"`
	Result           *metav1.Status         `json:"result,omitempty"`
}

func serveComputeDomainConversion(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
		return
	}

	var review conversionReview
	if err := json.Unmarshal(body, &review); err != nil {
		klog.ErrorS(err, "decode ConversionReview")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resp := &conversionReviewResponse{UID: review.Request.UID}
	var converted []runtime.RawExtension
	for i := range review.Request.Objects {
		out, convErr := convertComputeDomainRaw(review.Request.DesiredAPIVersion, review.Request.Objects[i])
		if convErr != nil {
			resp.Result = &metav1.Status{
				Status:  metav1.StatusFailure,
				Message: convErr.Error(),
				Code:    http.StatusBadRequest,
				Reason:  metav1.StatusReasonBadRequest,
			}
			break
		}
		converted = append(converted, out)
	}
	if resp.Result == nil {
		// apiextensions requires response.result.status == "Success" on success (not omitted).
		resp.Result = &metav1.Status{Status: metav1.StatusSuccess}
		resp.ConvertedObjects = converted
	}

	outReview := conversionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apiextensions.k8s.io/v1",
			Kind:       "ConversionReview",
		},
		Response: resp,
	}
	outBytes, err := json.Marshal(outReview)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(outBytes); err != nil {
		klog.ErrorS(err, "write ConversionReview response")
	}
}

func convertComputeDomainRaw(desiredAPIVersion string, in runtime.RawExtension) (runtime.RawExtension, error) {
	if len(in.Raw) == 0 {
		return runtime.RawExtension{}, fmt.Errorf("empty object")
	}
	var tm metav1.TypeMeta
	if err := json.Unmarshal(in.Raw, &tm); err != nil {
		return runtime.RawExtension{}, fmt.Errorf("decode TypeMeta: %w", err)
	}

	switch desiredAPIVersion {
	case v1beta2.SchemeGroupVersion.String():
		switch tm.APIVersion {
		case v1beta2.SchemeGroupVersion.String():
			return in, nil
		case v1beta1.SchemeGroupVersion.String():
			var obj v1beta1.ComputeDomain
			if err := json.Unmarshal(in.Raw, &obj); err != nil {
				return runtime.RawExtension{}, fmt.Errorf("decode v1beta1 ComputeDomain: %w", err)
			}
			out, err := computedomainconversion.V1beta1ToV1beta2(&obj)
			if err != nil {
				return runtime.RawExtension{}, err
			}
			raw, err := json.Marshal(out)
			if err != nil {
				return runtime.RawExtension{}, err
			}
			return runtime.RawExtension{Raw: raw}, nil
		default:
			return runtime.RawExtension{}, fmt.Errorf("unsupported object apiVersion %q for conversion to %s", tm.APIVersion, desiredAPIVersion)
		}
	case v1beta1.SchemeGroupVersion.String():
		switch tm.APIVersion {
		case v1beta1.SchemeGroupVersion.String():
			return in, nil
		case v1beta2.SchemeGroupVersion.String():
			var obj v1beta2.ComputeDomain
			if err := json.Unmarshal(in.Raw, &obj); err != nil {
				return runtime.RawExtension{}, fmt.Errorf("decode v1beta2 ComputeDomain: %w", err)
			}
			out, err := computedomainconversion.V1beta2ToV1beta1(&obj)
			if err != nil {
				return runtime.RawExtension{}, err
			}
			raw, err := json.Marshal(out)
			if err != nil {
				return runtime.RawExtension{}, err
			}
			return runtime.RawExtension{Raw: raw}, nil
		default:
			return runtime.RawExtension{}, fmt.Errorf("unsupported object apiVersion %q for conversion to %s", tm.APIVersion, desiredAPIVersion)
		}
	default:
		return runtime.RawExtension{}, fmt.Errorf("unsupported desiredAPIVersion %q", desiredAPIVersion)
	}
}
