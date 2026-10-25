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
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
	nvfake "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/clientset/versioned/fake"
)

type cddsFixture struct {
	m         *ComputeDomainDaemonSetManager
	core      *fake.Clientset
	nv        *nvfake.Clientset
	hasClaims bool
}

func testComputeDomainDaemonSet() *nvapi.ComputeDomainDaemonSet {
	return &nvapi.ComputeDomainDaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: nvapi.ComputeDomainDaemonSetName, UID: "cdds-uid", Generation: 1},
		Spec: nvapi.ComputeDomainDaemonSetSpec{
			MaxNodesPerClique: 18,
			LogVerbosity:      1,
			UpdateStrategy:    nvapi.ComputeDomainDaemonSetUpdateStrategy{Type: nvapi.ComputeDomainDaemonSetUpdateStrategyOnDelete},
		},
	}
}

func newCDDSFixture(t testing.TB, objects ...runtime.Object) *cddsFixture {
	t.Helper()
	var coreObjects, nvObjects []runtime.Object
	for _, obj := range objects {
		switch obj.(type) {
		case *nvapi.ComputeDomainDaemonSet, *nvapi.ComputeDomainClique:
			nvObjects = append(nvObjects, obj)
		default:
			coreObjects = append(coreObjects, obj)
		}
	}
	f := &cddsFixture{core: fake.NewSimpleClientset(coreObjects...), nv: nvfake.NewSimpleClientset(nvObjects...)}
	config := &ManagerConfig{
		driverNamespace: "driver",
		imageName:       "driver:v1",
		imexConfig:      imex.Config{Mode: imex.ModeDriverManaged, Scope: imex.ScopeCluster, Isolation: imex.IsolationIMEXChannel},
		clientsets:      flags.ClientSets{Core: f.core, Nvidia: f.nv},
	}
	f.m = NewComputeDomainDaemonSetManager(config, func() bool { return f.hasClaims })
	f.m.daemonSetTemplatePath = "../../templates/compute-domain-static-daemon.tmpl.yaml"
	f.m.rctTemplatePath = "../../templates/compute-domain-daemon-claim-template.tmpl.yaml"
	f.m.now = func() time.Time { return rolloutNow }
	f.refresh(t)
	return f
}

// refresh loads the informer caches from the fake API servers.
func (f *cddsFixture) refresh(t testing.TB) {
	t.Helper()
	ctx := context.Background()
	replace := func(store interface{ Replace([]any, string) error }, list runtime.Object, err error) {
		require.NoError(t, err)
		items, err := meta.ExtractList(list)
		require.NoError(t, err)
		objs := make([]any, len(items))
		for i := range items {
			objs[i] = items[i]
		}
		require.NoError(t, store.Replace(objs, ""))
	}
	cdds, err := f.nv.ResourceV1beta1().ComputeDomainDaemonSets().List(ctx, metav1.ListOptions{})
	replace(f.m.cddsInformer.GetStore(), cdds, err)
	cliques, err := f.nv.ResourceV1beta1().ComputeDomainCliques("driver").List(ctx, metav1.ListOptions{})
	replace(f.m.cliqueInf.GetStore(), cliques, err)
	daemonSets, err := f.core.AppsV1().DaemonSets("driver").List(ctx, metav1.ListOptions{})
	replace(f.m.daemonSetInf.GetStore(), daemonSets, err)
	pods, err := f.core.CoreV1().Pods("driver").List(ctx, metav1.ListOptions{})
	replace(f.m.podInf.GetStore(), pods, err)
	rcts, err := f.core.ResourceV1().ResourceClaimTemplates("driver").List(ctx, metav1.ListOptions{})
	replace(f.m.rctInf.GetStore(), rcts, err)
	nodes, err := f.core.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	replace(f.m.nodeInf.GetStore(), nodes, err)
}

func (f *cddsFixture) sync(t *testing.T) time.Duration {
	t.Helper()
	requeueAfter, err := f.m.sync(context.Background())
	require.NoError(t, err)
	f.refresh(t)
	return requeueAfter
}

func (f *cddsFixture) daemonSet(t *testing.T) *appsv1.DaemonSet {
	t.Helper()
	ds, err := f.core.AppsV1().DaemonSets("driver").Get(context.Background(), staticDaemonSetName("cdds-uid"), metav1.GetOptions{})
	require.NoError(t, err)
	return ds
}

func (f *cddsFixture) cdds(t *testing.T) *nvapi.ComputeDomainDaemonSet {
	t.Helper()
	cdds, err := f.nv.ResourceV1beta1().ComputeDomainDaemonSets().Get(context.Background(), nvapi.ComputeDomainDaemonSetName, metav1.GetOptions{})
	require.NoError(t, err)
	return cdds
}

func (f *cddsFixture) update(t *testing.T, mutate func(*nvapi.ComputeDomainDaemonSet)) {
	t.Helper()
	cdds := f.cdds(t)
	mutate(cdds)
	cdds.Generation++
	_, err := f.nv.ResourceV1beta1().ComputeDomainDaemonSets().Update(context.Background(), cdds, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.refresh(t)
}

func TestComputeDomainDaemonSetCreatesStaticDaemons(t *testing.T) {
	f := newCDDSFixture(t, testComputeDomainDaemonSet())

	f.sync(t)

	assert.Contains(t, f.cdds(t).Finalizers, computeDomainDaemonSetFinalizer)

	rct, err := f.core.ResourceV1().ResourceClaimTemplates("driver").Get(context.Background(), staticDaemonSetName("cdds-uid"), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "cdds-uid", rct.Labels[computeDomainDaemonSetLabelKey])
	assert.Empty(t, rct.Labels[computeDomainLabelKey], "the static daemons serve no ComputeDomain")
	var daemonConfig nvapi.ComputeDomainDaemonConfig
	require.NoError(t, json.Unmarshal(rct.Spec.Spec.Devices.Config[0].Opaque.Parameters.Raw, &daemonConfig))
	assert.Equal(t, "cdds-uid", daemonConfig.DomainID)

	ds := f.daemonSet(t)
	assert.Equal(t, appsv1.OnDeleteDaemonSetStrategyType, ds.Spec.UpdateStrategy.Type, "Kubernetes never restarts IMEX daemons on its own")
	assert.Equal(t, "driver:v1", ds.Spec.Template.Spec.Containers[0].Image)
	assert.NotEmpty(t, ds.Spec.Template.Annotations[staticDaemonTemplateHashKey])
	assert.Equal(t, gpuCliqueNodeLabelKey, ds.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Key)
	assert.Equal(t, rct.Name, *ds.Spec.Template.Spec.ResourceClaims[0].ResourceClaimTemplateName)
	assert.Empty(t, ds.Finalizers)
}

func TestComputeDomainDaemonSetKeepsImageAcrossDriverUpgrades(t *testing.T) {
	f := newCDDSFixture(t, testComputeDomainDaemonSet())
	f.sync(t)
	hash := f.daemonSet(t).Spec.Template.Annotations[staticDaemonTemplateHashKey]

	// A driver upgrade changes the controller's image.
	f.m.config.imageName = "driver:v2"
	f.sync(t)
	assert.Equal(t, "driver:v1", f.daemonSet(t).Spec.Template.Spec.Containers[0].Image)
	assert.Equal(t, hash, f.daemonSet(t).Spec.Template.Annotations[staticDaemonTemplateHashKey])

	f.update(t, func(cdds *nvapi.ComputeDomainDaemonSet) { cdds.Spec.Image = "driver:v2" })
	f.sync(t)
	assert.Equal(t, "driver:v2", f.daemonSet(t).Spec.Template.Spec.Containers[0].Image, "spec.image upgrades the daemons")
	assert.NotEqual(t, hash, f.daemonSet(t).Spec.Template.Annotations[staticDaemonTemplateHashKey])
}

func TestComputeDomainDaemonSetRendersSpec(t *testing.T) {
	cdds := testComputeDomainDaemonSet()
	cdds.Spec.NodeSelector = map[string]string{"pool": "gb200"}
	cdds.Spec.PriorityClassName = "system-node-critical"
	cdds.Spec.IMEXConfig = map[string]string{"IMEX_SERVER_PORT": "50005"}
	cdds.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "regcred"}}
	f := newCDDSFixture(t, cdds)

	f.sync(t)

	spec := f.daemonSet(t).Spec.Template.Spec
	assert.Equal(t, map[string]string{"pool": "gb200"}, spec.NodeSelector)
	assert.Equal(t, "system-node-critical", spec.PriorityClassName)
	assert.Equal(t, []corev1.LocalObjectReference{{Name: "regcred"}}, spec.ImagePullSecrets)
	assert.Contains(t, spec.Containers[0].Env, corev1.EnvVar{Name: "IMEX_CONFIG_OVERRIDES", Value: "IMEX_SERVER_PORT=50005,"})
	assert.Contains(t, spec.Containers[0].Env, corev1.EnvVar{Name: "MAX_NODES_PER_IMEX_DOMAIN", Value: "18"})
	require.Len(t, spec.Containers[0].Resources.Claims, 1, "the daemon claim stays")
}

func TestComputeDomainDaemonSetStatus(t *testing.T) {
	nodes, _ := testCliques(2, 2, "")
	objects := []runtime.Object{testComputeDomainDaemonSet()}
	for _, node := range nodes {
		objects = append(objects, node)
	}
	f := newCDDSFixture(t, objects...)
	f.sync(t)
	hash := f.daemonSet(t).Spec.Template.Annotations[staticDaemonTemplateHashKey]

	for i, node := range nodes {
		h := hash
		if i == 3 {
			h = "outdated"
		}
		_, err := f.core.CoreV1().Pods("driver").Create(context.Background(), daemonPod(node.Name, h, time.Hour), metav1.CreateOptions{})
		require.NoError(t, err)
	}
	ds := f.daemonSet(t)
	ds.Status.DesiredNumberScheduled, ds.Status.NumberReady = 4, 4
	_, err := f.core.AppsV1().DaemonSets("driver").UpdateStatus(context.Background(), ds, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.refresh(t)
	f.sync(t)

	status := f.cdds(t).Status
	assert.Equal(t, int64(1), status.ObservedGeneration)
	assert.Equal(t, int32(4), status.NumberReady)
	assert.Equal(t, int32(3), status.UpdatedNumberScheduled)
	assert.Equal(t, int32(2), status.NumberCliques)
	assert.Equal(t, []nvapi.ComputeDomainDaemonSetCliqueStatus{
		{ID: "c0", Nodes: 2, ReadyNodes: 2, UpdatedNodes: 2},
		{ID: "c1", Nodes: 2, ReadyNodes: 2, UpdatedNodes: 1},
	}, status.Cliques)
	assert.True(t, meta.IsStatusConditionTrue(status.Conditions, nvapi.ComputeDomainDaemonSetConditionReady))
	rollout := meta.FindStatusCondition(status.Conditions, nvapi.ComputeDomainDaemonSetConditionRolloutInProgress)
	require.NotNil(t, rollout)
	assert.Equal(t, "WaitingForPodDeletion", rollout.Reason, "OnDelete leaves the restart to the admin")

	f.core.ClearActions()
	f.sync(t)
	for _, action := range f.core.Actions() {
		assert.NotEqual(t, "delete", action.GetVerb(), "OnDelete never deletes daemon pods")
	}
}

// deletedPods returns the names of the pods the controller deleted.
func (f *cddsFixture) deletedPods() []string {
	var deleted []string
	for _, action := range f.core.Actions() {
		if del, ok := action.(k8stesting.DeleteAction); ok && del.GetResource().Resource == "pods" {
			deleted = append(deleted, del.GetName())
		}
	}
	return deleted
}

func TestComputeDomainDaemonSetRollingUpdate(t *testing.T) {
	nodes, _ := testCliques(3, 2, "")
	cdds := testComputeDomainDaemonSet()
	cdds.Spec.UpdateStrategy = nvapi.ComputeDomainDaemonSetUpdateStrategy{
		Type:          nvapi.ComputeDomainDaemonSetUpdateStrategyRollingUpdate,
		RollingUpdate: &nvapi.ComputeDomainDaemonSetRollingUpdate{},
	}
	objects := []runtime.Object{cdds}
	for _, node := range nodes {
		objects = append(objects, node, daemonPod(node.Name, "previous", time.Hour))
	}
	f := newCDDSFixture(t, objects...)

	f.sync(t)

	assert.ElementsMatch(t, []string{"imex-c0-n0", "imex-c0-n1"}, f.deletedPods(), "the first clique restarts as a whole")

	// The informer still shows the deleted pods: no second batch.
	f.core.ClearActions()
	_, err := f.m.sync(context.Background())
	require.NoError(t, err)
	for _, action := range f.core.Actions() {
		assert.NotEqual(t, "delete", action.GetVerb())
	}
}

func TestComputeDomainDaemonSetDeletion(t *testing.T) {
	cdds := testComputeDomainDaemonSet()
	clique := &nvapi.ComputeDomainClique{ObjectMeta: metav1.ObjectMeta{
		Name: "cdds-uid.c0", Namespace: "driver", Labels: map[string]string{computeDomainDaemonSetLabelKey: "cdds-uid"},
	}}
	f := newCDDSFixture(t, cdds, clique)
	f.sync(t)

	f.hasClaims = true
	deleting := f.cdds(t)
	deleting.DeletionTimestamp = &metav1.Time{Time: rolloutNow}
	require.NoError(t, f.nv.Tracker().Update(nvapi.SchemeGroupVersion.WithResource("computedomaindaemonsets"), deleting, ""))
	f.refresh(t)

	assert.Positive(t, f.sync(t), "deletion is retried")
	assert.True(t, meta.IsStatusConditionTrue(f.cdds(t).Status.Conditions, nvapi.ComputeDomainDaemonSetConditionDeletionBlocked))
	f.daemonSet(t)

	f.hasClaims = false
	_, err := f.m.sync(context.Background())
	require.Error(t, err, "waits for the DaemonSet to be gone")
	f.refresh(t)
	_, err = f.core.AppsV1().DaemonSets("driver").Get(context.Background(), staticDaemonSetName("cdds-uid"), metav1.GetOptions{})
	assert.Error(t, err)
	_, err = f.nv.ResourceV1beta1().ComputeDomainCliques("driver").Get(context.Background(), "cdds-uid.c0", metav1.GetOptions{})
	assert.Error(t, err, "its cliques are deleted")

	f.sync(t)
	assert.NotContains(t, f.cdds(t).Finalizers, computeDomainDaemonSetFinalizer)
}

func TestComputeDomainDaemonSetDeletesOrphans(t *testing.T) {
	orphan := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name: staticDaemonSetName("old-uid"), Namespace: "driver", Labels: map[string]string{computeDomainDaemonSetLabelKey: "old-uid"},
	}}
	f := newCDDSFixture(t, testComputeDomainDaemonSet(), orphan)

	f.sync(t)

	_, err := f.core.AppsV1().DaemonSets("driver").Get(context.Background(), orphan.Name, metav1.GetOptions{})
	assert.Error(t, err, "daemons of a removed ComputeDomainDaemonSet are deleted")
	f.daemonSet(t)
}

func TestComputeDomainDaemonSetRemovesStaleDaemons(t *testing.T) {
	clique := &nvapi.ComputeDomainClique{
		ObjectMeta: metav1.ObjectMeta{Name: "cdds-uid.c0", Namespace: "driver", Labels: map[string]string{computeDomainDaemonSetLabelKey: "cdds-uid"}},
		Daemons: []*nvapi.ComputeDomainDaemonInfo{
			{NodeName: "c0-n0", CliqueID: "c0", Index: 0},
			{NodeName: "gone", CliqueID: "c0", Index: 1},
		},
	}
	f := newCDDSFixture(t, testComputeDomainDaemonSet(), clique, cliqueNode("c0-n0", "c0", true), daemonPod("c0-n0", "h", time.Hour))

	f.sync(t)

	got, err := f.nv.ResourceV1beta1().ComputeDomainCliques("driver").Get(context.Background(), clique.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, got.Daemons, 1)
	assert.Equal(t, "c0-n0", got.Daemons[0].NodeName)
}

// BenchmarkComputeDomainDaemonSetSync syncs 1,800 daemons in 100 cliques of
// NVL72 racks, mid rolling update.
func BenchmarkComputeDomainDaemonSetSync(b *testing.B) {
	nodes, pods := testCliques(100, 18, "previous")
	cdds := testComputeDomainDaemonSet()
	cdds.Spec.UpdateStrategy = nvapi.ComputeDomainDaemonSetUpdateStrategy{
		Type:          nvapi.ComputeDomainDaemonSetUpdateStrategyRollingUpdate,
		RollingUpdate: &nvapi.ComputeDomainDaemonSetRollingUpdate{Paused: true},
	}
	objects := []runtime.Object{cdds}
	for i := range nodes {
		objects = append(objects, nodes[i], pods[i])
	}
	f := newCDDSFixture(b, objects...)
	ctx := context.Background()
	_, err := f.m.sync(ctx)
	require.NoError(b, err)
	f.refresh(b)

	b.ResetTimer()
	for b.Loop() {
		if _, err := f.m.sync(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
