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
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	draclient "k8s.io/dynamic-resource-allocation/client"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
)

// allocatedChannelClaim returns a claim of a ComputeDomain whose channel was
// allocated on a node with the ChannelAdmitted binding condition pending.
func allocatedChannelClaim(t *testing.T, name, namespace, domainID, node, device string) *resourceapi.ResourceClaim {
	t.Helper()
	config := nvapi.DefaultComputeDomainChannelConfig()
	config.DomainID = domainID
	raw, err := json.Marshal(config)
	require.NoError(t, err)
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(name + "-uid")},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{{
						Request:                  "channel",
						Driver:                   DriverName,
						Pool:                     node,
						Device:                   device,
						BindingConditions:        []string{imex.ChannelAdmittedCondition},
						BindingFailureConditions: []string{imex.ChannelRejectedCondition},
					}},
					Config: []resourceapi.DeviceAllocationConfiguration{{
						Source:   resourceapi.AllocationConfigSourceClaim,
						Requests: []string{"channel"},
						DeviceConfiguration: resourceapi.DeviceConfiguration{
							Opaque: &resourceapi.OpaqueDeviceConfiguration{Driver: DriverName, Parameters: runtime.RawExtension{Raw: raw}},
						},
					}},
				},
			},
		},
	}
}

type admissionFixture struct {
	m      *ClaimAdmissionManager
	client *fake.Clientset
}

func newAdmissionFixture(t *testing.T, mode imex.Mode, cds ...*nvapi.ComputeDomain) *admissionFixture {
	t.Helper()
	client := fake.NewSimpleClientset()
	config := &ManagerConfig{
		imexConfig:       imex.Config{Mode: mode, Isolation: imex.IsolationIMEXDomain},
		clientsets:       flags.ClientSets{Resource: draclient.New(client), Core: client},
		deviceTaintRules: &v1DeviceTaintRuleClient{client: client.ResourceV1().DeviceTaintRules()},
	}
	getCD := func(uid string) (*nvapi.ComputeDomain, error) {
		for _, cd := range cds {
			if string(cd.UID) == uid {
				return cd, nil
			}
		}
		return nil, nil
	}
	return &admissionFixture{m: NewClaimAdmissionManager(config, getCD), client: client}
}

// add makes a claim visible to both the API (for status updates) and the
// informer cache.
func (f *admissionFixture) add(t *testing.T, claim *resourceapi.ResourceClaim) {
	t.Helper()
	_, err := f.client.ResourceV1().ResourceClaims(claim.Namespace).Create(context.Background(), claim, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, f.m.informer.GetStore().Add(claim))
}

func (f *admissionFixture) remove(t *testing.T, claim *resourceapi.ResourceClaim) {
	t.Helper()
	require.NoError(t, f.m.informer.GetStore().Delete(claim))
}

// admit runs admission and returns the condition the controller set.
func (f *admissionFixture) admit(t *testing.T, claim *resourceapi.ResourceClaim) *metav1.Condition {
	t.Helper()
	require.NoError(t, f.m.admit(context.Background(), claim))
	updated, err := f.client.ResourceV1().ResourceClaims(claim.Namespace).Get(context.Background(), claim.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, updated.Status.Devices, 1)
	status := updated.Status.Devices[0]
	assert.Equal(t, claim.Status.Allocation.Devices.Results[0].Device, status.Device)
	if c := meta.FindStatusCondition(status.Conditions, imex.ChannelAdmittedCondition); c != nil {
		return c
	}
	return meta.FindStatusCondition(status.Conditions, imex.ChannelRejectedCondition)
}

func (f *admissionFixture) nodeOwner(t *testing.T, node string) string {
	t.Helper()
	rule, err := f.client.ResourceV1().DeviceTaintRules().Get(context.Background(), nodeOwnershipRuleName(node), metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return ""
	}
	require.NoError(t, err)
	return rule.Spec.Taint.Value
}

func cdInNamespace(uid, namespace string, channels ...int) *nvapi.ComputeDomain {
	cd := testComputeDomain(uid, len(channels), channels...)
	cd.Namespace = namespace
	return cd
}

func TestClaimAdmissionDriverManaged(t *testing.T) {
	setChannelReservationsGate(t, true)
	cdA := cdInNamespace("cd-a", "team-a", 0, 1)
	cdB := cdInNamespace("cd-b", "team-b", 0, 1)
	f := newAdmissionFixture(t, imex.ModeDriverManaged, cdA, cdB)

	// The first claim on a free node takes it for its ComputeDomain.
	a0 := allocatedChannelClaim(t, "a0", "team-a", "cd-a", "node-1", "channel-0")
	f.add(t, a0)
	c := f.admit(t, a0)
	assert.Equal(t, imex.ChannelAdmittedCondition, c.Type)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, "cd-a", f.nodeOwner(t, "node-1"))

	rule, err := f.client.ResourceV1().DeviceTaintRules().Get(context.Background(), nodeOwnershipRuleName("node-1"), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, DriverName, *rule.Spec.DeviceSelector.Driver)
	assert.Equal(t, "node-1", *rule.Spec.DeviceSelector.Pool)
	assert.Nil(t, rule.Spec.DeviceSelector.Device, "all of the node's devices are tainted")
	assert.Equal(t, resourceapi.DeviceTaintEffectNoSchedule, rule.Spec.Taint.Effect)

	// Another ComputeDomain is rejected on that node, even on a channel ID
	// the owner does not use there.
	b1 := allocatedChannelClaim(t, "b1", "team-b", "cd-b", "node-1", "channel-1")
	f.add(t, b1)
	c = f.admit(t, b1)
	assert.Equal(t, imex.ChannelRejectedCondition, c.Type)
	assert.Equal(t, "NodeOwnedByOtherComputeDomain", c.Reason)

	// ... but admitted on another node, reusing the same channel ID.
	b0 := allocatedChannelClaim(t, "b0", "team-b", "cd-b", "node-2", "channel-0")
	f.add(t, b0)
	assert.Equal(t, imex.ChannelAdmittedCondition, f.admit(t, b0).Type)

	// More claims of the owner, from another of its templates, are admitted.
	a1 := allocatedChannelClaim(t, "a1", "team-a", "cd-a", "node-1", "channel-1")
	f.add(t, a1)
	assert.Equal(t, imex.ChannelAdmittedCondition, f.admit(t, a1).Type)

	// The node stays owned while any claim of the owner remains on it.
	f.remove(t, a0)
	require.NoError(t, f.m.releaseNode(context.Background(), "node-1"))
	assert.Equal(t, "cd-a", f.nodeOwner(t, "node-1"))

	f.remove(t, a1)
	f.remove(t, b1) // the rejected claim gets reallocated elsewhere
	require.NoError(t, f.m.releaseNode(context.Background(), "node-1"))
	assert.Empty(t, f.nodeOwner(t, "node-1"), "the node is free again")

	// Now cd-b can take node-1.
	b1again := allocatedChannelClaim(t, "b1-retry", "team-b", "cd-b", "node-1", "channel-1")
	f.add(t, b1again)
	assert.Equal(t, imex.ChannelAdmittedCondition, f.admit(t, b1again).Type)
	assert.Equal(t, "cd-b", f.nodeOwner(t, "node-1"))
}

func TestClaimAdmissionHostManaged(t *testing.T) {
	setChannelReservationsGate(t, true)
	cdA := cdInNamespace("cd-a", "team-a", 3)
	cdB := cdInNamespace("cd-b", "team-b", 4)
	f := newAdmissionFixture(t, imex.ModeHostManaged, cdA, cdB)

	tests := map[string]struct {
		claim  *resourceapi.ResourceClaim
		want   string
		reason string
	}{
		"reserved channel": {
			claim: allocatedChannelClaim(t, "a", "team-a", "cd-a", "node-1", "channel-3"),
			want:  imex.ChannelAdmittedCondition,
		},
		"two ComputeDomains on one node with their own channels": {
			claim: allocatedChannelClaim(t, "b", "team-b", "cd-b", "node-1", "channel-4"),
			want:  imex.ChannelAdmittedCondition,
		},
		"another ComputeDomain's channel": {
			claim:  allocatedChannelClaim(t, "b-steal", "team-b", "cd-b", "node-1", "channel-3"),
			want:   imex.ChannelRejectedCondition,
			reason: "ChannelNotReserved",
		},
		"claim from another namespace": {
			claim:  allocatedChannelClaim(t, "x", "team-x", "cd-a", "node-1", "channel-3"),
			want:   imex.ChannelRejectedCondition,
			reason: "NamespaceMismatch",
		},
		"unknown ComputeDomain": {
			claim:  allocatedChannelClaim(t, "gone", "team-a", "cd-gone", "node-1", "channel-5"),
			want:   imex.ChannelRejectedCondition,
			reason: "ComputeDomainNotFound",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			f.add(t, tc.claim)

			c := f.admit(t, tc.claim)

			assert.Equal(t, tc.want, c.Type)
			if tc.reason != "" {
				assert.Equal(t, tc.reason, c.Reason)
			}
		})
	}
	assert.Empty(t, f.nodeOwner(t, "node-1"), "host-managed IMEX does not claim nodes")
}

func TestPendingChannelResults(t *testing.T) {
	claim := allocatedChannelClaim(t, "a", "ns", "cd-a", "node-1", "channel-0")
	assert.Len(t, pendingChannelResults(claim), 1)

	result := claim.Status.Allocation.Devices.Results[0]
	setChannelAdmissionCondition(claim, &result, admitDecision())
	assert.Empty(t, pendingChannelResults(claim), "decided channels are not admitted again")

	withoutConditions := allocatedChannelClaim(t, "b", "ns", "cd-a", "node-1", "channel-0")
	withoutConditions.Status.Allocation.Devices.Results[0].BindingConditions = nil
	assert.Empty(t, pendingChannelResults(withoutConditions), "only channels published with the binding condition are admitted")
}

func TestReleasedChannels(t *testing.T) {
	allocated := allocatedChannelClaim(t, "a", "ns", "cd-a", "node-1", "channel-0")
	deallocated := allocated.DeepCopy()
	deallocated.Status.Allocation = nil
	deleting := allocated.DeepCopy()
	deleting.DeletionTimestamp = &metav1.Time{}

	assert.True(t, releasedChannels(allocated, deallocated))
	assert.True(t, releasedChannels(allocated, deleting))
	assert.False(t, releasedChannels(allocated, allocated), "periodic resyncs do not trigger release checks")
	assert.False(t, releasedChannels(deallocated, allocated))
}

func TestNodeOwnershipRuleName(t *testing.T) {
	assert.Equal(t, "computedomain-node-node-1", nodeOwnershipRuleName("node-1"))
	long := nodeOwnershipRuleName(strings.Repeat("n", 250))
	assert.LessOrEqual(t, len(long), maxObjectNameLength)
	assert.True(t, strings.HasPrefix(long, "computedomain-node-"))
}

func TestRenderDaemonResourceClaimTemplateToleration(t *testing.T) {
	daemonConfig := nvapi.DefaultComputeDomainDaemonConfig()
	daemonConfig.DomainID = "a"
	rct, err := renderResourceClaimTemplate("../../templates/compute-domain-daemon-claim-template.tmpl.yaml", &ResourceClaimTemplateTemplateData{
		Namespace:               "driver",
		Name:                    "computedomain-daemon-a",
		Finalizer:               computeDomainFinalizer,
		ComputeDomainLabelKey:   computeDomainLabelKey,
		ComputeDomainLabelValue: "a",
		TargetLabelKey:          computeDomainResourceClaimTemplateTargetLabelKey,
		TargetLabelValue:        computeDomainResourceClaimTemplateTargetDaemon,
		DeviceClassName:         computeDomainDaemonDeviceClass,
		DriverName:              DriverName,
		DaemonConfig:            daemonConfig,
		OwnerTolerationKey:      imex.ChannelOwnerTaintKey,
		OwnerTolerationValue:    "a",
	})
	require.NoError(t, err)

	tolerations := rct.Spec.Spec.Devices.Requests[0].Exactly.Tolerations
	require.Len(t, tolerations, 1)
	assert.Equal(t, "a", tolerations[0].Value)
}

func TestChannelDomainIDPrecedence(t *testing.T) {
	claim := allocatedChannelClaim(t, "a", "ns", "cd-claim", "node-1", "channel-0")
	classConfig := claim.Status.Allocation.Devices.Config[0].DeepCopy()
	classConfig.Source = resourceapi.AllocationConfigSourceClass
	classConfig.Opaque.Parameters.Raw = []byte(`{"kind":"ComputeDomainChannelConfig","domainID":"cd-class"}`)
	// Listed after the claim config, but claim configs take precedence.
	claim.Status.Allocation.Devices.Config = append(claim.Status.Allocation.Devices.Config, *classConfig)

	assert.Equal(t, "cd-claim", channelDomainID(claim, "channel"))
	assert.Empty(t, channelDomainID(claim, "other-request"))
}

func TestClaimAdmissionWithV1beta2DeviceTaintRules(t *testing.T) {
	setChannelReservationsGate(t, true)
	// Kubernetes 1.36: node ownership works through v1beta2 DeviceTaintRules
	// while ResourceClaim status updates stay on v1.
	cdA := cdInNamespace("cd-a", "team-a", 0)
	cdB := cdInNamespace("cd-b", "team-b", 0)
	f := newAdmissionFixture(t, imex.ModeDriverManaged, cdA, cdB)
	f.m.config.deviceTaintRules = &v1beta2DeviceTaintRuleClient{client: f.client.ResourceV1beta2().DeviceTaintRules()}

	a := allocatedChannelClaim(t, "a", "team-a", "cd-a", "node-1", "channel-0")
	f.add(t, a)
	assert.Equal(t, imex.ChannelAdmittedCondition, f.admit(t, a).Type)

	b := allocatedChannelClaim(t, "b", "team-b", "cd-b", "node-1", "channel-0")
	f.add(t, b)
	assert.Equal(t, imex.ChannelRejectedCondition, f.admit(t, b).Type)

	rule, err := f.client.ResourceV1beta2().DeviceTaintRules().Get(context.Background(), nodeOwnershipRuleName("node-1"), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "cd-a", rule.Spec.Taint.Value)
}

func setChannelReservationsGate(t *testing.T, enabled bool) {
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

func TestClaimAdmissionGateDisabled(t *testing.T) {
	// Kubelet plugins may enable the gate before the controller: their
	// channels are admitted rather than left to time out.
	f := newAdmissionFixture(t, imex.ModeDriverManaged)
	claim := allocatedChannelClaim(t, "a", "team-a", "cd-unknown", "node-1", "channel-0")
	f.add(t, claim)

	c := f.admit(t, claim)

	assert.Equal(t, imex.ChannelAdmittedCondition, c.Type)
	assert.Empty(t, f.nodeOwner(t, "node-1"), "no node ownership without the gate")
}

func TestSweepDeviceTaintRulesGateDisabled(t *testing.T) {
	// Leftover rules would otherwise keep tainting nodes and channels.
	f := newAdmissionFixture(t, imex.ModeDriverManaged)

	require.NoError(t, f.m.sweepDeviceTaintRules(context.Background()))

	var deleted bool
	for _, action := range f.client.Actions() {
		if dc, ok := action.(k8stesting.DeleteCollectionAction); ok && dc.GetResource().Resource == "devicetaintrules" {
			assert.Equal(t, deviceTaintRuleKindLabelKey, dc.GetListRestrictions().Labels.String())
			deleted = true
		}
	}
	assert.True(t, deleted)
}

func TestClaimAdmissionRejectsUnattributableClaims(t *testing.T) {
	setChannelReservationsGate(t, true)
	f := newAdmissionFixture(t, imex.ModeDriverManaged, cdInNamespace("cd-a", "team-a", 0))
	owner := allocatedChannelClaim(t, "a", "team-a", "cd-a", "node-1", "channel-0")
	f.add(t, owner)
	f.admit(t, owner)

	invalid := allocatedChannelClaim(t, "bad-cfg", "team-a", "not-needed-for-test", "node-1", "channel-0")
	invalid.Status.Allocation.Devices.Config[0].Opaque.Parameters.Raw = []byte(`{"apiVersion":"resource.nvidia.com/v1beta1","kind":"ComputeDomainChannelConfig","domainID":"not-needed-for-test","unexpectedField":true}`)
	unknown := allocatedChannelClaim(t, "unknown-cd", "team-a", "not-needed-for-test", "node-1", "channel-0")
	noConfig := allocatedChannelClaim(t, "no-cfg", "team-a", "cd-a", "node-1", "channel-0")
	noConfig.Status.Allocation.Devices.Config = nil

	tests := []struct {
		claim   *resourceapi.ResourceClaim
		reason  string
		message string
	}{
		{invalid, "InvalidConfig", `unknown field "unexpectedField"`},
		{unknown, "ComputeDomainNotFound", "not-needed-for-test"},
		{noConfig, "NoComputeDomain", "no ComputeDomainChannelConfig"},
	}
	for _, tc := range tests {
		f.add(t, tc.claim)
		c := f.admit(t, tc.claim)
		assert.Equal(t, imex.ChannelRejectedCondition, c.Type, tc.claim.Name)
		assert.Equal(t, tc.reason, c.Reason, tc.claim.Name)
		assert.Contains(t, c.Message, tc.message, tc.claim.Name)
	}
	assert.Equal(t, "cd-a", f.nodeOwner(t, "node-1"))
}
