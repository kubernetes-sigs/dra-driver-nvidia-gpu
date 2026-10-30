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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	draclient "k8s.io/dynamic-resource-allocation/client"
	"k8s.io/klog/v2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
)

const workloadTemplatePath = "../../templates/compute-domain-workload-claim-template.tmpl.yaml"

// testComputeDomain returns a ComputeDomain with one template "job" set
// through channel, or count templates "job-<i>" set through channels.
func testComputeDomain(uid string, count int) *nvapi.ComputeDomain {
	cd := &nvapi.ComputeDomain{
		ObjectMeta: metav1.ObjectMeta{Name: "cd-" + uid, Namespace: "ns-" + uid, UID: types.UID(uid)},
	}
	if count == 1 {
		cd.Spec.Channel = &nvapi.ComputeDomainChannelSpec{
			ResourceClaimTemplate: nvapi.ComputeDomainResourceClaimTemplate{Name: "job"},
		}
	}
	for i := 0; count > 1 && i < count; i++ {
		cd.Spec.Channels = append(cd.Spec.Channels, nvapi.ComputeDomainChannelEntry{
			ResourceClaimTemplate: nvapi.ComputeDomainResourceClaimTemplate{Name: fmt.Sprintf("job-%d", i)},
		})
	}
	return cd
}

func TestWorkloadResourceClaimTemplateNames(t *testing.T) {
	assert.Equal(t, []string{"job"}, workloadResourceClaimTemplateNames(testComputeDomain("a", 1)), "channel is a list of one")
	assert.Equal(t, []string{"job-0", "job-1", "job-2"}, workloadResourceClaimTemplateNames(testComputeDomain("a", 3)))
}

func renderWorkloadTemplate(t *testing.T, data *ResourceClaimTemplateTemplateData) *resourceapi.ResourceClaimTemplate {
	t.Helper()
	data.Namespace = "ns"
	data.Name = "job"
	data.Finalizer = computeDomainFinalizer
	data.ComputeDomainLabelKey = computeDomainLabelKey
	data.ComputeDomainLabelValue = "a"
	data.TargetLabelKey = computeDomainResourceClaimTemplateTargetLabelKey
	data.TargetLabelValue = computeDomainResourceClaimTemplateTargetWorkload
	data.DriverName = DriverName
	if data.ChannelConfig == nil {
		data.ChannelConfig = nvapi.DefaultComputeDomainChannelConfig()
	}
	data.ChannelConfig.DomainID = "a"
	rct, err := renderResourceClaimTemplate(workloadTemplatePath, data)
	require.NoError(t, err)
	return rct
}

func templateChannelConfig(t *testing.T, rct *resourceapi.ResourceClaimTemplate) *nvapi.ComputeDomainChannelConfig {
	t.Helper()
	require.Len(t, rct.Spec.Spec.Devices.Config, 1)
	var config nvapi.ComputeDomainChannelConfig
	require.NoError(t, json.Unmarshal(rct.Spec.Spec.Devices.Config[0].Opaque.Parameters.Raw, &config))
	return &config
}

func TestRenderWorkloadResourceClaimTemplate(t *testing.T) {
	t.Run("without reservations, the default channel as before", func(t *testing.T) {
		rct := renderWorkloadTemplate(t, &ResourceClaimTemplateTemplateData{DeviceClassName: computeDomainDefaultChannelDeviceClass})

		request := rct.Spec.Spec.Devices.Requests[0].Exactly
		assert.Equal(t, computeDomainDefaultChannelDeviceClass, request.DeviceClassName)
		assert.Empty(t, request.Selectors)
		assert.Empty(t, templateChannelConfig(t, rct).ResourceClaimTemplateName)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(rct.Spec.Spec.Devices.Config[0].Opaque.Parameters.Raw, &raw))
		assert.ElementsMatch(t, []string{"apiVersion", "kind", "domainID", "allocationMode"}, keysOf(raw), "the config is unchanged")
	})

	t.Run("with reservations, the template names itself and carries no channel", func(t *testing.T) {
		config := nvapi.DefaultComputeDomainChannelConfig()
		config.ResourceClaimTemplateName = "job"
		rct := renderWorkloadTemplate(t, &ResourceClaimTemplateTemplateData{
			DeviceClassName: computeDomainChannelDeviceClass,
			ChannelConfig:   config,
			Owner:           "uid-a",
			OwnerTaintKey:   imex.ChannelOwnerTaintKey,
		})

		request := rct.Spec.Spec.Devices.Requests[0].Exactly
		assert.Equal(t, computeDomainChannelDeviceClass, request.DeviceClassName)
		assert.Equal(t, "job", templateChannelConfig(t, rct).ResourceClaimTemplateName)
		assert.Empty(t, request.Selectors, "the DeviceClass selects the shared channel device")
		assert.Equal(t, []resourceapi.DeviceToleration{ownerToleration("uid-a")}, request.Tolerations)
	})
}

func keysOf(m map[string]any) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestWorkloadTemplateCreateKeepsTemplatesFromBeforeTheGate(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	m := NewWorkloadResourceClaimTemplateManager(&ManagerConfig{
		imexConfig: imex.Config{Mode: imex.ModeDriverManaged, Isolation: imex.IsolationIMEXDomain},
		clientsets: flags.ClientSets{Resource: draclient.New(client)},
	}, nil)
	m.templatePath = workloadTemplatePath
	require.NoError(t, addComputeDomainLabelIndexer[*resourceapi.ResourceClaimTemplate](m.informer))
	m.mutationCache = cache.NewIntegerResourceVersionMutationCache(klog.Background(), m.informer.GetStore(), m.informer.GetIndexer(), time.Minute, true)
	template := func(cd *nvapi.ComputeDomain) *resourceapi.ResourceClaimTemplate {
		rct, err := client.ResourceV1().ResourceClaimTemplates(cd.Namespace).Get(ctx, "job", metav1.GetOptions{})
		require.NoError(t, err)
		return rct
	}

	existing := testComputeDomain("a", 1)
	require.NoError(t, m.Create(ctx, existing))
	beforeGate := template(existing)

	setChannelReservationsGate(t, true)
	require.NoError(t, m.Create(ctx, existing))
	assert.Equal(t, beforeGate, template(existing), "existing ComputeDomains keep working as before, on channel 0")
	for _, action := range client.Actions() {
		assert.NotEqual(t, "delete", action.GetVerb())
	}

	created := testComputeDomain("b", 1)
	require.NoError(t, m.Create(ctx, created))
	assert.True(t, namesWorkloadTemplate(template(created)), "new ComputeDomains get their own channels")
	assert.Equal(t, computeDomainChannelDeviceClass, template(created).Spec.Spec.Devices.Requests[0].Exactly.DeviceClassName)
}

func TestWorkloadTemplateUpToDate(t *testing.T) {
	cd := testComputeDomain("a", 1)
	channel := cd.Spec.GetChannels()[0]
	newManager := func(mode imex.Mode) *WorkloadResourceClaimTemplateManager {
		return &WorkloadResourceClaimTemplateManager{BaseResourceClaimTemplateManager: &BaseResourceClaimTemplateManager{
			config:       &ManagerConfig{imexConfig: imex.Config{Mode: mode, Isolation: imex.IsolationIMEXDomain}},
			templatePath: workloadTemplatePath,
		}}
	}
	render := func(m *WorkloadResourceClaimTemplateManager, reservations bool) *resourceapi.ResourceClaimTemplate {
		rct, err := renderResourceClaimTemplate(workloadTemplatePath, m.templateData(cd, channel, reservations))
		require.NoError(t, err)
		return rct
	}
	upToDate := func(m *WorkloadResourceClaimTemplateManager, rct *resourceapi.ResourceClaimTemplate) bool {
		ok, err := m.upToDate(rct, m.templateData(cd, channel, true))
		require.NoError(t, err)
		return ok
	}

	driver := newManager(imex.ModeDriverManaged)
	beforeGate := render(driver, false)
	named := render(driver, true)

	assert.False(t, namesWorkloadTemplate(beforeGate), "the controller keeps templates from before the gate")
	assert.True(t, namesWorkloadTemplate(named))
	assert.True(t, upToDate(driver, named))

	request := named.Spec.Spec.Devices.Requests[0].Exactly
	assert.Empty(t, request.Selectors)
	assert.Equal(t, []resourceapi.DeviceToleration{ownerToleration(string(cd.UID))}, request.Tolerations, "per-ComputeDomain IMEX daemons tolerate only nodes of their own ComputeDomain")

	mutations := map[string]func(*resourceapi.ExactDeviceRequest){
		"selector": func(r *resourceapi.ExactDeviceRequest) {
			r.Selectors = append(r.Selectors, resourceapi.DeviceSelector{CEL: &resourceapi.CELDeviceSelector{Expression: "true"}})
		},
		"other DeviceClass": func(r *resourceapi.ExactDeviceRequest) { r.DeviceClassName = computeDomainDefaultChannelDeviceClass },
		"no toleration":     func(r *resourceapi.ExactDeviceRequest) { r.Tolerations = nil },
		"other owner":       func(r *resourceapi.ExactDeviceRequest) { r.Tolerations[0].Value = "other" },
		"extra toleration": func(r *resourceapi.ExactDeviceRequest) {
			r.Tolerations = append(r.Tolerations, resourceapi.DeviceToleration{Operator: resourceapi.DeviceTolerationOpExists})
		},
		"two devices": func(r *resourceapi.ExactDeviceRequest) { r.Count = 2 },
	}
	for name, mutate := range mutations {
		changed := named.DeepCopy()
		mutate(changed.Spec.Spec.Devices.Requests[0].Exactly)
		assert.False(t, upToDate(driver, changed), name)
	}
	defaulted := named.DeepCopy()
	defaulted.Spec.Spec.Devices.Requests[0].Exactly.AllocationMode = resourceapi.DeviceAllocationModeExactCount
	defaulted.Spec.Spec.Devices.Requests[0].Exactly.Count = 1
	assert.True(t, upToDate(driver, defaulted), "fields defaulted by the API server match")

	reordered := named.DeepCopy()
	reordered.Spec.Spec.Devices.Config[0].Opaque.Parameters.Raw = []byte(`{"resourceClaimTemplateName":"job","domainID":"a","kind":"ComputeDomainChannelConfig","apiVersion":"resource.nvidia.com/v1beta1"}`)
	assert.True(t, upToDate(driver, reordered), "configs are compared decoded")

	host := newManager(imex.ModeHostManaged)
	hostRequest := render(host, true).Spec.Spec.Devices.Requests[0].Exactly
	assert.Empty(t, hostRequest.Selectors)
	assert.Empty(t, hostRequest.Tolerations, "ComputeDomains share nodes in a shared IMEX domain")
}

func ownerToleration(uid string) resourceapi.DeviceToleration {
	return resourceapi.DeviceToleration{
		Key:      imex.ChannelOwnerTaintKey,
		Operator: resourceapi.DeviceTolerationOpEqual,
		Value:    uid,
		Effect:   resourceapi.DeviceTaintEffectNoSchedule,
	}
}

func setChannelReservationsGate(t testing.TB, enabled bool) {
	t.Helper()
	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.ComputeDomainChannelReservations): enabled,
	}))
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
			string(featuregates.ComputeDomainChannelReservations): false,
		}))
	})
}
