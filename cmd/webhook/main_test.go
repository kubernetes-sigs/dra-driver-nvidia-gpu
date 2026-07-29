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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	admissionv1 "k8s.io/api/admission/v1"
	resourceapi "k8s.io/api/resource/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/utils/ptr"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func TestReadyEndpoint(t *testing.T) {
	s := httptest.NewServer(newMux())
	t.Cleanup(s.Close)

	res, err := http.Get(s.URL + "/readyz")
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, res.StatusCode)
}

func TestResourceClaimValidatingWebhook(t *testing.T) {

	tests := map[string]struct {
		featureGates         map[string]bool
		admissionReview      *admissionv1.AdmissionReview
		requestContentType   string
		expectedResponseCode int
		expectedAllowed      bool
		expectedMessage      string
	}{
		"bad contentType": {
			requestContentType:   "invalid type",
			expectedResponseCode: http.StatusUnsupportedMediaType,
		},
		"invalid AdmissionReview": {
			admissionReview:      &admissionv1.AdmissionReview{},
			expectedResponseCode: http.StatusBadRequest,
		},
		"valid GpuConfig in ResourceClaim": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithGpuConfigs(
					resourceClaimResourceV1Beta1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.DefaultTimeSlice),
							},
						},
					},
				),
			),
			expectedAllowed: true,
		},
		"invalid GpuConfigs in ResourceClaim": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
				string(featuregates.MPSSupport):          true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithGpuConfigs(
					resourceClaimResourceV1Beta1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.TimeSliceInterval("Invalid Interval")),
							},
						},
					},
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.MpsStrategy,
							MpsConfig: &configapi.MpsConfig{
								DefaultActiveThreadPercentage: ptr.To(-1),
							},
						},
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: "2 configs failed to validate: object at spec.devices.config[0].opaque.parameters is invalid: unknown time-slice interval: Invalid Interval, supported time-slice intervals: Default, Short, Medium, Long; object at spec.devices.config[1].opaque.parameters is invalid: active thread percentage must not be negative",
		},
		"valid GpuConfig in ResourceClaimTemplate": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimTemplateWithGpuConfigs(
					resourceClaimTemplateResourceV1Beta1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.DefaultTimeSlice),
							},
						},
					},
				),
			),
			expectedAllowed: true,
		},
		"invalid GpuConfigs in ResourceClaimTemplate": {
			admissionReview: admissionReviewWithObject(
				resourceClaimTemplateWithGpuConfigs(
					resourceClaimTemplateResourceV1Beta1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.TimeSliceInterval("Invalid Interval")),
							},
						},
					},
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.MpsStrategy,
							MpsConfig: &configapi.MpsConfig{
								DefaultActiveThreadPercentage: ptr.To(-1),
							},
						},
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: "2 configs failed to validate: object at spec.spec.devices.config[0].opaque.parameters is invalid: unknown time-slice interval: Invalid Interval, supported time-slice intervals: Default, Short, Medium, Long; object at spec.spec.devices.config[1].opaque.parameters is invalid: active thread percentage must not be negative",
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
				string(featuregates.MPSSupport):          true,
			},
		},

		// v1 API version tests
		"valid GpuConfig in ResourceClaim v1": {
			admissionReview: admissionReviewWithObject(
				resourceClaimWithGpuConfigs(
					resourceClaimResourceV1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.DefaultTimeSlice),
							},
						},
					},
				),
			),
			expectedAllowed: true,
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
		},
		"valid GpuConfig in ResourceClaimTemplate v1": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimTemplateWithGpuConfigs(
					resourceClaimTemplateResourceV1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.DefaultTimeSlice),
							},
						},
					},
				),
			),
			expectedAllowed: true,
		},
		"invalid GpuConfig in ResourceClaim v1 (tests conversion)": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithGpuConfigs(
					resourceClaimResourceV1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.TimeSliceInterval("Invalid Interval")),
							},
						},
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: "1 configs failed to validate: object at spec.devices.config[0].opaque.parameters is invalid: unknown time-slice interval: Invalid Interval, supported time-slice intervals: Default, Short, Medium, Long",
		},
		"invalid GpuConfig in ResourceClaimTemplate v1 (tests conversion)": {
			featureGates: map[string]bool{
				string(featuregates.MPSSupport): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimTemplateWithGpuConfigs(
					resourceClaimTemplateResourceV1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.MpsStrategy,
							MpsConfig: &configapi.MpsConfig{
								DefaultActiveThreadPercentage: ptr.To(-1),
							},
						},
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: "1 configs failed to validate: object at spec.spec.devices.config[0].opaque.parameters is invalid: active thread percentage must not be negative",
		},

		// v1beta2 API version tests
		"valid GpuConfig in ResourceClaim v1beta2": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithGpuConfigs(
					resourceClaimResourceV1Beta2,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.DefaultTimeSlice),
							},
						},
					},
				),
			),
			expectedAllowed: true,
		},
		"valid GpuConfig in ResourceClaimTemplate v1beta2": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimTemplateWithGpuConfigs(
					resourceClaimTemplateResourceV1Beta2,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.DefaultTimeSlice),
							},
						},
					},
				),
			),
			expectedAllowed: true,
		},
		"invalid GpuConfig in ResourceClaim v1beta2 (tests conversion)": {
			featureGates: map[string]bool{
				string(featuregates.MPSSupport): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithGpuConfigs(
					resourceClaimResourceV1Beta2,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.MpsStrategy,
							MpsConfig: &configapi.MpsConfig{
								DefaultActiveThreadPercentage: ptr.To(-1),
							},
						},
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: "1 configs failed to validate: object at spec.devices.config[0].opaque.parameters is invalid: active thread percentage must not be negative",
		},
		"invalid GpuConfig in ResourceClaimTemplate v1beta2 (tests conversion)": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimTemplateWithGpuConfigs(
					resourceClaimTemplateResourceV1Beta2,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.TimeSliceInterval("Invalid Interval")),
							},
						},
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: "1 configs failed to validate: object at spec.spec.devices.config[0].opaque.parameters is invalid: unknown time-slice interval: Invalid Interval, supported time-slice intervals: Default, Short, Medium, Long",
		},

		// Feature gate disabled tests - these should fail when feature gates are off
		"TimeSlicingStrategy rejected when TimeSlicingSettings feature gate is disabled": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): false,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithGpuConfigs(
					resourceClaimResourceV1Beta1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.DefaultTimeSlice),
							},
						},
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: `1 configs failed to validate: object at spec.devices.config[0].opaque.parameters is invalid: "TimeSlicing" is selected as the GPU sharing strategy, but the "TimeSlicingSettings" feature gate is not enabled`,
		},
		"MpsStrategy rejected when MPSSupport feature gate is disabled": {
			featureGates: map[string]bool{
				string(featuregates.MPSSupport): false,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithGpuConfigs(
					resourceClaimResourceV1Beta1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.MpsStrategy,
							MpsConfig: &configapi.MpsConfig{
								DefaultActiveThreadPercentage: ptr.To(50),
							},
						},
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: `1 configs failed to validate: object at spec.devices.config[0].opaque.parameters is invalid: "MPS" is selected as the GPU sharing strategy, but the "MPSSupport" feature gate is not enabled`,
		},
		"TimeSlicingStrategy rejected in ResourceClaimTemplate when TimeSlicingSettings feature gate is disabled": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): false,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimTemplateWithGpuConfigs(
					resourceClaimTemplateResourceV1Beta1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.TimeSlicingStrategy,
							TimeSlicingConfig: &configapi.TimeSlicingConfig{
								Interval: ptr.To(configapi.DefaultTimeSlice),
							},
						},
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: `1 configs failed to validate: object at spec.spec.devices.config[0].opaque.parameters is invalid: "TimeSlicing" is selected as the GPU sharing strategy, but the "TimeSlicingSettings" feature gate is not enabled`,
		},
		// adminAccess + sharing tests: a sharing configuration must not apply to a
		// request that sets adminAccess (issue #1280).
		"TimeSlicing sharing targeting an adminAccess request is rejected": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithRequestsAndConfigs(
					resourceClaimResourceV1Beta1,
					[]resourceapi.DeviceRequest{
						deviceRequest("monitor", true),
					},
					[]resourceapi.DeviceClaimConfiguration{
						gpuDeviceConfig(&configapi.GpuConfig{
							Sharing: &configapi.GpuSharing{
								Strategy: configapi.TimeSlicingStrategy,
							},
						}, "monitor"),
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: `1 configs failed to validate: object at spec.devices.config[0].opaque.parameters is invalid: TimeSlicing sharing configuration is not allowed for request "monitor": the request sets adminAccess, and sharing settings apply to the whole device`,
		},
		// A config with no request names applies to every request in the claim, but
		// the kubelet plugin narrows it further by device type and the webhook cannot
		// see device types. Admitting it here and letting Prepare() reject it is the
		// conservative choice: the opposite would deny claims the plugin would accept.
		"MPS sharing applying to all requests is admitted even when a request sets adminAccess": {
			featureGates: map[string]bool{
				string(featuregates.MPSSupport): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithRequestsAndConfigs(
					resourceClaimResourceV1Beta1,
					[]resourceapi.DeviceRequest{
						deviceRequest("workload", false),
						deviceRequest("monitor", true),
					},
					[]resourceapi.DeviceClaimConfiguration{
						gpuDeviceConfig(&configapi.GpuConfig{
							Sharing: &configapi.GpuSharing{
								Strategy: configapi.MpsStrategy,
							},
						}),
					},
				),
			),
			expectedAllowed: true,
		},
		// spec is immutable on both objects, so an object admitted before this check
		// existed can never comply. Failing its UPDATEs would block finalizer removal.
		"sharing targeting an adminAccess request is admitted on UPDATE": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
			admissionReview: asUpdate(admissionReviewWithObject(
				resourceClaimWithRequestsAndConfigs(
					resourceClaimResourceV1Beta1,
					[]resourceapi.DeviceRequest{
						deviceRequest("monitor", true),
					},
					[]resourceapi.DeviceClaimConfiguration{
						gpuDeviceConfig(&configapi.GpuConfig{
							Sharing: &configapi.GpuSharing{
								Strategy: configapi.TimeSlicingStrategy,
							},
						}, "monitor"),
					},
				),
			)),
			expectedAllowed: true,
		},
		"sharing targeting only workload requests is allowed alongside an adminAccess request": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithRequestsAndConfigs(
					resourceClaimResourceV1Beta1,
					[]resourceapi.DeviceRequest{
						deviceRequest("workload", false),
						deviceRequest("monitor", true),
					},
					[]resourceapi.DeviceClaimConfiguration{
						gpuDeviceConfig(&configapi.GpuConfig{
							Sharing: &configapi.GpuSharing{
								Strategy: configapi.TimeSlicingStrategy,
							},
						}, "workload"),
					},
				),
			),
			expectedAllowed: true,
		},
		"adminAccess request without sharing config is allowed": {
			admissionReview: admissionReviewWithObject(
				resourceClaimWithRequestsAndConfigs(
					resourceClaimResourceV1Beta1,
					[]resourceapi.DeviceRequest{
						deviceRequest("monitor", true),
					},
					[]resourceapi.DeviceClaimConfiguration{
						gpuDeviceConfig(&configapi.GpuConfig{}),
					},
				),
			),
			expectedAllowed: true,
		},
		// A prioritized-list request has no Exactly section, so it can never set
		// adminAccess, and a config that targets one of its subrequests
		// ("<request>/<subrequest>") must not be matched against the adminAccess
		// request names.
		"sharing targeting a subrequest is allowed alongside an adminAccess request": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimWithRequestsAndConfigs(
					resourceClaimResourceV1Beta1,
					[]resourceapi.DeviceRequest{
						deviceRequestWithSubRequest("workload", "big"),
						deviceRequest("monitor", true),
					},
					[]resourceapi.DeviceClaimConfiguration{
						gpuDeviceConfig(&configapi.GpuConfig{
							Sharing: &configapi.GpuSharing{
								Strategy: configapi.TimeSlicingStrategy,
							},
						}, "workload/big"),
					},
				),
			),
			expectedAllowed: true,
		},
		"TimeSlicing sharing targeting an adminAccess request in ResourceClaimTemplate is rejected": {
			featureGates: map[string]bool{
				string(featuregates.TimeSlicingSettings): true,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimTemplateWithRequestsAndConfigs(
					resourceClaimTemplateResourceV1Beta1,
					[]resourceapi.DeviceRequest{
						deviceRequest("monitor", true),
					},
					[]resourceapi.DeviceClaimConfiguration{
						gpuDeviceConfig(&configapi.GpuConfig{
							Sharing: &configapi.GpuSharing{
								Strategy: configapi.TimeSlicingStrategy,
							},
						}, "monitor"),
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: `1 configs failed to validate: object at spec.spec.devices.config[0].opaque.parameters is invalid: TimeSlicing sharing configuration is not allowed for request "monitor": the request sets adminAccess, and sharing settings apply to the whole device`,
		},

		"MpsStrategy rejected in ResourceClaimTemplate when MPSSupport feature gate is disabled": {
			featureGates: map[string]bool{
				string(featuregates.MPSSupport): false,
			},
			admissionReview: admissionReviewWithObject(
				resourceClaimTemplateWithGpuConfigs(
					resourceClaimTemplateResourceV1Beta1,
					&configapi.GpuConfig{
						Sharing: &configapi.GpuSharing{
							Strategy: configapi.MpsStrategy,
							MpsConfig: &configapi.MpsConfig{
								DefaultActiveThreadPercentage: ptr.To(50),
							},
						},
					},
				),
			),
			expectedAllowed: false,
			expectedMessage: `1 configs failed to validate: object at spec.spec.devices.config[0].opaque.parameters is invalid: "MPS" is selected as the GPU sharing strategy, but the "MPSSupport" feature gate is not enabled`,
		},
	}

	s := httptest.NewServer(newMux())
	t.Cleanup(s.Close)

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := featuregates.FeatureGates().SetEmulationVersion(utilversion.MajorMinor(999, 999))
			require.NoError(t, err)

			if test.featureGates != nil {
				err := featuregates.FeatureGates().SetFromMap(test.featureGates)
				require.NoError(t, err)
			}

			requestBody, err := json.Marshal(test.admissionReview)
			require.NoError(t, err)

			contentType := test.requestContentType
			if contentType == "" {
				contentType = "application/json"
			}

			res, err := http.Post(s.URL+"/validate-resource-claim-parameters", contentType, bytes.NewReader(requestBody))
			require.NoError(t, err)
			expectedResponseCode := test.expectedResponseCode
			if expectedResponseCode == 0 {
				expectedResponseCode = http.StatusOK
			}
			assert.Equal(t, expectedResponseCode, res.StatusCode)
			if res.StatusCode != http.StatusOK {
				// We don't have an AdmissionReview to validate
				return
			}

			responseBody, err := io.ReadAll(res.Body)
			require.NoError(t, err)
			res.Body.Close()

			responseAdmissionReview, err := readAdmissionReview(responseBody)
			assert.NoError(t, err)
			assert.Equal(t, test.expectedAllowed, responseAdmissionReview.Response.Allowed)
			if !test.expectedAllowed {
				assert.Equal(t, test.expectedMessage, string(responseAdmissionReview.Response.Result.Message))
			}
		})
	}
}

func admissionReviewWithObject(obj runtime.Object) *admissionv1.AdmissionReview {
	// Extract GVR from the object's GVK
	gvk := obj.GetObjectKind().GroupVersionKind()
	resource := metav1.GroupVersionResource{
		Group:    gvk.Group,
		Version:  gvk.Version,
		Resource: strings.ToLower(gvk.Kind) + "s", // Convert Kind to resource name
	}

	requestedAdmissionReview := &admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			Resource:  resource,
			Operation: admissionv1.Create,
			Object: runtime.RawExtension{
				Object: obj,
			},
		},
	}
	requestedAdmissionReview.SetGroupVersionKind(admissionv1.SchemeGroupVersion.WithKind("AdmissionReview"))
	return requestedAdmissionReview
}

// asUpdate rewrites an admission review to look like an UPDATE of an existing
// object rather than a CREATE.
func asUpdate(ar *admissionv1.AdmissionReview) *admissionv1.AdmissionReview {
	ar.Request.Operation = admissionv1.Update
	return ar
}

// deviceRequest builds a v1beta1 device request. In v1beta1 adminAccess is a flat
// field on the request; the webhook's conversion to v1 moves it under `exactly`.
func deviceRequest(name string, adminAccess bool) resourceapi.DeviceRequest {
	r := resourceapi.DeviceRequest{
		Name:            name,
		DeviceClassName: DriverName,
	}
	if adminAccess {
		r.AdminAccess = ptr.To(true)
	}
	return r
}

// deviceRequestWithSubRequest builds a prioritized-list request. Such a request
// carries its details in FirstAvailable rather than at the top level, so after
// conversion to v1 its Exactly section is nil and it cannot set adminAccess.
func deviceRequestWithSubRequest(name, subRequestName string) resourceapi.DeviceRequest {
	return resourceapi.DeviceRequest{
		Name: name,
		FirstAvailable: []resourceapi.DeviceSubRequest{
			{
				Name:            subRequestName,
				DeviceClassName: DriverName,
				AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
				Count:           1,
			},
		},
	}
}

// gpuDeviceConfig wraps a GpuConfig in a DeviceClaimConfiguration applying to the
// given request names (none means "applies to all requests").
func gpuDeviceConfig(gpuConfig *configapi.GpuConfig, requests ...string) resourceapi.DeviceClaimConfiguration {
	gpuConfig.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   configapi.GroupName,
		Version: configapi.Version,
		Kind:    "GpuConfig",
	})
	return resourceapi.DeviceClaimConfiguration{
		Requests: requests,
		DeviceConfiguration: resourceapi.DeviceConfiguration{
			Opaque: &resourceapi.OpaqueDeviceConfiguration{
				Driver: DriverName,
				Parameters: runtime.RawExtension{
					Object: gpuConfig,
				},
			},
		},
	}
}

func resourceClaimWithRequestsAndConfigs(gvr metav1.GroupVersionResource, requests []resourceapi.DeviceRequest, configs []resourceapi.DeviceClaimConfiguration) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: gvr.Group + "/" + gvr.Version,
			Kind:       "ResourceClaim",
		},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{
				Requests: requests,
				Config:   configs,
			},
		},
	}
}

func resourceClaimTemplateWithRequestsAndConfigs(gvr metav1.GroupVersionResource, requests []resourceapi.DeviceRequest, configs []resourceapi.DeviceClaimConfiguration) *resourceapi.ResourceClaimTemplate {
	return &resourceapi.ResourceClaimTemplate{
		TypeMeta: metav1.TypeMeta{
			APIVersion: gvr.Group + "/" + gvr.Version,
			Kind:       "ResourceClaimTemplate",
		},
		Spec: resourceapi.ResourceClaimTemplateSpec{
			Spec: resourceapi.ResourceClaimSpec{
				Devices: resourceapi.DeviceClaim{
					Requests: requests,
					Config:   configs,
				},
			},
		},
	}
}

func resourceClaimWithGpuConfigs(gvr metav1.GroupVersionResource, gpuConfigs ...*configapi.GpuConfig) *resourceapi.ResourceClaim {
	resourceClaim := &resourceapi.ResourceClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: gvr.Group + "/" + gvr.Version,
			Kind:       "ResourceClaim",
		},
		Spec: resourceClaimSpecWithGpuConfigs(gpuConfigs...),
	}
	return resourceClaim
}

func resourceClaimTemplateWithGpuConfigs(gvr metav1.GroupVersionResource, gpuConfigs ...*configapi.GpuConfig) *resourceapi.ResourceClaimTemplate {
	resourceClaimTemplate := &resourceapi.ResourceClaimTemplate{
		TypeMeta: metav1.TypeMeta{
			APIVersion: gvr.Group + "/" + gvr.Version,
			Kind:       "ResourceClaimTemplate",
		},
		Spec: resourceapi.ResourceClaimTemplateSpec{
			Spec: resourceClaimSpecWithGpuConfigs(gpuConfigs...),
		},
	}
	return resourceClaimTemplate
}

func resourceClaimSpecWithGpuConfigs(gpuConfigs ...*configapi.GpuConfig) resourceapi.ResourceClaimSpec {
	resourceClaimSpec := resourceapi.ResourceClaimSpec{}
	for _, gpuConfig := range gpuConfigs {
		gpuConfig.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   configapi.GroupName,
			Version: configapi.Version,
			Kind:    "GpuConfig",
		})
		deviceConfig := resourceapi.DeviceClaimConfiguration{
			DeviceConfiguration: resourceapi.DeviceConfiguration{
				Opaque: &resourceapi.OpaqueDeviceConfiguration{
					Driver: DriverName,
					Parameters: runtime.RawExtension{
						Object: gpuConfig,
					},
				},
			},
		}
		resourceClaimSpec.Devices.Config = append(resourceClaimSpec.Devices.Config, deviceConfig)
	}
	return resourceClaimSpec
}
