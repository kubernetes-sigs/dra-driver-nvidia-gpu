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
	"os"
	"slices"
	"strings"

	"github.com/urfave/cli/v2"

	admissionv1 "k8s.io/api/admission/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
)

const (
	DriverName = "gpu.nvidia.com"
)

type Flags struct {
	loggingConfig     *flags.LoggingConfig
	featureGateConfig *flags.FeatureGateConfig

	certFile string
	keyFile  string
	port     int
}

func main() {
	if err := newApp().Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func newApp() *cli.App {
	flags := &Flags{
		loggingConfig:     flags.NewLoggingConfig(),
		featureGateConfig: flags.NewFeatureGateConfig(),
	}
	cliFlags := []cli.Flag{
		&cli.StringFlag{
			Name:        "tls-cert-file",
			Usage:       "File containing the default x509 Certificate for HTTPS. (CA cert, if any, concatenated after server cert).",
			Destination: &flags.certFile,
			Required:    true,
		},
		&cli.StringFlag{
			Name:        "tls-private-key-file",
			Usage:       "File containing the default x509 private key matching --tls-cert-file.",
			Destination: &flags.keyFile,
			Required:    true,
		},
		&cli.IntFlag{
			Name:        "port",
			Usage:       "Secure port that the webhook listens on",
			Value:       443,
			Destination: &flags.port,
		},
	}
	cliFlags = append(cliFlags, flags.loggingConfig.Flags()...)
	cliFlags = append(cliFlags, flags.featureGateConfig.Flags()...)

	app := &cli.App{
		Name:            "webhook",
		Usage:           "webhook implements a validating admission webhook complementing a DRA driver plugin.",
		ArgsUsage:       " ",
		HideHelpCommand: true,
		Flags:           cliFlags,
		Before: func(c *cli.Context) error {
			if c.Args().Len() > 0 {
				return fmt.Errorf("arguments not supported: %v", c.Args().Slice())
			}
			return flags.loggingConfig.Apply()
		},
		Action: func(c *cli.Context) error {
			server := &http.Server{
				Handler: newMux(),
				Addr:    fmt.Sprintf(":%d", flags.port),
			}
			klog.Info("starting webhook server on", server.Addr)
			return server.ListenAndServeTLS(flags.certFile, flags.keyFile)
		},
	}

	return app
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/validate-resource-claim-parameters", serveResourceClaim)
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, req *http.Request) {
		_, err := w.Write([]byte("ok"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	})
	return mux
}

func serveResourceClaim(w http.ResponseWriter, r *http.Request) {
	serve(w, r, admitResourceClaimParameters)
}

// serve handles the http portion of a request prior to handing to an admit
// function.
func serve(w http.ResponseWriter, r *http.Request, admit func(admissionv1.AdmissionReview) *admissionv1.AdmissionResponse) {
	var body []byte
	if r.Body != nil {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			klog.Error(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		body = data
	}

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		msg := fmt.Sprintf("contentType=%s, expected application/json", contentType)
		klog.Error(msg)
		http.Error(w, msg, http.StatusUnsupportedMediaType)
		return
	}

	klog.V(2).Infof("handling request: %s", body)

	requestedAdmissionReview, err := readAdmissionReview(body)
	if err != nil {
		msg := fmt.Sprintf("failed to read AdmissionReview from request body: %v", err)
		klog.Error(msg)
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	responseAdmissionReview := &admissionv1.AdmissionReview{}
	responseAdmissionReview.SetGroupVersionKind(requestedAdmissionReview.GroupVersionKind())
	responseAdmissionReview.Response = admit(*requestedAdmissionReview)
	responseAdmissionReview.Response.UID = requestedAdmissionReview.Request.UID

	klog.V(2).Infof("sending response: %v", responseAdmissionReview)
	respBytes, err := json.Marshal(responseAdmissionReview)
	if err != nil {
		klog.Error(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(respBytes); err != nil {
		klog.Error(err)
	}
}

func readAdmissionReview(data []byte) (*admissionv1.AdmissionReview, error) {
	deserializer := codecs.UniversalDeserializer()
	obj, gvk, err := deserializer.Decode(data, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("request could not be decoded: %w", err)
	}

	if *gvk != admissionv1.SchemeGroupVersion.WithKind("AdmissionReview") {
		return nil, fmt.Errorf("unsupported group version kind: %v", gvk)
	}

	requestedAdmissionReview, ok := obj.(*admissionv1.AdmissionReview)
	if !ok {
		return nil, fmt.Errorf("expected v1.AdmissionReview but got: %T", obj)
	}

	return requestedAdmissionReview, nil
}

// admitResourceClaimParameters accepts both ResourceClaims and ResourceClaimTemplates and validates their
// opaque device configuration parameters for this driver.
func admitResourceClaimParameters(ar admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	klog.V(2).Info("admitting resource claim parameters")

	var deviceConfigs []resourceapi.DeviceClaimConfiguration
	var deviceRequests []resourceapi.DeviceRequest
	var specPath string

	switch ar.Request.Resource {
	case resourceClaimResourceV1, resourceClaimResourceV1Beta1, resourceClaimResourceV1Beta2:
		claim, err := extractResourceClaim(ar)
		if err != nil {
			klog.Error(err)
			return &admissionv1.AdmissionResponse{
				Result: &metav1.Status{
					Message: err.Error(),
					Reason:  metav1.StatusReasonBadRequest,
				},
			}
		}
		deviceConfigs = claim.Spec.Devices.Config
		deviceRequests = claim.Spec.Devices.Requests
		specPath = "spec"
	case resourceClaimTemplateResourceV1, resourceClaimTemplateResourceV1Beta1, resourceClaimTemplateResourceV1Beta2:
		claimTemplate, err := extractResourceClaimTemplate(ar)
		if err != nil {
			klog.Error(err)
			return &admissionv1.AdmissionResponse{
				Result: &metav1.Status{
					Message: err.Error(),
					Reason:  metav1.StatusReasonBadRequest,
				},
			}
		}
		deviceConfigs = claimTemplate.Spec.Spec.Devices.Config
		deviceRequests = claimTemplate.Spec.Spec.Devices.Requests
		specPath = "spec.spec"
	default:
		msg := fmt.Sprintf("expected resource to be one of the supported versions for resourceclaims or resourceclaimtemplates, got %s", ar.Request.Resource)
		klog.Error(msg)
		return &admissionv1.AdmissionResponse{
			Result: &metav1.Status{
				Message: msg,
				Reason:  metav1.StatusReasonBadRequest,
			},
		}
	}

	adminRequests := requestsWithAdminAccess(deviceRequests)

	var errs []error
	for configIndex, config := range deviceConfigs {
		if config.Opaque == nil || config.Opaque.Driver != DriverName {
			continue
		}

		fieldPath := fmt.Sprintf("%s.devices.config[%d].opaque.parameters", specPath, configIndex)
		// Strict-decode: do not allow for users to provide unknown fields.
		decodedConfig, err := runtime.Decode(nvapi.StrictDecoder, config.Opaque.Parameters.Raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("error decoding object at %s: %w", fieldPath, err))
			continue
		}

		// Cast the opaque config to a nvapi.Interface type and validate it
		var configInterface nvapi.Interface
		switch castConfig := decodedConfig.(type) {
		case *nvapi.GpuConfig:
			configInterface = castConfig
		case *nvapi.MigDeviceConfig:
			configInterface = castConfig
		case *nvapi.ComputeDomainChannelConfig:
			configInterface = castConfig
		case *nvapi.ComputeDomainDaemonConfig:
			configInterface = castConfig
		default:
			errs = append(errs, fmt.Errorf("expected a recognized configuration type at %s but got: %T", fieldPath, decodedConfig))
			continue
		}

		// Normalize the config to set any implied defaults
		if err := configInterface.Normalize(); err != nil {
			errs = append(errs, fmt.Errorf("error normalizing config at %s: %w", fieldPath, err))
			continue
		}

		// Validate the config to ensure its integrity
		if err := configInterface.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("object at %s is invalid: %w", fieldPath, err))
			continue
		}

		// Reject sharing settings that apply to an adminAccess request. The kubelet
		// plugin enforces the same rule at Prepare() time (see
		// validateNoSharingWithAdminAccess in cmd/gpu-kubelet-plugin); checking here
		// as well surfaces the error at admission time instead of at pod start.
		//
		// Only on CREATE: spec is immutable on both objects, so an object admitted
		// before this check existed can never be brought into compliance, and failing
		// its UPDATEs would block finalizer removal and wedge deletion.
		if ar.Request.Operation == admissionv1.Create {
			if err := validateNoSharingWithAdminAccess(configInterface, config.Requests, adminRequests); err != nil {
				errs = append(errs, fmt.Errorf("object at %s is invalid: %w", fieldPath, err))
			}
		}
	}

	if len(errs) > 0 {
		var errMsgs []string
		for _, err := range errs {
			errMsgs = append(errMsgs, err.Error())
		}
		msg := fmt.Sprintf("%d configs failed to validate: %s", len(errs), strings.Join(errMsgs, "; "))
		klog.Error(msg)
		return &admissionv1.AdmissionResponse{
			Result: &metav1.Status{
				Message: msg,
				Reason:  metav1.StatusReason(metav1.StatusReasonInvalid),
			},
		}
	}

	return &admissionv1.AdmissionResponse{
		Allowed: true,
	}
}

// requestsWithAdminAccess returns the names of all requests in the claim spec that
// set adminAccess, in spec order. AdminAccess only exists on exact requests:
// prioritized-list subrequests (firstAvailable) cannot set it.
func requestsWithAdminAccess(requests []resourceapi.DeviceRequest) []string {
	var admin []string
	for _, r := range requests {
		if r.Exactly != nil && r.Exactly.AdminAccess != nil && *r.Exactly.AdminAccess {
			admin = append(admin, r.Name)
		}
	}
	return admin
}

// validateNoSharingWithAdminAccess rejects a sharing configuration (TimeSlicing or
// MPS) that applies to a request marked adminAccess. Such a claim is meant to be a
// read-only observer of a device, but sharing settings are device-global: applying
// them on Prepare, and tearing them down on Unprepare, would modify state that the
// workload owning the device depends on. The kubelet plugin rejects the combination
// at Prepare() time; this check reports the same error at admission time.
//
// Only configs that name their requests explicitly are checked. A config with an
// empty request list applies to every request in the claim, but the kubelet plugin
// narrows it further by device type, which the webhook cannot do from the claim spec
// alone: rejecting on a name match that the plugin would never make would deny valid
// claims. The plugin catches that case at Prepare() time instead.
//
// adminRequests only ever contains names of exact requests, so references to
// subrequests ("<request>/<subrequest>") can never match, which is correct:
// subrequests cannot set adminAccess.
func validateNoSharingWithAdminAccess(config nvapi.Interface, targetRequests []string, adminRequests []string) error {
	if len(adminRequests) == 0 || len(targetRequests) == 0 {
		return nil
	}

	var sharing nvapi.Sharing
	switch castConfig := config.(type) {
	case *nvapi.GpuConfig:
		sharing = castConfig.Sharing
	case *nvapi.MigDeviceConfig:
		sharing = castConfig.Sharing
	default:
		return nil
	}

	var strategy string
	switch {
	case sharing.IsTimeSlicing():
		strategy = nvapi.TimeSlicingStrategy
	case sharing.IsMps():
		strategy = nvapi.MpsStrategy
	default:
		return nil
	}

	offending := ""
	for _, t := range targetRequests {
		if slices.Contains(adminRequests, t) {
			offending = t
			break
		}
	}
	if offending == "" {
		return nil
	}

	return fmt.Errorf(
		"%s sharing configuration is not allowed for request %q: the request sets adminAccess, and sharing settings apply to the whole device",
		strategy, offending,
	)
}
