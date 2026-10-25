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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
