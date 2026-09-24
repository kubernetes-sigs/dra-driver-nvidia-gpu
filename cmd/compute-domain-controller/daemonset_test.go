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
	"testing"
	"text/template"

	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/types"
)

func baseDaemonSetTemplateData() DaemonSetTemplateData {
	return DaemonSetTemplateData{
		Namespace:                 "test-ns",
		Name:                      "computedomain-daemon-test",
		Finalizer:                 "resource.nvidia.com/computedomain-finalizer",
		ComputeDomainLabelKey:     "resource.nvidia.com/computeDomain",
		ComputeDomainLabelValue:   types.UID("cd-uid"),
		ResourceClaimTemplateName: "computedomain-daemon-test",
		ImageName:                 "example.com/dra-driver-nvidia-gpu:test",
		MaxNodesPerIMEXDomain:     18,
		LogVerbosity:              4,
	}
}

func TestDaemonSetTemplateRendersIMEXConfigOverrides(t *testing.T) {
	tmpl, err := template.ParseFiles("../../templates/compute-domain-daemon.tmpl.yaml")
	require.NoError(t, err)

	data := baseDaemonSetTemplateData()
	data.IMEXConfigOverrides = map[string]string{
		"IMEX_NODE_DISCONNECTED_GRACE_TIME": "45",
	}

	var out bytes.Buffer
	require.NoError(t, tmpl.Execute(&out, data))

	require.Contains(t, out.String(), "- name: IMEX_CONFIG_OVERRIDES")
	require.Contains(t, out.String(), "IMEX_NODE_DISCONNECTED_GRACE_TIME=45,")
}

func TestDaemonSetTemplateOmitsIMEXConfigOverridesWhenUnset(t *testing.T) {
	tmpl, err := template.ParseFiles("../../templates/compute-domain-daemon.tmpl.yaml")
	require.NoError(t, err)

	data := baseDaemonSetTemplateData()

	var out bytes.Buffer
	require.NoError(t, tmpl.Execute(&out, data))

	require.NotContains(t, out.String(), "IMEX_CONFIG_OVERRIDES")
}
