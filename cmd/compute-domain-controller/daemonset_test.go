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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

func TestRendersPriorityClassAndResources(t *testing.T) {
	tmpl, err := template.ParseFiles("../../templates/compute-domain-daemon.tmpl.yaml")
	require.NoError(t, err)

	data := baseDaemonSetTemplateData()
	data.PriorityClassName = "system-node-critical"
	data.ResourceRequests = resourceListToMap(corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("128Mi"),
	})
	data.ResourceLimits = resourceListToMap(corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("200m"),
		corev1.ResourceMemory: resource.MustParse("256Mi"),
	})

	var out bytes.Buffer
	require.NoError(t, tmpl.Execute(&out, data))

	require.Contains(t, out.String(), "priorityClassName: system-node-critical")
	require.Contains(t, out.String(), "requests:")
	require.Contains(t, out.String(), "cpu: 100m")
	require.Contains(t, out.String(), "memory: 128Mi")
	require.Contains(t, out.String(), "limits:")
	require.Contains(t, out.String(), "cpu: 200m")
	require.Contains(t, out.String(), "memory: 256Mi")
	// The resource claim reference for the CD daemon's dynamic
	// ResourceClaimTemplate must remain untouched alongside the new
	// requests/limits blocks under the same `resources:` field.
	require.Contains(t, out.String(), "claims:")
	require.Contains(t, out.String(), "- name: compute-domain-daemon")
}

func TestOmitsPriorityClassAndResourcesWhenUnset(t *testing.T) {
	tmpl, err := template.ParseFiles("../../templates/compute-domain-daemon.tmpl.yaml")
	require.NoError(t, err)

	data := baseDaemonSetTemplateData()

	var out bytes.Buffer
	require.NoError(t, tmpl.Execute(&out, data))

	require.NotContains(t, out.String(), "priorityClassName")
	require.NotContains(t, out.String(), "requests:")
	require.NotContains(t, out.String(), "limits:")
	// The claims block (unrelated to requests/limits) must still render.
	require.Contains(t, out.String(), "claims:")
}

func TestResourceListToMap(t *testing.T) {
	require.Nil(t, resourceListToMap(nil))
	require.Nil(t, resourceListToMap(corev1.ResourceList{}))

	got := resourceListToMap(corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("250m"),
		corev1.ResourceMemory: resource.MustParse("512Mi"),
	})
	require.Equal(t, map[string]string{
		"cpu":    "250m",
		"memory": "512Mi",
	}, got)
}
