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
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	coreapi "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	draclient "k8s.io/dynamic-resource-allocation/client"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
)

func channelSlice(node string, ids ...int) *resourceapi.ResourceSlice {
	slice := &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: node},
		Spec: resourceapi.ResourceSliceSpec{
			Driver: DriverName, NodeName: &node,
			Pool: resourceapi.ResourcePool{Name: node, Generation: 1, ResourceSliceCount: 1},
		},
	}
	for _, id := range ids {
		slice.Spec.Devices = append(slice.Spec.Devices, resourceapi.Device{
			Name: fmt.Sprintf("channel-%d", id),
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"type": {StringValue: ptr.To("channel")}, "id": {IntValue: ptr.To(int64(id))},
			},
		})
	}
	return slice
}

func channelManager(objects ...runtime.Object) (*WorkloadResourceClaimTemplateManager, *corefake.Clientset) {
	client := corefake.NewClientset(objects...)
	return &WorkloadResourceClaimTemplateManager{BaseResourceClaimTemplateManager: &BaseResourceClaimTemplateManager{
		config: &ManagerConfig{driverNamespace: "driver", clientsets: flags.ClientSets{Core: client, Resource: draclient.New(client)}},
	}}, client
}

func TestHostChannelReservationLifecycle(t *testing.T) {
	ctx := context.Background()
	manager, client := channelManager(channelSlice("node-a", 0, 1), channelSlice("node-b", 0, 1))
	first, err := manager.reserveHostChannel(ctx, "domain-a")
	require.NoError(t, err)
	require.Equal(t, 0, first)
	second, err := manager.reserveHostChannel(ctx, "domain-b")
	require.NoError(t, err)
	require.Equal(t, 1, second)
	_, err = manager.reserveHostChannel(ctx, "domain-c")
	require.ErrorContains(t, err, "no unreserved IMEX channel")

	// A new manager has no local reservation state, as after process restart.
	restarted := &WorkloadResourceClaimTemplateManager{BaseResourceClaimTemplateManager: &BaseResourceClaimTemplateManager{config: manager.config}}
	id, err := restarted.reserveHostChannel(ctx, "domain-a")
	require.NoError(t, err)
	require.Equal(t, first, id)
	require.NoError(t, restarted.releaseHostChannel(ctx, "domain-a"))
	require.NoError(t, restarted.releaseHostChannel(ctx, "domain-a"))
	id, err = restarted.reserveHostChannel(ctx, "domain-c")
	require.NoError(t, err)
	require.Equal(t, 0, id)
	registry, err := client.CoreV1().ConfigMaps("driver").Get(ctx, imex.ChannelReservationsConfigMap, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"domain-b": "1", "domain-c": "0"}, registry.Data)
}

func TestHostChannelReservationConflict(t *testing.T) {
	ctx := context.Background()
	manager, client := channelManager(channelSlice("node-a", 0, 1))
	conflicted := false
	client.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if conflicted {
			return false, nil, nil
		}
		conflicted = true
		update, ok := action.(clienttesting.UpdateAction)
		require.True(t, ok)
		object, ok := update.GetObject().(*coreapi.ConfigMap)
		require.True(t, ok)
		registry := object.DeepCopy()
		registry.Data = map[string]string{"other-domain": "0"}
		require.NoError(t, client.Tracker().Update(coreapi.SchemeGroupVersion.WithResource("configmaps"), registry, "driver"))
		return true, nil, errors.NewConflict(schema.GroupResource{Resource: "configmaps"}, registry.Name, fmt.Errorf("concurrent reservation"))
	})
	id, err := manager.reserveHostChannel(ctx, "domain-a")
	require.NoError(t, err)
	require.True(t, conflicted)
	require.Equal(t, 1, id)
}

func TestAvailableHostChannels(t *testing.T) {
	for name, test := range map[string]struct {
		slices    []runtime.Object
		want      []int
		wantError bool
	}{
		"intersection":   {slices: []runtime.Object{channelSlice("a", 0, 1, 2), channelSlice("b", 0, 2)}, want: []int{0, 2}},
		"no publication": {},
		"incomplete publication": {slices: []runtime.Object{func() *resourceapi.ResourceSlice {
			s := channelSlice("a", 0)
			s.Spec.Pool.ResourceSliceCount = 2
			return s
		}()}, wantError: true},
		"latest generation": {slices: []runtime.Object{channelSlice("a", 0, 1), func() *resourceapi.ResourceSlice {
			s := channelSlice("a", 0)
			s.Name = "a-new"
			s.Spec.Pool.Generation = 2
			return s
		}()}, want: []int{0}},
	} {
		t.Run(name, func(t *testing.T) {
			manager, _ := channelManager(test.slices...)
			channels, err := availableHostChannels(context.Background(), manager.config)
			if test.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, channels)
		})
	}
}

func TestHostChannelReleaseWaitsForClaims(t *testing.T) {
	for _, allocated := range []bool{false, true} {
		t.Run(fmt.Sprintf("allocated=%v", allocated), func(t *testing.T) {
			ctx := context.Background()
			configuration := resourceapi.DeviceConfiguration{Opaque: &resourceapi.OpaqueDeviceConfiguration{
				Driver: DriverName, Parameters: runtime.RawExtension{Raw: []byte(`{"kind":"ComputeDomainChannelConfig","domainID":"domain-a"}`)},
			}}
			claim := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "workload"}}
			if allocated {
				claim.Status.Allocation = &resourceapi.AllocationResult{Devices: resourceapi.DeviceAllocationResult{Config: []resourceapi.DeviceAllocationConfiguration{{DeviceConfiguration: configuration}}}}
			} else {
				claim.Spec.Devices.Config = []resourceapi.DeviceClaimConfiguration{{DeviceConfiguration: configuration}}
			}
			manager, client := channelManager(channelSlice("a", 0), claim)
			_, err := manager.reserveHostChannel(ctx, "domain-a")
			require.NoError(t, err)
			require.ErrorContains(t, manager.releaseHostChannel(ctx, "domain-a"), "waiting for ResourceClaim workload/worker")
			_, err = manager.reserveHostChannel(ctx, "domain-b")
			require.Error(t, err)
			require.NoError(t, client.ResourceV1().ResourceClaims("workload").Delete(ctx, "worker", metav1.DeleteOptions{}))
			require.NoError(t, manager.releaseHostChannel(ctx, "domain-a"))
			id, err := manager.reserveHostChannel(ctx, "domain-b")
			require.NoError(t, err)
			require.Zero(t, id)
		})
	}
}
