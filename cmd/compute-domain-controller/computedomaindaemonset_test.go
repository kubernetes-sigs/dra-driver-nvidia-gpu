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
	"strconv"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/equality"
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
	m    *ComputeDomainDaemonSetManager
	core *fake.Clientset
	nv   *nvfake.Clientset
	// claimNodes have claims allocated an IMEX channel.
	claimNodes map[string]bool
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
	// Like the API server, bump the DaemonSet generation on spec changes.
	f.core.PrependReactor("*", "daemonsets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		switch action.GetVerb() {
		case "create":
			if create, ok := action.(k8stesting.CreateAction); ok {
				if ds, ok := create.GetObject().(*appsv1.DaemonSet); ok {
					ds.Generation = 1
				}
			}
		case "update":
			a, ok := action.(k8stesting.UpdateAction)
			if !ok || a.GetSubresource() != "" {
				break
			}
			ds, ok := a.GetObject().(*appsv1.DaemonSet)
			if !ok {
				break
			}
			old, err := f.core.Tracker().Get(a.GetResource(), ds.Namespace, ds.Name)
			if err != nil {
				break
			}
			oldDS, ok := old.(*appsv1.DaemonSet)
			if !ok {
				break
			}
			ds.Generation = oldDS.Generation
			if !equality.Semantic.DeepEqual(oldDS.Spec, ds.Spec) {
				ds.Generation++
			}
		}
		return false, nil, nil
	})
	config := &ManagerConfig{
		driverNamespace: "driver",
		imageName:       "driver:v1",
		imexConfig:      imex.Config{Mode: imex.ModeDriverManaged, Scope: imex.ScopeCluster, Isolation: imex.IsolationIMEXChannel},
		clientsets:      flags.ClientSets{Core: f.core, Nvidia: f.nv},
	}
	f.m = NewComputeDomainDaemonSetManager(config, func() map[string]bool { return f.claimNodes })
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

	f.claimNodes = map[string]bool{"c0-n0": true}
	deleting := f.cdds(t)
	deleting.DeletionTimestamp = &metav1.Time{Time: rolloutNow}
	require.NoError(t, f.nv.Tracker().Update(nvapi.SchemeGroupVersion.WithResource("computedomaindaemonsets"), deleting, ""))
	f.refresh(t)

	assert.Positive(t, f.sync(t), "deletion is retried")
	assert.True(t, meta.IsStatusConditionTrue(f.cdds(t).Status.Conditions, nvapi.ComputeDomainDaemonSetConditionDeletionBlocked))
	f.daemonSet(t)

	f.claimNodes = nil
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

func TestComputeDomainDaemonSetRealignsMovedDaemons(t *testing.T) {
	inPartition := func(pod *corev1.Pod, partition string) *corev1.Pod {
		pod.Labels[computeDomainCliqueLabelKey] = partition
		return pod
	}
	clique := &nvapi.ComputeDomainClique{
		ObjectMeta: metav1.ObjectMeta{Name: "cdds-uid.c0", Namespace: "driver", Labels: map[string]string{
			computeDomainDaemonSetLabelKey: "cdds-uid",
			computeDomainCliqueLabelKey:    "c0",
		}},
		Daemons: []*nvapi.ComputeDomainDaemonInfo{
			{NodeName: "moved", CliqueID: "c0", Index: 0},
			{NodeName: "unlabeled", CliqueID: "c0", Index: 1},
		},
	}
	f := newCDDSFixture(t, testComputeDomainDaemonSet(), clique,
		// Re-partitioned from c0 to c1.
		cliqueNode("moved", "c1", true), inPartition(daemonPod("moved", "h", time.Hour), "c0"),
		// Its daemon has not joined a partition yet.
		cliqueNode("starting", "c1", true), daemonPod("starting", "h", time.Hour),
		cliqueNode("same", "c1", true), inPartition(daemonPod("same", "h", time.Hour), "c1"),
		// The kubelet plugin removed the label, for example while restarting.
		cliqueNode("unlabeled", "", true), inPartition(daemonPod("unlabeled", "h", time.Hour), "c0"),
	)

	f.sync(t)

	assert.Equal(t, []string{"imex-moved"}, f.deletedPods(), "OnDelete does not hold back realigning a moved daemon")

	// Once its pod is gone, the moved daemon leaves the old clique.
	f.sync(t)
	got, err := f.nv.ResourceV1beta1().ComputeDomainCliques("driver").Get(context.Background(), clique.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, got.Daemons, 1)
	assert.Equal(t, "unlabeled", got.Daemons[0].NodeName)
}

func TestComputeDomainDaemonSetIgnoresDriverTemplateChanges(t *testing.T) {
	f := newCDDSFixture(t, testComputeDomainDaemonSet())
	f.sync(t)
	ds := f.daemonSet(t)

	// A driver upgrade renders a different pod template.
	f.m.config.imagePullSecretNames = []string{"new-registry"}
	f.core.ClearActions()
	f.sync(t)

	for _, action := range f.core.Actions() {
		assert.NotEqual(t, "update", action.GetVerb(), "driver upgrades do not touch the daemons")
	}
	assert.Equal(t, ds.Spec.Template, f.daemonSet(t).Spec.Template)
	assert.True(t, meta.IsStatusConditionTrue(f.cdds(t).Status.Conditions, nvapi.ComputeDomainDaemonSetConditionTemplateOutdated))

	// The next spec change applies it.
	f.update(t, func(cdds *nvapi.ComputeDomainDaemonSet) { cdds.Spec.LogVerbosity = 2 })
	f.sync(t)
	assert.Equal(t, []corev1.LocalObjectReference{{Name: "new-registry"}}, f.daemonSet(t).Spec.Template.Spec.ImagePullSecrets)
	assert.False(t, meta.IsStatusConditionTrue(f.cdds(t).Status.Conditions, nvapi.ComputeDomainDaemonSetConditionTemplateOutdated))
}

func TestComputeDomainDaemonSetRevertsEdits(t *testing.T) {
	f := newCDDSFixture(t, testComputeDomainDaemonSet())
	f.sync(t)

	edited := f.daemonSet(t)
	edited.Spec.Template.Spec.Containers[0].Image = "someone/else:latest"
	_, err := f.core.AppsV1().DaemonSets("driver").Update(context.Background(), edited, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.refresh(t)
	f.sync(t)

	ds := f.daemonSet(t)
	assert.Equal(t, "driver:v1", ds.Spec.Template.Spec.Containers[0].Image)
	assert.Equal(t, strconv.FormatInt(ds.Generation, 10), ds.Annotations[staticDaemonAppliedGenerationKey])

	f.core.ClearActions()
	f.sync(t)
	for _, action := range f.core.Actions() {
		assert.NotEqual(t, "update", action.GetVerb(), "a reverted DaemonSet is left alone")
	}
}

func TestComputeDomainDaemonSetHoldsBackRemovingDaemonsInUse(t *testing.T) {
	inPool := func(node *corev1.Node, pool string) *corev1.Node {
		node.Labels["pool"] = pool
		return node
	}
	f := newCDDSFixture(t, testComputeDomainDaemonSet(),
		inPool(cliqueNode("n0", "c0", true), "a"), daemonPod("n0", "h", time.Hour),
		inPool(cliqueNode("n1", "c0", true), "b"), daemonPod("n1", "h", time.Hour),
	)
	f.sync(t)
	f.claimNodes = map[string]bool{"n1": true}

	f.update(t, func(cdds *nvapi.ComputeDomainDaemonSet) { cdds.Spec.NodeSelector = map[string]string{"pool": "a"} })
	f.sync(t)

	assert.Empty(t, f.daemonSet(t).Spec.Template.Spec.NodeSelector, "the DaemonSet would remove the daemon of n1 right away")
	blocked := meta.FindStatusCondition(f.cdds(t).Status.Conditions, nvapi.ComputeDomainDaemonSetConditionUpdateBlocked)
	require.NotNil(t, blocked)
	assert.Equal(t, metav1.ConditionTrue, blocked.Status)
	assert.Contains(t, blocked.Message, "n1")

	f.claimNodes = nil
	f.sync(t)
	assert.Equal(t, map[string]string{"pool": "a"}, f.daemonSet(t).Spec.Template.Spec.NodeSelector)
	assert.False(t, meta.IsStatusConditionTrue(f.cdds(t).Status.Conditions, nvapi.ComputeDomainDaemonSetConditionUpdateBlocked))
}

func TestComputeDomainDaemonSetInvalidSpec(t *testing.T) {
	cdds := testComputeDomainDaemonSet()
	cdds.Spec.NodeSelector = map[string]string{"not a label": "x"}
	cdds.Spec.IMEXConfig = map[string]string{"IMEX_CMD_BIND_INTERFACE_IP": "10.0.0.1", "IMEX_SERVER_PORT": "1,2"}
	f := newCDDSFixture(t, cdds)

	f.sync(t)

	_, err := f.core.AppsV1().DaemonSets("driver").Get(context.Background(), staticDaemonSetName("cdds-uid"), metav1.GetOptions{})
	assert.Error(t, err, "an invalid spec is not applied")
	degraded := meta.FindStatusCondition(f.cdds(t).Status.Conditions, nvapi.ComputeDomainDaemonSetConditionDegraded)
	require.NotNil(t, degraded)
	assert.Equal(t, "InvalidSpec", degraded.Reason)
	assert.Contains(t, degraded.Message, "spec.nodeSelector")
	assert.Contains(t, degraded.Message, "IMEX_CMD_BIND_INTERFACE_IP")
	assert.Contains(t, degraded.Message, "must not contain a comma")
}

func TestComputeDomainDaemonSetRendersSpecValuesVerbatim(t *testing.T) {
	cdds := testComputeDomainDaemonSet()
	cdds.Spec.IMEXConfig = map[string]string{"B": `"quoted": value`, "A": "1"}
	f := newCDDSFixture(t, cdds)

	f.sync(t)

	env := f.daemonSet(t).Spec.Template.Spec.Containers[0].Env
	assert.Contains(t, env, corev1.EnvVar{Name: "IMEX_CONFIG_OVERRIDES", Value: `A=1,B="quoted": value,`}, "values cannot change the rendered YAML")
}

func TestPodPlacedOn(t *testing.T) {
	f := newCDDSFixture(t, testComputeDomainDaemonSet())
	ds, err := f.m.renderDaemonSet(testComputeDomainDaemonSet(), "rct", nil)
	require.NoError(t, err)
	spec := &ds.Spec.Template.Spec

	assert.True(t, podPlacedOn(spec, cliqueNode("n0", "c0", true)))
	assert.False(t, podPlacedOn(spec, cliqueNode("n1", "", true)), "only nodes of a clique")
	legacy := cliqueNode("n2", "c0", true)
	legacy.Labels[computeDomainLabelKey] = "cd-uid"
	assert.False(t, podPlacedOn(spec, legacy), "not while a per-ComputeDomain IMEX daemon runs there")
}

func TestDeleteLegacyDaemons(t *testing.T) {
	labels := map[string]string{computeDomainLabelKey: "cd-a", computeDomainResourceClaimTemplateTargetLabelKey: computeDomainResourceClaimTemplateTargetDaemon}
	legacyDS := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "computedomain-daemon-cd-a", Namespace: "driver", Labels: labels, Finalizers: []string{computeDomainFinalizer}}}
	legacyRCT := &resourceapi.ResourceClaimTemplate{ObjectMeta: metav1.ObjectMeta{Name: "computedomain-daemon-cd-a", Namespace: "driver", Labels: labels, Finalizers: []string{computeDomainFinalizer}}}
	otherDS := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "computedomain-daemon-cd-b", Namespace: "driver", Labels: map[string]string{computeDomainLabelKey: "cd-b"}}}
	client := fake.NewSimpleClientset(legacyDS, legacyRCT, otherDS)
	m := &ComputeDomainManager{config: &ManagerConfig{driverNamespace: "driver", clientsets: flags.ClientSets{Core: client}}}

	require.NoError(t, m.deleteLegacyDaemons(context.Background(), "cd-a"))

	_, err := client.AppsV1().DaemonSets("driver").Get(context.Background(), legacyDS.Name, metav1.GetOptions{})
	assert.Error(t, err, "the per-ComputeDomain DaemonSet is deleted, finalizer included")
	_, err = client.ResourceV1().ResourceClaimTemplates("driver").Get(context.Background(), legacyRCT.Name, metav1.GetOptions{})
	assert.Error(t, err)
	_, err = client.AppsV1().DaemonSets("driver").Get(context.Background(), otherDS.Name, metav1.GetOptions{})
	assert.NoError(t, err, "other ComputeDomains keep theirs")
}

func TestReportStaticDaemonsDisabled(t *testing.T) {
	nv := nvfake.NewSimpleClientset(testComputeDomainDaemonSet())
	config := &ManagerConfig{
		imexConfig: imex.Config{Mode: imex.ModeDriverManaged},
		clientsets: flags.ClientSets{Nvidia: nv},
	}

	reportStaticDaemonsDisabled(context.Background(), config)

	cdds, err := nv.ResourceV1beta1().ComputeDomainDaemonSets().Get(context.Background(), nvapi.ComputeDomainDaemonSetName, metav1.GetOptions{})
	require.NoError(t, err)
	degraded := meta.FindStatusCondition(cdds.Status.Conditions, nvapi.ComputeDomainDaemonSetConditionDegraded)
	require.NotNil(t, degraded)
	assert.Equal(t, "StaticDaemonsDisabled", degraded.Reason)

	// Nothing to report without one.
	reportStaticDaemonsDisabled(context.Background(), &ManagerConfig{clientsets: flags.ClientSets{Nvidia: nvfake.NewSimpleClientset()}})
}

func TestPlanCliqueRolloutDefaultsMinReadySeconds(t *testing.T) {
	nodes, pods := testCliques(2, 1, "old")
	pods[0].Annotations[staticDaemonTemplateHashKey] = "new"
	pods[0].Status.Conditions[0].LastTransitionTime = metav1.NewTime(rolloutNow.Add(-10 * time.Second))
	cliques, unclustered := groupByClique(nodes, pods, nil)

	plan := planCliqueRollout(cliques, unclustered, "new", nil, nodeReadiness(nodes), nil, rolloutNow)

	assert.Empty(t, plan.cliques, "an omitted rollingUpdate still waits 30s")
	assert.Equal(t, 20*time.Second, plan.requeueAfter)
}
