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
	"strconv"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
	nvfake "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/clientset/versioned/fake"
)

func TestEnsureCliqueExistsLabelsDomain(t *testing.T) {
	tests := map[string]struct {
		labelKey string
	}{
		"ComputeDomain":          {labelKey: computeDomainLabelKey},
		"ComputeDomainDaemonSet": {labelKey: computeDomainDaemonSetLabelKey},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			client := nvfake.NewSimpleClientset()
			m := NewComputeDomainCliqueManager(&ManagerConfig{
				clientsets:        flags.ClientSets{Nvidia: client},
				computeDomainUUID: "domain-uid",
				cliqueID:          "clique-a",
				podNamespace:      "driver",
				podName:           "daemon",
				podUID:            "daemon-uid",
				domainLabelKey:    tc.labelKey,
			})
			m.mutationCache = cache.NewIntegerResourceVersionMutationCache(klog.Background(), m.informer.GetStore(), m.informer.GetIndexer(), mutationCacheTTL, true)

			require.NoError(t, m.ensureCliqueExists(context.Background()))

			clique, err := client.ResourceV1beta1().ComputeDomainCliques("driver").Get(context.Background(), "domain-uid.clique-a", metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, "domain-uid", clique.Labels[tc.labelKey])
			assert.Equal(t, "clique-a", clique.Labels[computeDomainCliqueLabelKey])
			assert.Len(t, clique.Labels, 2, "cliques of static IMEX daemons carry no ComputeDomain label")
		})
	}
}

func newTestCliqueManager(t *testing.T) (*ComputeDomainCliqueManager, *nvfake.Clientset) {
	t.Helper()
	client := nvfake.NewSimpleClientset()
	m := NewComputeDomainCliqueManager(&ManagerConfig{
		clientsets:            flags.ClientSets{Nvidia: client},
		computeDomainUUID:     "domain-uid",
		cliqueID:              "clique-a",
		podNamespace:          "driver",
		podName:               "daemon",
		podUID:                "daemon-uid",
		podIP:                 "10.0.0.1",
		nodeName:              "node-a",
		maxNodesPerIMEXDomain: 18,
		domainLabelKey:        computeDomainDaemonSetLabelKey,
	})
	m.mutationCache = cache.NewIntegerResourceVersionMutationCache(klog.Background(), m.informer.GetStore(), m.informer.GetIndexer(), mutationCacheTTL, true)
	return m, client
}

func TestRecreateDeletedClique(t *testing.T) {
	ctx := context.Background()
	m, client := newTestCliqueManager(t)
	cliques := client.ResourceV1beta1().ComputeDomainCliques("driver")

	require.NoError(t, m.ensureCliqueExists(ctx))
	require.NoError(t, cliques.Delete(ctx, "domain-uid.clique-a", metav1.DeleteOptions{}))
	require.NoError(t, m.ensureCliqueExists(ctx))
	_, err := cliques.Get(ctx, "domain-uid.clique-a", metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "the cache still holds the deleted clique")

	require.NoError(t, m.recreateClique(ctx, nil))
	clique, err := cliques.Get(ctx, "domain-uid.clique-a", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "domain-uid", clique.Labels[computeDomainDaemonSetLabelKey])
	assert.Equal(t, "daemon-uid", string(clique.OwnerReferences[0].UID))

	require.NoError(t, m.recreateClique(ctx, nil), "another daemon may have recreated it")
}

func TestReinsertedDaemonReportsReadiness(t *testing.T) {
	ctx := context.Background()
	m, client := newTestCliqueManager(t)
	// The API server bumps resourceVersion on every write, which the
	// mutation cache relies on to prefer our latest write.
	version := 1
	client.PrependReactor("update", "computedomaincliques", func(action k8stesting.Action) (bool, runtime.Object, error) {
		update, ok := action.(k8stesting.UpdateAction)
		if !ok {
			return false, nil, nil
		}
		obj, ok := update.GetObject().(*nvapi.ComputeDomainClique)
		if !ok {
			return false, nil, nil
		}
		version++
		obj.ResourceVersion = strconv.Itoa(version)
		return false, nil, nil
	})
	ready := true
	m.podManager.ready.Store(&ready)

	clique := m.newClique()
	clique.ResourceVersion = "1"
	_, err := client.ResourceV1beta1().ComputeDomainCliques("driver").Create(ctx, clique, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, m.informer.GetStore().Add(clique))
	go func() {
		for range m.updatedDaemonsChan {
		}
	}()
	defer close(m.updatedDaemonsChan)

	require.NoError(t, m.onAddOrUpdate(ctx, clique))
	got, err := client.ResourceV1beta1().ComputeDomainCliques("driver").Get(ctx, clique.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, got.Daemons, 1)
	assert.Equal(t, nvapi.ComputeDomainStatusReady, got.Daemons[0].Status, "a ready pod does not stay NotReady after its entry is inserted again")
}
