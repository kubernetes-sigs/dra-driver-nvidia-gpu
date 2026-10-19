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
	"fmt"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	resourcev1beta2 "k8s.io/api/resource/v1beta2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
)

func clientServing(t *testing.T, groupVersions ...string) *fake.Clientset {
	t.Helper()
	client := fake.NewSimpleClientset()
	discovery, ok := client.Discovery().(*fakediscovery.FakeDiscovery)
	require.True(t, ok)
	for _, gv := range groupVersions {
		discovery.Resources = append(discovery.Resources, &metav1.APIResourceList{
			GroupVersion: gv,
			APIResources: []metav1.APIResource{{Name: "devicetaintrules"}, {Name: "resourceclaims"}},
		})
	}
	return client
}

func TestNewDeviceTaintRuleClient(t *testing.T) {
	t.Run("prefers v1 (Kubernetes 1.37+)", func(t *testing.T) {
		c, err := newDeviceTaintRuleClient(clientServing(t, "resource.k8s.io/v1", "resource.k8s.io/v1beta2"))
		require.NoError(t, err)
		assert.IsType(t, &v1DeviceTaintRuleClient{}, c)
	})

	t.Run("falls back to v1beta2 (Kubernetes 1.36)", func(t *testing.T) {
		c, err := newDeviceTaintRuleClient(clientServing(t, "resource.k8s.io/v1beta2"))
		require.NoError(t, err)
		assert.IsType(t, &v1beta2DeviceTaintRuleClient{}, c)
	})

	t.Run("fails without DeviceTaintRules", func(t *testing.T) {
		_, err := newDeviceTaintRuleClient(clientServing(t))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "DRADeviceTaintRules")
	})
}

func TestV1beta2DeviceTaintRuleClient(t *testing.T) {
	client := fake.NewSimpleClientset()
	c := &v1beta2DeviceTaintRuleClient{client: client.ResourceV1beta2().DeviceTaintRules()}
	rule := &resourceapi.DeviceTaintRule{
		ObjectMeta: metav1.ObjectMeta{Name: "computedomain-node-node-1", Labels: map[string]string{deviceTaintRuleKindLabelKey: deviceTaintRuleKindChannel}},
		Spec: resourceapi.DeviceTaintRuleSpec{
			DeviceSelector: &resourceapi.DeviceTaintSelector{Driver: ptr.To(DriverName), Pool: ptr.To("node-1")},
			Taint:          resourceapi.DeviceTaint{Key: imex.ChannelOwnerTaintKey, Value: "cd-a", Effect: resourceapi.DeviceTaintEffectNoSchedule},
		},
	}

	_, err := c.Create(context.Background(), rule)
	require.NoError(t, err)

	// Stored as v1beta2 ...
	stored, err := client.ResourceV1beta2().DeviceTaintRules().Get(context.Background(), rule.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, resourcev1beta2.DeviceTaintEffectNoSchedule, stored.Spec.Taint.Effect)
	assert.Equal(t, "node-1", *stored.Spec.DeviceSelector.Pool)

	// ... and read back as v1.
	got, err := c.Get(context.Background(), rule.Name)
	require.NoError(t, err)
	assert.Equal(t, rule.Spec, got.Spec)

	rules, err := c.List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, rule.Spec, rules[0].Spec)

	require.NoError(t, c.Delete(context.Background(), rule.Name, metav1.DeleteOptions{}))
	_, err = c.Get(context.Background(), rule.Name)
	require.Error(t, err)
}

func TestEnsureTaintRules(t *testing.T) {
	m, client := testReservationManagerWithClient(t)
	m.config.deviceTaintRules = &v1DeviceTaintRuleClient{client: client.ResourceV1().DeviceTaintRules()}
	cd := testComputeDomain("a", 2, 3, 5)

	require.NoError(t, m.EnsureChannelTaintRules(context.Background(), cd))

	for _, channel := range []int{3, 5} {
		rule, err := client.ResourceV1().DeviceTaintRules().Get(context.Background(), channelTaintRuleName("a", channel), metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, DriverName, *rule.Spec.DeviceSelector.Driver)
		assert.Equal(t, fmt.Sprintf("channel-%d", channel), *rule.Spec.DeviceSelector.Device)
		assert.Equal(t, imex.ChannelOwnerTaintKey, rule.Spec.Taint.Key)
		assert.Equal(t, "a", rule.Spec.Taint.Value)
		assert.Equal(t, resourceapi.DeviceTaintEffectNoSchedule, rule.Spec.Taint.Effect)
		assert.Equal(t, "a", rule.Labels[computeDomainLabelKey])
	}

	// Reconciles are idempotent and restore deleted rules.
	require.NoError(t, client.ResourceV1().DeviceTaintRules().Delete(context.Background(), channelTaintRuleName("a", 3), metav1.DeleteOptions{}))
	require.NoError(t, m.EnsureChannelTaintRules(context.Background(), cd))
	_, err := client.ResourceV1().DeviceTaintRules().Get(context.Background(), channelTaintRuleName("a", 3), metav1.GetOptions{})
	require.NoError(t, err)

	// Deletion removes all of the ComputeDomain's rules by label.
	require.NoError(t, m.DeleteTaintRules(context.Background(), "a"))
	var deleted bool
	for _, action := range client.Actions() {
		if dc, ok := action.(k8stesting.DeleteCollectionAction); ok && dc.GetResource().Resource == "devicetaintrules" {
			assert.Equal(t, computeDomainLabelKey+"=a", dc.GetListRestrictions().Labels.String())
			deleted = true
		}
	}
	assert.True(t, deleted)
}

func TestRenderWorkloadResourceClaimTemplateToleration(t *testing.T) {
	rct := renderWorkloadTemplate(t, &ResourceClaimTemplateTemplateData{
		DeviceClassName:      computeDomainChannelDeviceClass,
		ChannelSelector:      channelSelectorExpression(7),
		OwnerTolerationKey:   imex.ChannelOwnerTaintKey,
		OwnerTolerationValue: "a",
	})

	tolerations := rct.Spec.Spec.Devices.Requests[0].Exactly.Tolerations
	require.Len(t, tolerations, 1)
	assert.Equal(t, resourceapi.DeviceToleration{
		Key:      imex.ChannelOwnerTaintKey,
		Operator: resourceapi.DeviceTolerationOpEqual,
		Value:    "a",
		Effect:   resourceapi.DeviceTaintEffectNoSchedule,
	}, tolerations[0])
}

func TestEnsureChannelTaintRulesSkipsSharedChannel(t *testing.T) {
	m, client := testReservationManagerWithClient(t)
	m.config.deviceTaintRules = &v1DeviceTaintRuleClient{client: client.ResourceV1().DeviceTaintRules()}

	require.NoError(t, m.EnsureChannelTaintRules(context.Background(), testComputeDomain("legacy", 1, legacySharedChannel)))

	rules, err := client.ResourceV1().DeviceTaintRules().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, rules.Items, "legacy ComputeDomains keep sharing channel 0")
}

func TestHostManagedTemplateRequiresToleration(t *testing.T) {
	m := &WorkloadResourceClaimTemplateManager{BaseResourceClaimTemplateManager: &BaseResourceClaimTemplateManager{
		config: &ManagerConfig{imexConfig: imex.Config{Mode: imex.ModeHostManaged, Isolation: imex.IsolationIMEXDomain}},
	}}
	cd := testComputeDomain("a", 1, 3)
	untolerated := renderWorkloadTemplate(t, &ResourceClaimTemplateTemplateData{
		DeviceClassName: computeDomainChannelDeviceClass,
		ChannelSelector: channelSelectorExpression(3),
	})
	tolerated := renderWorkloadTemplate(t, &ResourceClaimTemplateTemplateData{
		DeviceClassName:      computeDomainChannelDeviceClass,
		ChannelSelector:      channelSelectorExpression(3),
		OwnerTolerationKey:   imex.ChannelOwnerTaintKey,
		OwnerTolerationValue: "a",
	})

	assert.False(t, m.upToDate(untolerated, cd, 3), "templates without the owner toleration are replaced")
	assert.True(t, m.upToDate(tolerated, cd, 3))

	for name, mutate := range map[string]func(*resourceapi.DeviceToleration){
		"operator": func(t *resourceapi.DeviceToleration) { t.Operator = resourceapi.DeviceTolerationOpExists },
		"effect":   func(t *resourceapi.DeviceToleration) { t.Effect = resourceapi.DeviceTaintEffectNoExecute },
	} {
		other := tolerated.DeepCopy()
		mutate(&other.Spec.Spec.Devices.Requests[0].Exactly.Tolerations[0])
		assert.False(t, m.upToDate(other, cd, 3), "a toleration with another %s is replaced", name)
	}
}
