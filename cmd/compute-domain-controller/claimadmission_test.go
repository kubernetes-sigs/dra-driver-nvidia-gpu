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
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	draclient "k8s.io/dynamic-resource-allocation/client"
	"k8s.io/utils/ptr"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
	nvfake "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/clientset/versioned/fake"
)

// allocatedChannelClaim returns a claim of a workload template of a
// ComputeDomain allocated the shared channel device on a node, with the
// ChannelAdmitted binding condition pending. An empty template is one created
// before channel reservations.
func allocatedChannelClaim(t testing.TB, name, namespace, domainID, node, template string) *resourceapi.ResourceClaim {
	t.Helper()
	claim := legacyChannelClaim(t, name, namespace, domainID, node, imex.ChannelDeviceName)
	result := &claim.Status.Allocation.Devices.Results[0]
	result.ShareID = ptr.To(types.UID(name + "-share"))
	config := nvapi.DefaultComputeDomainChannelConfig()
	config.DomainID = domainID
	config.ResourceClaimTemplateName = template
	raw, err := json.Marshal(config)
	require.NoError(t, err)
	claim.Status.Allocation.Devices.Config[0].Opaque.Parameters.Raw = raw
	return claim
}

// legacyChannelClaim returns a claim allocated a per-channel device, as
// published without channel reservations.
func legacyChannelClaim(t testing.TB, name, namespace, domainID, node, device string) *resourceapi.ResourceClaim {
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

func newAdmissionFixture(t testing.TB, mode imex.Mode, cds ...*nvapi.ComputeDomain) *admissionFixture {
	t.Helper()
	client := fake.NewSimpleClientset()
	config := &ManagerConfig{
		imexConfig:      imex.Config{Mode: mode, Isolation: imex.IsolationIMEXDomain},
		imexMaxChannels: imex.DefaultMaxChannels,
		clientsets:      flags.ClientSets{Resource: draclient.New(client), Core: client},
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
// informer cache, with the nodes of its channels in the API.
func (f *admissionFixture) add(t testing.TB, claim *resourceapi.ResourceClaim) {
	t.Helper()
	_, err := f.client.ResourceV1().ResourceClaims(claim.Namespace).Create(context.Background(), claim, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, f.m.informer.GetStore().Add(claim))
	for _, result := range channelResults(claim) {
		f.node(t, result.Pool)
	}
}

// node returns a node from the API, created if missing.
func (f *admissionFixture) node(t testing.TB, name string) *corev1.Node {
	t.Helper()
	ctx := context.Background()
	node, err := f.client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		node, err = f.client.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	}
	require.NoError(t, err)
	return node
}

// syncNode copies a node from the API into the cache of labeled nodes, like
// the informer.
func (f *admissionFixture) syncNode(t testing.TB, name string) {
	t.Helper()
	node := f.node(t, name)
	if node.Labels[computeDomainLabelKey] == "" {
		require.NoError(t, f.m.nodeInformer.GetStore().Delete(node))
		return
	}
	require.NoError(t, f.m.nodeInformer.GetStore().Add(node))
}

// clique publishes the NVLink clique of a node on its channel device.
func (f *admissionFixture) clique(t testing.TB, node, cliqueID string) {
	t.Helper()
	slice := &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: node + "-slice"},
		Spec: resourceapi.ResourceSliceSpec{
			Driver:   DriverName,
			NodeName: ptr.To(node),
			Pool:     resourceapi.ResourcePool{Name: node},
			Devices: []resourceapi.Device{{
				Name:       imex.ChannelDeviceName,
				Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{imex.ChannelCliqueAttribute: {StringValue: ptr.To(cliqueID)}},
			}},
		},
	}
	require.NoError(t, f.m.sliceInformer.GetStore().Add(slice))
}

// allocation returns the channel the controller assigned to a claim.
func (f *admissionFixture) allocation(t testing.TB, namespace, name string) imex.ChannelAllocation {
	t.Helper()
	claim, err := f.client.ResourceV1().ResourceClaims(namespace).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, claim.Status.Devices, 1)
	allocation, err := imex.DecodeChannelAllocation(claim.Status.Devices[0].Data)
	require.NoError(t, err)
	require.NotNil(t, allocation)
	return *allocation
}

// owner returns the owner of a node in the API.
func (f *admissionFixture) owner(t testing.TB, name string) string {
	t.Helper()
	return f.node(t, name).Labels[computeDomainLabelKey]
}

// remove deletes a claim from the cache, like a deleted or deallocated claim.
func (f *admissionFixture) remove(t testing.TB, claim *resourceapi.ResourceClaim) {
	t.Helper()
	require.NoError(t, f.m.informer.GetStore().Delete(claim))
	f.m.forgetInflight(claim.UID)
}

// sync copies a claim's status from the API into the cache, like the
// informer, which also forgets its in-process admission.
func (f *admissionFixture) sync(t testing.TB, claim *resourceapi.ResourceClaim) {
	t.Helper()
	updated, err := f.client.ResourceV1().ResourceClaims(claim.Namespace).Get(context.Background(), claim.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, f.m.informer.GetStore().Update(updated))
	f.m.forgetDecided(updated)
}

// admit runs admission and returns the condition the controller set.
func (f *admissionFixture) admit(t testing.TB, claim *resourceapi.ResourceClaim) *metav1.Condition {
	t.Helper()
	require.NoError(t, f.m.admitKey(context.Background(), claim.Namespace+"/"+claim.Name))
	return f.condition(t, claim)
}

func (f *admissionFixture) condition(t testing.TB, claim *resourceapi.ResourceClaim) *metav1.Condition {
	t.Helper()
	updated, err := f.client.ResourceV1().ResourceClaims(claim.Namespace).Get(context.Background(), claim.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, updated.Status.Devices, 1)
	status := updated.Status.Devices[0]
	assert.Equal(t, claim.Status.Allocation.Devices.Results[0].Device, status.Device)
	assert.Equal(t, (*string)(claim.Status.Allocation.Devices.Results[0].ShareID), status.ShareID)
	if c := meta.FindStatusCondition(status.Conditions, imex.ChannelAdmittedCondition); c != nil {
		return c
	}
	return meta.FindStatusCondition(status.Conditions, imex.ChannelRejectedCondition)
}

// labelNode sets the node label naming the ComputeDomain whose IMEX daemon
// runs on the node, in the API and the cache.
func (f *admissionFixture) labelNode(t testing.TB, name, domainID string) {
	t.Helper()
	node := f.node(t, name).DeepCopy()
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	node.Labels[computeDomainLabelKey] = domainID
	_, err := f.client.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.syncNode(t, name)
}

// unlabelNode removes the node label, like the kubelet plugin after the last
// claim of the ComputeDomain is unprepared.
func (f *admissionFixture) unlabelNode(t testing.TB, name string) {
	t.Helper()
	node := f.node(t, name).DeepCopy()
	delete(node.Labels, computeDomainLabelKey)
	_, err := f.client.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.syncNode(t, name)
}

func cdInNamespace(uid, namespace string) *nvapi.ComputeDomain {
	cd := testComputeDomain(uid, 1)
	cd.Namespace = namespace
	return cd
}

func TestClaimAdmissionDriverManaged(t *testing.T) {
	setChannelReservationsGate(t, true)
	cdA := cdInNamespace("cd-a", "team-a")
	cdB := cdInNamespace("cd-b", "team-b")
	f := newAdmissionFixture(t, imex.ModeDriverManaged, cdA, cdB)

	// The first claim on a free node is admitted, and the node is labeled
	// for cd-a, so the scheduler skips it for other ComputeDomains.
	a0 := allocatedChannelClaim(t, "a0", "team-a", "cd-a", "node-1", "job")
	f.add(t, a0)
	c := f.admit(t, a0)
	assert.Equal(t, imex.ChannelAdmittedCondition, c.Type)
	assert.Equal(t, metav1.ConditionTrue, c.Status)
	assert.Equal(t, "cd-a", f.owner(t, "node-1"))

	// Another ComputeDomain is rejected on that node, also before the cache
	// shows the admission.
	b1 := allocatedChannelClaim(t, "b1", "team-b", "cd-b", "node-1", "job")
	f.add(t, b1)
	c = f.admit(t, b1)
	assert.Equal(t, imex.ChannelRejectedCondition, c.Type)
	assert.Equal(t, "NodeUsedByOtherComputeDomain", c.Reason)
	assert.Contains(t, c.Message, "cd-a")

	// ... and once the cache shows it.
	f.sync(t, a0)
	b1retry := allocatedChannelClaim(t, "b1-retry", "team-b", "cd-b", "node-1", "job")
	f.add(t, b1retry)
	assert.Equal(t, imex.ChannelRejectedCondition, f.admit(t, b1retry).Type)

	// It is admitted on another node, reusing the same channel.
	b0 := allocatedChannelClaim(t, "b0", "team-b", "cd-b", "node-2", "job")
	f.add(t, b0)
	assert.Equal(t, imex.ChannelAdmittedCondition, f.admit(t, b0).Type)

	// More claims of the user, of the same or another template, are admitted.
	a0second := allocatedChannelClaim(t, "a0-second", "team-a", "cd-a", "node-1", "job")
	f.add(t, a0second)
	assert.Equal(t, imex.ChannelAdmittedCondition, f.admit(t, a0second).Type)
	a1 := allocatedChannelClaim(t, "a1", "team-a", "cd-a", "node-1", "job")
	f.add(t, a1)
	assert.Equal(t, imex.ChannelAdmittedCondition, f.admit(t, a1).Type)

	// The node is used while any claim of cd-a remains on it.
	f.sync(t, a0second)
	f.sync(t, a1)
	f.remove(t, a0)
	f.remove(t, a0second)
	another := allocatedChannelClaim(t, "b1-third", "team-b", "cd-b", "node-1", "job")
	f.add(t, another)
	assert.Equal(t, imex.ChannelRejectedCondition, f.admit(t, another).Type)

	// Then it is released, as rejected claims of cd-b do not hold it.
	f.remove(t, a1)
	f.syncNode(t, "node-1")
	require.NoError(t, f.m.releaseNode(context.Background(), "node-1"))
	assert.Empty(t, f.owner(t, "node-1"))
	f.syncNode(t, "node-1")
	free := allocatedChannelClaim(t, "b1-free", "team-b", "cd-b", "node-1", "job")
	f.add(t, free)
	assert.Equal(t, imex.ChannelAdmittedCondition, f.admit(t, free).Type)
}

func TestClaimAdmissionWaitsForIMEXDaemonTeardown(t *testing.T) {
	setChannelReservationsGate(t, true)
	f := newAdmissionFixture(t, imex.ModeDriverManaged, cdInNamespace("cd-a", "team-a"), cdInNamespace("cd-b", "team-b"))

	// A force-deleted pod's claim is gone, but its IMEX daemon still runs
	// until the kubelet plugin unprepares it and removes the node label.
	f.labelNode(t, "node-1", "cd-a")
	b := allocatedChannelClaim(t, "b", "team-b", "cd-b", "node-1", "job")
	f.add(t, b)
	c := f.admit(t, b)
	assert.Equal(t, imex.ChannelRejectedCondition, c.Type)
	assert.Equal(t, "NodeUsedByOtherComputeDomain", c.Reason)

	f.unlabelNode(t, "node-1")
	retry := allocatedChannelClaim(t, "b-retry", "team-b", "cd-b", "node-1", "job")
	f.add(t, retry)
	assert.Equal(t, imex.ChannelAdmittedCondition, f.admit(t, retry).Type)

	// Its own label does not hold it back.
	f.labelNode(t, "node-2", "cd-b")
	own := allocatedChannelClaim(t, "b-own", "team-b", "cd-b", "node-2", "job")
	f.add(t, own)
	assert.Equal(t, imex.ChannelAdmittedCondition, f.admit(t, own).Type)
}

func TestClaimAdmissionCountsChannelsInUse(t *testing.T) {
	setChannelReservationsGate(t, true)
	f := newAdmissionFixture(t, imex.ModeDriverManaged, cdInNamespace("cd-a", "team-a"), cdInNamespace("cd-b", "team-b"))

	// Undecided claims of another ComputeDomain do not hold a node.
	pending := allocatedChannelClaim(t, "a-pending", "team-a", "cd-a", "node-1", "job")
	require.NoError(t, f.m.informer.GetStore().Add(pending))
	b := allocatedChannelClaim(t, "b", "team-b", "cd-b", "node-1", "job")
	f.add(t, b)
	assert.Equal(t, imex.ChannelAdmittedCondition, f.admit(t, b).Type)

	// Channels allocated without waiting for admission, by a kubelet plugin
	// without the gate, do.
	unconditional := legacyChannelClaim(t, "a-legacy", "team-a", "cd-a", "node-2", "channel-0")
	unconditional.Status.Allocation.Devices.Results[0].BindingConditions = nil
	require.NoError(t, f.m.informer.GetStore().Add(unconditional))
	b2 := allocatedChannelClaim(t, "b2", "team-b", "cd-b", "node-2", "job")
	f.add(t, b2)
	assert.Equal(t, imex.ChannelRejectedCondition, f.admit(t, b2).Type)
}

// TestClaimAdmissionRace has the first claims of many ComputeDomains contend
// for one free node at once.
func TestClaimAdmissionRace(t *testing.T) {
	setChannelReservationsGate(t, true)
	const computeDomains = 32
	var cds []*nvapi.ComputeDomain
	for i := range computeDomains {
		cds = append(cds, cdInNamespace(fmt.Sprintf("cd-%d", i), fmt.Sprintf("ns-%d", i)))
	}
	f := newAdmissionFixture(t, imex.ModeDriverManaged, cds...)
	var claims []*resourceapi.ResourceClaim
	for _, cd := range cds {
		claim := allocatedChannelClaim(t, "claim-"+string(cd.UID), cd.Namespace, string(cd.UID), "node-1", "job")
		f.add(t, claim)
		claims = append(claims, claim)
	}

	var wg sync.WaitGroup
	for _, claim := range claims {
		wg.Go(func() {
			assert.NoError(t, f.m.admitKey(context.Background(), claim.Namespace+"/"+claim.Name))
		})
	}
	wg.Wait()

	admitted := 0
	for _, claim := range claims {
		if f.condition(t, claim).Type == imex.ChannelAdmittedCondition {
			admitted++
		}
	}
	assert.Equal(t, 1, admitted, "one ComputeDomain gets the node")
}

func TestClaimAdmissionClusterScope(t *testing.T) {
	setChannelReservationsGate(t, true)
	f := newAdmissionFixture(t, imex.ModeHostManaged, cdInNamespace("cd-a", "team-a"), cdInNamespace("cd-b", "team-b"))
	f.clique(t, "node-1", "clique-1")

	tests := map[string]struct {
		claim  *resourceapi.ResourceClaim
		want   string
		reason string
	}{
		"a ComputeDomain": {
			claim: allocatedChannelClaim(t, "a", "team-a", "cd-a", "node-1", "job"),
			want:  imex.ChannelAdmittedCondition,
		},
		"two ComputeDomains on one node": {
			claim: allocatedChannelClaim(t, "b", "team-b", "cd-b", "node-1", "job"),
			want:  imex.ChannelAdmittedCondition,
		},
		"claim from another namespace": {
			claim:  allocatedChannelClaim(t, "x", "team-x", "cd-a", "node-1", "job"),
			want:   imex.ChannelRejectedCondition,
			reason: "NamespaceMismatch",
		},
		"unknown ComputeDomain": {
			claim:  allocatedChannelClaim(t, "gone", "team-a", "cd-gone", "node-1", "job"),
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
	assert.Empty(t, f.m.inflight, "nodes are not owned in a shared IMEX domain")
	assert.NotEqual(t, f.allocation(t, "team-a", "a").Channel, f.allocation(t, "team-b", "b").Channel, "ComputeDomains in a clique have their own channels")
}

func TestClaimAdmissionAssignsChannels(t *testing.T) {
	setChannelReservationsGate(t, true)
	f := newAdmissionFixture(t, imex.ModeHostManaged, cdInNamespace("cd-a", "team-a"), cdInNamespace("cd-b", "team-b"))
	f.clique(t, "node-1", "clique-1")
	f.clique(t, "node-2", "clique-1")
	f.clique(t, "node-3", "clique-2")
	admit := func(name, namespace, domainID, node, template string) imex.ChannelAllocation {
		t.Helper()
		claim := allocatedChannelClaim(t, name, namespace, domainID, node, template)
		f.add(t, claim)
		require.Equal(t, imex.ChannelAdmittedCondition, f.admit(t, claim).Type)
		return f.allocation(t, namespace, name)
	}

	a1 := admit("a1", "team-a", "cd-a", "node-1", "job")
	assert.Equal(t, imex.ChannelAllocation{Channel: 1, IMEXDomain: "clique-1"}, a1, "the claim status carries the channel and its clique")
	assert.Equal(t, a1, admit("a2", "team-a", "cd-a", "node-2", "job"), "claims of a template share its channel in a clique")
	assert.Equal(t, 2, admit("a-serve", "team-a", "cd-a", "node-1", "serve").Channel, "another template gets its own channel")
	assert.Equal(t, 3, admit("b1", "team-b", "cd-b", "node-2", "job").Channel, "another ComputeDomain gets its own channel")
	assert.Equal(t, imex.ChannelAllocation{Channel: 1, IMEXDomain: "clique-2"}, admit("b-other-clique", "team-b", "cd-b", "node-3", "job"), "channels are unique per clique")
	assert.Equal(t, imex.ChannelAllocation{Channel: 1, IMEXDomain: "node/node-4"}, admit("a-no-clique", "team-a", "cd-a", "node-4", "job"), "a node without a clique is its own IMEX domain")
	assert.Equal(t, legacySharedChannel, admit("legacy", "team-a", "cd-a", "node-1", "").Channel, "claims without a template name get the legacy shared channel")

	perChannel := legacyChannelClaim(t, "per-channel", "team-a", "cd-a", "node-1", "channel-3")
	f.add(t, perChannel)
	assert.Equal(t, imex.ChannelAdmittedCondition, f.admit(t, perChannel).Type, "per-channel devices of plugins without the gate carry their channel in the name")
	updated, err := f.client.ResourceV1().ResourceClaims("team-a").Get(context.Background(), "per-channel", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Nil(t, updated.Status.Devices[0].Data)
}

func TestClaimAdmissionRunsOutOfChannels(t *testing.T) {
	setChannelReservationsGate(t, true)
	f := newAdmissionFixture(t, imex.ModeHostManaged, cdInNamespace("cd-a", "team-a"), cdInNamespace("cd-b", "team-b"))
	f.m.assigner = newChannelAssigner(2)
	f.clique(t, "node-1", "clique-1")

	a := allocatedChannelClaim(t, "a", "team-a", "cd-a", "node-1", "job")
	f.add(t, a)
	assert.Equal(t, imex.ChannelAdmittedCondition, f.admit(t, a).Type)
	b := allocatedChannelClaim(t, "b", "team-b", "cd-b", "node-1", "job")
	f.add(t, b)
	c := f.admit(t, b)
	assert.Equal(t, imex.ChannelRejectedCondition, c.Type, "the scheduler then tries another clique")
	assert.Equal(t, "NoFreeChannel", c.Reason)

	f.remove(t, a)
	f.m.assigner.Forget(a.UID)
	retry := allocatedChannelClaim(t, "b-retry", "team-b", "cd-b", "node-1", "job")
	f.add(t, retry)
	assert.Equal(t, imex.ChannelAdmittedCondition, f.admit(t, retry).Type, "a channel is free once its last claim is gone")
}

func TestClaimAdmissionRestoresChannels(t *testing.T) {
	setChannelReservationsGate(t, true)
	f := newAdmissionFixture(t, imex.ModeHostManaged, cdInNamespace("cd-a", "team-a"), cdInNamespace("cd-b", "team-b"))
	f.clique(t, "node-1", "clique-1")
	a := allocatedChannelClaim(t, "a", "team-a", "cd-a", "node-1", "job")
	f.add(t, a)
	f.admit(t, a)
	admitted, err := f.client.ResourceV1().ResourceClaims("team-a").Get(context.Background(), "a", metav1.GetOptions{})
	require.NoError(t, err)

	// A new controller learns the channel from the admitted claim.
	restarted := newAdmissionFixture(t, imex.ModeHostManaged, cdInNamespace("cd-a", "team-a"), cdInNamespace("cd-b", "team-b"))
	restarted.clique(t, "node-1", "clique-1")
	restarted.m.restoreChannels(admitted)
	b := allocatedChannelClaim(t, "b", "team-b", "cd-b", "node-1", "job")
	restarted.add(t, b)
	restarted.admit(t, b)
	assert.Equal(t, 2, restarted.allocation(t, "team-b", "b").Channel, "the restored channel is not assigned again")
	a2 := allocatedChannelClaim(t, "a2", "team-a", "cd-a", "node-1", "job")
	restarted.add(t, a2)
	restarted.admit(t, a2)
	assert.Equal(t, 1, restarted.allocation(t, "team-a", "a2").Channel, "later claims of the template get the restored channel")

	deallocated := admitted.DeepCopy()
	deallocated.Status.Allocation = nil
	restarted.m.forgetChannels(deallocated)
	restarted.m.assigner.Forget(a2.UID)
	c := allocatedChannelClaim(t, "c", "team-a", "cd-a", "node-1", "other")
	restarted.add(t, c)
	restarted.admit(t, c)
	assert.Equal(t, 3, restarted.allocation(t, "team-a", "c").Channel, "a freed channel is reused last")
}

func TestPendingChannelResults(t *testing.T) {
	claim := allocatedChannelClaim(t, "a", "ns", "cd-a", "node-1", "job")
	assert.Len(t, pendingChannelResults(claim), 1)

	result := claim.Status.Allocation.Devices.Results[0]
	setChannelAdmissionCondition(claim, &result, admitDecision())
	assert.Empty(t, pendingChannelResults(claim), "decided channels are not admitted again")
	assert.True(t, channelInUse(claim, &result))

	withoutConditions := allocatedChannelClaim(t, "b", "ns", "cd-a", "node-1", "job")
	withoutConditions.Status.Allocation.Devices.Results[0].BindingConditions = nil
	assert.Empty(t, pendingChannelResults(withoutConditions), "only channels published with the binding condition are admitted")

	daemon := legacyChannelClaim(t, "c", "ns", "cd-a", "node-1", "daemon-0")
	assert.Empty(t, channelResults(daemon), "daemon devices are no channels")
}

func TestChannelDomainIDPrecedence(t *testing.T) {
	claim := allocatedChannelClaim(t, "a", "ns", "cd-claim", "node-1", "job")
	classConfig := claim.Status.Allocation.Devices.Config[0].DeepCopy()
	classConfig.Source = resourceapi.AllocationConfigSourceClass
	classConfig.Opaque.Parameters.Raw = []byte(`{"kind":"ComputeDomainChannelConfig","domainID":"cd-class","resourceClaimTemplateName":"class"}`)
	// Listed after the claim config, but claim configs take precedence.
	claim.Status.Allocation.Devices.Config = append(claim.Status.Allocation.Devices.Config, *classConfig)

	assert.Equal(t, "cd-claim", channelDomainID(claim, "channel"))
	assert.Equal(t, "job", channelConfig(claim, "channel").ResourceClaimTemplateName)
	assert.Empty(t, channelDomainID(claim, "other-request"))
}

func TestClaimAdmissionGateDisabled(t *testing.T) {
	// Kubelet plugins may enable the gate before the controller: their
	// channels are admitted rather than left to time out.
	f := newAdmissionFixture(t, imex.ModeDriverManaged)
	claim := allocatedChannelClaim(t, "a", "team-a", "cd-unknown", "node-1", "job")
	f.add(t, claim)

	c := f.admit(t, claim)

	assert.Equal(t, imex.ChannelAdmittedCondition, c.Type)
	assert.Empty(t, f.m.inflight, "no node ownership without the gate")
}

func TestClaimAdmissionRejectsUnattributableClaims(t *testing.T) {
	setChannelReservationsGate(t, true)
	cd := cdInNamespace("cd-a", "team-a")
	f := newAdmissionFixture(t, imex.ModeDriverManaged, cd)

	invalid := allocatedChannelClaim(t, "bad-cfg", "team-a", "not-needed-for-test", "node-1", "job")
	invalid.Status.Allocation.Devices.Config[0].Opaque.Parameters.Raw = []byte(`{"apiVersion":"resource.nvidia.com/v1beta1","kind":"ComputeDomainChannelConfig","domainID":"not-needed-for-test","unexpectedField":true}`)
	unknown := allocatedChannelClaim(t, "unknown-cd", "team-a", "not-needed-for-test", "node-1", "job")
	noConfig := allocatedChannelClaim(t, "no-cfg", "team-a", "cd-a", "node-1", "job")
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
	assert.Empty(t, f.m.inflight, "rejected claims do not hold the node")

	cd.DeletionTimestamp = &metav1.Time{}
	deleting := allocatedChannelClaim(t, "deleting", "team-a", "cd-a", "node-1", "job")
	f.add(t, deleting)
	assert.Equal(t, "ComputeDomainDeleting", f.admit(t, deleting).Reason)
}

func TestClaimAdmissionStatusUpdateFailure(t *testing.T) {
	setChannelReservationsGate(t, true)
	f := newAdmissionFixture(t, imex.ModeDriverManaged, cdInNamespace("cd-a", "team-a"), cdInNamespace("cd-b", "team-b"))
	a := allocatedChannelClaim(t, "a", "team-a", "cd-a", "node-1", "job")
	f.add(t, a)
	failed := false
	f.client.PrependReactor("update", "resourceclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "status" || failed {
			return false, nil, nil
		}
		failed = true
		return true, nil, errors.NewConflict(resourceapi.Resource("resourceclaims"), "a", nil)
	})

	require.Error(t, f.m.admitKey(context.Background(), "team-a/a"))
	assert.Empty(t, f.m.inflight, "a failed admission is not in flight")
	assert.Equal(t, "cd-a", f.owner(t, "node-1"), "the node stays labeled for the retry")

	b := allocatedChannelClaim(t, "b", "team-b", "cd-b", "node-1", "job")
	f.add(t, b)
	assert.Equal(t, imex.ChannelRejectedCondition, f.admit(t, b).Type, "the label holds the node before the cache shows it")
	assert.Equal(t, imex.ChannelAdmittedCondition, f.admit(t, a).Type, "the retry is admitted")
}

func TestReleaseNode(t *testing.T) {
	setChannelReservationsGate(t, true)
	f := newAdmissionFixture(t, imex.ModeDriverManaged, cdInNamespace("cd-a", "team-a"), cdInNamespace("cd-b", "team-b"))
	ctx := context.Background()
	a := allocatedChannelClaim(t, "a", "team-a", "cd-a", "node-1", "job")
	f.add(t, a)
	f.admit(t, a)
	f.sync(t, a)
	f.syncNode(t, "node-1")

	require.NoError(t, f.m.releaseNode(ctx, "node-1"))
	assert.Equal(t, "cd-a", f.owner(t, "node-1"), "a claim of the owner holds the node")

	rejected := allocatedChannelClaim(t, "b", "team-b", "cd-b", "node-1", "job")
	f.add(t, rejected)
	f.admit(t, rejected)
	f.sync(t, rejected)
	f.remove(t, a)
	require.NoError(t, f.m.releaseNode(ctx, "node-1"))
	assert.Empty(t, f.owner(t, "node-1"), "a pod deleted before it started frees the node, whatever other claims were rejected there")

	f.labelNode(t, "node-2", "cd-a")
	f.m.addInflight("node-2", "c", "cd-a")
	require.NoError(t, f.m.releaseNode(ctx, "node-2"))
	assert.Equal(t, "cd-a", f.owner(t, "node-2"), "an admission in flight holds the node")

	// The cache still shows cd-a, but the API already shows the next owner.
	f.m.forgetInflight("c")
	node := f.node(t, "node-2").DeepCopy()
	node.Labels[computeDomainLabelKey] = "cd-b"
	_, err := f.client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, f.m.releaseNode(ctx, "node-2"))
	assert.Equal(t, "cd-b", f.owner(t, "node-2"), "a node relabeled since the cache saw it is left alone")
}

func TestEnqueueReleases(t *testing.T) {
	setChannelReservationsGate(t, true)
	f := newAdmissionFixture(t, imex.ModeDriverManaged)
	f.m.releaseDelay = 0
	a := allocatedChannelClaim(t, "a", "team-a", "cd-a", "node-1", "job")
	deallocated := a.DeepCopy()
	deallocated.Status.Allocation = nil

	f.m.enqueueReleases(a, a)
	f.m.enqueueReleases(a, deallocated)
	f.m.enqueueReleases(a, nil)

	assert.Equal(t, 1, f.m.releaseQueue.Len(), "a deallocated or deleted claim queues its node once")
}

func TestClaimsForComputeDomain(t *testing.T) {
	f := newAdmissionFixture(t, imex.ModeDriverManaged)
	a := allocatedChannelClaim(t, "a", "team-a", "cd-a", "node-1", "job")
	b := allocatedChannelClaim(t, "b", "team-b", "cd-b", "node-1", "job")
	f.add(t, a)
	f.add(t, b)

	claims := f.m.claimsForComputeDomain("cd-a")
	require.Len(t, claims, 1)
	assert.Equal(t, "a", claims[0].Name)
	assert.Len(t, f.m.claimsByIndex(claimNodeIndex, "node-1"), 2)
	assert.Empty(t, f.m.claimsByIndex(claimNodeIndex, "node-2"))
}

func clientServing(t *testing.T, groupVersions ...string) *fake.Clientset {
	t.Helper()
	client := fake.NewSimpleClientset()
	discovery, ok := client.Discovery().(*fakediscovery.FakeDiscovery)
	require.True(t, ok)
	for _, gv := range groupVersions {
		discovery.Resources = append(discovery.Resources, &metav1.APIResourceList{
			GroupVersion: gv,
			APIResources: []metav1.APIResource{{Name: "resourceclaims"}},
		})
	}
	return client
}

func TestClaimAdmissionSupported(t *testing.T) {
	tests := map[string]struct {
		groupVersions []string
		gate          bool
		want          bool
		wantErr       string
	}{
		"Kubernetes 1.34+":                          {groupVersions: []string{"resource.k8s.io/v1"}, want: true},
		"Kubernetes 1.32 and 1.33 without the gate": {groupVersions: []string{"resource.k8s.io/v1beta2"}},
		"Kubernetes 1.32 and 1.33 with the gate": {
			groupVersions: []string{"resource.k8s.io/v1beta2"},
			gate:          true,
			wantErr:       "require resource.k8s.io/v1",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			setChannelReservationsGate(t, tc.gate)

			got, err := claimAdmissionSupported(clientServing(t, tc.groupVersions...))

			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestClaimAdmissionSupportedDiscoveryError(t *testing.T) {
	client := clientServing(t, "resource.k8s.io/v1")
	client.PrependReactor("get", "resource", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.NewServiceUnavailable("unavailable")
	})

	_, err := claimAdmissionSupported(client)

	require.Error(t, err, "a discovery error must not turn admission off")
}

func TestComputeDomainManagerWithoutClaimAdmission(t *testing.T) {
	m := NewComputeDomainManager(&ManagerConfig{
		imexConfig: imex.Config{Mode: imex.ModeHostManaged},
		clientsets: flags.ClientSets{Nvidia: nvfake.NewSimpleClientset(), Core: fake.NewSimpleClientset()},
	})

	assert.Nil(t, m.claimAdmissionManager, "no claim informer that could never sync")
	require.NoError(t, m.Stop())
}

func TestClaimAdmissionStaticDaemon(t *testing.T) {
	setChannelReservationsGate(t, true)
	f := newAdmissionFixture(t, imex.ModeDriverManaged, cdInNamespace("cd-a", "team-a"), cdInNamespace("cd-b", "team-b"))
	f.m.config.imexConfig.Scope, f.m.config.imexConfig.Isolation = imex.ScopeCluster, imex.IsolationIMEXChannel
	f.clique(t, "node-1", "clique-1")

	a := allocatedChannelClaim(t, "a", "team-a", "cd-a", "node-1", "job")
	f.add(t, a)
	assert.Equal(t, imex.ChannelAdmittedCondition, f.admit(t, a).Type)

	// Another ComputeDomain shares the node on its own channel.
	b := allocatedChannelClaim(t, "b", "team-b", "cd-b", "node-1", "job")
	f.add(t, b)
	assert.Equal(t, imex.ChannelAdmittedCondition, f.admit(t, b).Type)
	assert.NotEqual(t, f.allocation(t, "team-a", "a").Channel, f.allocation(t, "team-b", "b").Channel)
	assert.Empty(t, f.m.inflight, "static IMEX daemons do not own nodes")
}
