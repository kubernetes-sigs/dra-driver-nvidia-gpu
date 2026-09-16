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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	clientfeatures "k8s.io/client-go/features"
	clientfeaturestesting "k8s.io/client-go/features/testing"
	corefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
	nvfake "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/clientset/versioned/fake"
)

func TestStatusManagerCleansOnlyObsoleteCliqueMembers(t *testing.T) {
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.WatchListClient, false)
	require.True(t, featuregates.Enabled(featuregates.ComputeDomainCliques))
	overlappingPod := statusTestPod("member", "rack-a", "10.0.0.5")
	overlappingPod.Name = "replacement"
	for _, tc := range []struct {
		name       string
		domain     string
		clique     *string
		nodeName   string
		podIP      string
		additional *corev1.Pod
		keepMember bool
	}{
		{name: "same clique", domain: "domain", clique: ptr.To("rack-a"), nodeName: "member", podIP: "10.0.0.1", keepMember: true},
		{name: "unready replacement with new IP", domain: "domain", clique: ptr.To("rack-a"), nodeName: "member", podIP: "10.0.0.2", keepMember: true},
		{name: "startup before clique label and IP", domain: "domain", nodeName: "member", keepMember: true},
		{name: "moved to another clique", domain: "domain", clique: ptr.To("rack-b"), nodeName: "member", podIP: "10.0.0.2"},
		{name: "another domain on same node", domain: "other-domain", clique: ptr.To("rack-a"), nodeName: "member", podIP: "10.0.0.1"},
		{name: "another domain before clique label", domain: "other-domain", nodeName: "member"},
		{name: "explicitly nonfabric", domain: "domain", clique: ptr.To(""), nodeName: "member", podIP: "10.0.0.1"},
		{name: "unscheduled pod", domain: "domain", clique: ptr.To("rack-a")},
		{name: "pod removed"},
		{name: "overlapping pods in different cliques", domain: "domain", clique: ptr.To("rack-b"), nodeName: "member", podIP: "10.0.0.1", additional: overlappingPod, keepMember: true},
		{name: "overlapping pods in different domains", domain: "other-domain", clique: ptr.To("rack-a"), nodeName: "member", podIP: "10.0.0.1", additional: overlappingPod, keepMember: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			member := &nvapi.ComputeDomainDaemonInfo{NodeName: "member", IPAddress: "10.0.0.1", CliqueID: "rack-a", Index: 7, Status: nvapi.ComputeDomainStatusReady}
			survivor := &nvapi.ComputeDomainDaemonInfo{NodeName: "survivor", IPAddress: "10.0.0.3", CliqueID: "rack-a", Index: 11, Status: nvapi.ComputeDomainStatusReady}
			clique := statusTestClique("rack-a", []*nvapi.ComputeDomainDaemonInfo{
				member, survivor,
				{NodeName: "departed", IPAddress: "10.0.0.4", CliqueID: "rack-a", Index: 15},
			})
			pods := []runtime.Object{statusTestPod("survivor", "rack-a", "10.0.0.3")}
			if tc.domain != "" {
				pod := statusTestPod("member", "rack-a", tc.podIP)
				pod.Spec.NodeName = tc.nodeName
				pod.Labels = map[string]string{computeDomainLabelKey: tc.domain}
				if tc.clique != nil {
					pod.Labels[computeDomainCliqueLabelKey] = *tc.clique
				}
				pods = append(pods, pod)
			}
			if tc.additional != nil {
				pods = append(pods, tc.additional)
			}
			clients := pkgflags.ClientSets{Core: corefake.NewClientset(pods...), Nvidia: nvfake.NewSimpleClientset(clique)}
			startStatusTestManager(t, clients)
			want := []*nvapi.ComputeDomainDaemonInfo{survivor}
			if tc.keepMember {
				want = append(want, member)
			}
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				got, err := clients.Nvidia.ResourceV1beta1().ComputeDomainCliques("driver").Get(t.Context(), clique.Name, metav1.GetOptions{})
				if assert.NoError(c, err) {
					assert.ElementsMatch(c, want, got.Daemons)
				}
			}, 10*time.Second, 20*time.Millisecond)
		})
	}
}

func TestStatusManagerFreesMovedNodeSlotAndPublishesUniqueStatus(t *testing.T) {
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.WatchListClient, false)
	var daemons []*nvapi.ComputeDomainDaemonInfo
	var pods []runtime.Object
	for i := range 18 {
		nodeName := fmt.Sprintf("node-%02d", i)
		ip := fmt.Sprintf("10.0.0.%d", i+1)
		daemons = append(daemons, &nvapi.ComputeDomainDaemonInfo{NodeName: nodeName, IPAddress: ip, CliqueID: "rack-a", Index: i})
		pods = append(pods, statusTestPod(nodeName, "rack-a", ip))
	}
	oldClique := statusTestClique("rack-a", daemons)
	newClique := statusTestClique("rack-b", []*nvapi.ComputeDomainDaemonInfo{
		{NodeName: "node-00", IPAddress: "10.0.1.1", CliqueID: "rack-b", Index: 5},
	})
	domain := &nvapi.ComputeDomain{ObjectMeta: metav1.ObjectMeta{Name: "domain", Namespace: "driver", UID: "domain"}}
	clients := pkgflags.ClientSets{Core: corefake.NewClientset(pods...), Nvidia: nvfake.NewSimpleClientset(domain, oldClique)}
	startStatusTestManager(t, clients)

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		got, err := clients.Nvidia.ResourceV1beta1().ComputeDomains("driver").Get(t.Context(), domain.Name, metav1.GetOptions{})
		if assert.NoError(c, err) {
			assert.Len(c, got.Status.Nodes, 18)
		}
	}, 10*time.Second, 20*time.Millisecond)

	pod, err := clients.Core.CoreV1().Pods("driver").Get(t.Context(), "node-00", metav1.GetOptions{})
	require.NoError(t, err)
	pod.Labels[computeDomainCliqueLabelKey] = "rack-b"
	pod.Status.PodIP = "10.0.1.1"
	_, err = clients.Core.CoreV1().Pods("driver").Update(t.Context(), pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = clients.Nvidia.ResourceV1beta1().ComputeDomainCliques("driver").Create(t.Context(), newClique, metav1.CreateOptions{})
	require.NoError(t, err)

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		old, err := clients.Nvidia.ResourceV1beta1().ComputeDomainCliques("driver").Get(t.Context(), oldClique.Name, metav1.GetOptions{})
		if assert.NoError(c, err) {
			assert.Equal(c, daemons[1:], old.Daemons)
		}
		current, err := clients.Nvidia.ResourceV1beta1().ComputeDomainCliques("driver").Get(t.Context(), newClique.Name, metav1.GetOptions{})
		if assert.NoError(c, err) {
			assert.Equal(c, newClique.Daemons, current.Daemons)
		}
		got, err := clients.Nvidia.ResourceV1beta1().ComputeDomains("driver").Get(t.Context(), domain.Name, metav1.GetOptions{})
		if !assert.NoError(c, err) {
			return
		}
		assert.Len(c, got.Status.Nodes, 18)
		seen := make(map[string]bool)
		for _, node := range got.Status.Nodes {
			assert.False(c, seen[node.Name], "duplicate node %s", node.Name)
			seen[node.Name] = true
			if node.Name == "node-00" {
				assert.Equal(c, "rack-b", node.CliqueID)
				assert.Equal(c, "10.0.1.1", node.IPAddress)
				assert.Equal(c, 5, node.Index)
			}
		}
		assert.True(c, seen["node-00"])
	}, 10*time.Second, 20*time.Millisecond)
}

func TestStatusManagerConfirmsRemovalsAgainstLivePods(t *testing.T) {
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.WatchListClient, false)
	startingPod := statusTestPod("member", "rack-a", "")
	delete(startingPod.Labels, computeDomainCliqueLabelKey)
	oldDomainPod := statusTestPod("member", "rack-a", "10.0.0.1")
	oldDomainPod.Labels[computeDomainLabelKey] = "old-domain"
	for _, tc := range []struct {
		name       string
		cached     *corev1.Pod
		live       *corev1.Pod
		listErr    error
		keepMember bool
	}{
		{name: "pod cache has old clique", cached: statusTestPod("member", "rack-b", "10.0.0.1"), live: statusTestPod("member", "rack-a", "10.0.0.2"), keepMember: true},
		{name: "live pod is starting without clique label", cached: statusTestPod("member", "rack-b", "10.0.0.1"), live: startingPod, keepMember: true},
		{name: "pod cache has old domain", cached: oldDomainPod, live: statusTestPod("member", "rack-a", "10.0.0.1"), keepMember: true},
		{name: "pod missing from cache", live: statusTestPod("member", "rack-a", "10.0.0.1"), keepMember: true},
		{name: "live pod confirms move", cached: statusTestPod("member", "rack-b", "10.0.0.1"), live: statusTestPod("member", "rack-b", "10.0.0.1")},
		{name: "live pod confirms nonfabric", live: statusTestPod("member", "", "10.0.0.1")},
		{name: "live pod confirms deletion"},
		{name: "API unavailable", listErr: fmt.Errorf("API unavailable"), keepMember: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			member := &nvapi.ComputeDomainDaemonInfo{NodeName: "member", IPAddress: "10.0.0.1", CliqueID: "rack-a", Index: 7}
			survivor := &nvapi.ComputeDomainDaemonInfo{NodeName: "survivor", IPAddress: "10.0.0.3", CliqueID: "rack-a", Index: 11}
			departed := &nvapi.ComputeDomainDaemonInfo{NodeName: "departed", IPAddress: "10.0.0.4", CliqueID: "rack-a", Index: 15}
			clique := statusTestClique("rack-a", []*nvapi.ComputeDomainDaemonInfo{member, survivor, departed})
			cached := &corev1.PodList{Items: []corev1.Pod{*statusTestPod("survivor", "rack-a", "10.0.0.3")}}
			if tc.cached != nil {
				cached.Items = append(cached.Items, *tc.cached)
			}
			live := []runtime.Object{statusTestPod("survivor", "rack-a", "10.0.0.3")}
			if tc.live != nil {
				live = append(live, tc.live)
			}
			core := corefake.NewClientset(live...)
			snapshot, err := core.CoreV1().Pods("driver").List(t.Context(), metav1.ListOptions{})
			require.NoError(t, err)
			cached.ListMeta = snapshot.ListMeta
			var lists atomic.Int32
			core.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				if lists.Add(1) == 1 {
					return true, cached, nil
				}
				return tc.listErr != nil, nil, tc.listErr
			})
			clients := pkgflags.ClientSets{Core: core, Nvidia: nvfake.NewSimpleClientset(clique)}
			startStatusTestManager(t, clients)
			want := []*nvapi.ComputeDomainDaemonInfo{survivor}
			if tc.keepMember {
				want = append(want, member)
			}
			if tc.listErr != nil {
				want = append(want, departed)
			}
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				if tc.listErr != nil {
					assert.GreaterOrEqual(c, lists.Load(), int32(3), "cleanup must retry after a failed live read")
				}
				got, err := clients.Nvidia.ResourceV1beta1().ComputeDomainCliques("driver").Get(t.Context(), clique.Name, metav1.GetOptions{})
				if assert.NoError(c, err) {
					assert.ElementsMatch(c, want, got.Daemons)
				}
			}, 10*time.Second, 20*time.Millisecond)
		})
	}
}

func statusTestPod(node, clique, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: node, Namespace: "driver", UID: types.UID(node),
			Labels: map[string]string{computeDomainLabelKey: "domain", computeDomainCliqueLabelKey: clique},
		},
		Spec: corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			PodIP:      ip,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
		},
	}
}

func statusTestClique(clique string, daemons []*nvapi.ComputeDomainDaemonInfo) *nvapi.ComputeDomainClique {
	return &nvapi.ComputeDomainClique{
		ObjectMeta: metav1.ObjectMeta{
			Name: "domain." + clique, Namespace: "driver",
			Labels: map[string]string{computeDomainLabelKey: "domain", computeDomainCliqueLabelKey: clique},
		},
		Daemons: daemons,
	}
}

func startStatusTestManager(t *testing.T, clients pkgflags.ClientSets) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	manager := NewComputeDomainStatusManager(&ManagerConfig{driverNamespace: "driver", clientsets: clients},
		func() ([]*nvapi.ComputeDomain, error) {
			list, err := clients.Nvidia.ResourceV1beta1().ComputeDomains("driver").List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, err
			}
			var domains []*nvapi.ComputeDomain
			for i := range list.Items {
				domains = append(domains, &list.Items[i])
			}
			return domains, nil
		},
		func(ctx context.Context, cd *nvapi.ComputeDomain) (*nvapi.ComputeDomain, error) {
			names := sets.New[string]()
			for _, node := range cd.Status.Nodes {
				if names.Has(node.Name) {
					return nil, fmt.Errorf("ComputeDomain status.nodes: duplicate name %q", node.Name)
				}
				names.Insert(node.Name)
			}
			return clients.Nvidia.ResourceV1beta1().ComputeDomains(cd.Namespace).UpdateStatus(ctx, cd, metav1.UpdateOptions{})
		})
	t.Cleanup(func() { require.NoError(t, manager.Stop()) })
	require.NoError(t, manager.Start(ctx))
}
