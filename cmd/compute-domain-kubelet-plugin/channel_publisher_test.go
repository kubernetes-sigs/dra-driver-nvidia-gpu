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
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
)

// recordingPublisher records the channel device of each publish.
type recordingPublisher struct {
	mu        sync.Mutex
	published []resourceapi.Device
}

func (r *recordingPublisher) publish(_ context.Context, resources resourceslice.DriverResources) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, pool := range resources.Pools {
		for _, slice := range pool.Slices {
			for _, device := range slice.Devices {
				if device.Name == imex.ChannelDeviceName {
					r.published = append(r.published, device)
				}
			}
		}
	}
	return nil
}

func (r *recordingPublisher) owners() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var owners []string
	for _, device := range r.published {
		owners = append(owners, channelOwner(device))
	}
	return owners
}

func TestDevicePublisherPublishesOnChange(t *testing.T) {
	setChannelReservationsGate(t, true)
	allocatable, _ := testChannels(4)
	config := driverManagedConfig()
	config.flags.nodeName = "node"
	recorder := &recordingPublisher{}
	p := newDevicePublisher(config, allocatable, recorder.publish)
	ctx := context.Background()

	require.NoError(t, p.Update(ctx, func(s *channelDeviceState) { s.owner = "cd-a" }))
	assert.Empty(t, recorder.owners(), "nothing is published before Start")

	require.NoError(t, p.Start(ctx))
	require.NoError(t, p.Update(ctx, func(s *channelDeviceState) { s.owner = "cd-a" }))
	require.NoError(t, p.Update(ctx, func(s *channelDeviceState) { s.owner = "" }))

	assert.Equal(t, []string{"cd-a", ""}, recorder.owners(), "an unchanged owner is not published again")
}

func TestDevicePublisherWatchesOwnerLabel(t *testing.T) {
	setChannelReservationsGate(t, true)
	allocatable, _ := testChannels(4)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Labels: map[string]string{computeDomainLabelKey: "cd-a"}}}
	other := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "other", Labels: map[string]string{computeDomainLabelKey: "cd-b"}}}
	client := fake.NewClientset(node, other)
	config := driverManagedConfig()
	config.flags.nodeName = "node"
	config.clientsets.Core = client
	recorder := &recordingPublisher{}
	p := newDevicePublisher(config, allocatable, recorder.publish)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(p.Stop)

	require.NoError(t, p.WatchOwner(ctx))
	require.NoError(t, p.Start(ctx))
	assert.Equal(t, []string{"cd-a"}, recorder.owners(), "the first publish carries the owner")

	node = node.DeepCopy()
	delete(node.Labels, computeDomainLabelKey)
	_, err := client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		owners := recorder.owners()
		return len(owners) == 2 && owners[1] == ""
	}, 5*time.Second, 10*time.Millisecond, "removing the label frees the node")
}

func TestDevicePublisherFollowsCliqueID(t *testing.T) {
	setChannelReservationsGate(t, true)
	allocatable, _ := testChannels(4)
	config := hostManagedConfig()
	config.flags.nodeName = "node"
	cliqueID := "clique-a"
	manager := &ComputeDomainManager{config: config, cliqueID: cliqueID, getCliqueIDFunc: func() (string, error) { return cliqueID, nil }}
	recorder := &recordingPublisher{}
	d := &driver{
		state:     &DeviceState{computeDomainManager: manager},
		publisher: newDevicePublisher(config, allocatable, recorder.publish),
	}
	ctx := context.Background()

	require.NoError(t, d.startPublishing(ctx, config))
	cliqueID = "clique-b"
	require.NoError(t, manager.refreshGPUCliqueID(ctx))
	require.NoError(t, manager.refreshGPUCliqueID(ctx))

	var cliques []string
	for _, device := range recorder.published {
		cliques = append(cliques, ptr.Deref(device.Attributes[imex.ChannelCliqueAttribute].StringValue, ""))
	}
	assert.Equal(t, []string{"clique-a", "clique-b"}, cliques, "a new partition is published once")
}
