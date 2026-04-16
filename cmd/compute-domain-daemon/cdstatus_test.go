package main

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	nvapi "sigs.k8s.io/nvidia-dra-driver-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/nvidia-dra-driver-gpu/pkg/flags"
	nvfake "sigs.k8s.io/nvidia-dra-driver-gpu/pkg/nvidia.com/clientset/versioned/fake"
)

type noopMutationCache struct{}

func (n *noopMutationCache) GetByKey(string) (interface{}, bool, error) {
	return nil, false, nil
}

func (n *noopMutationCache) ByIndex(string, string) ([]interface{}, error) {
	return nil, nil
}

func (n *noopMutationCache) Mutation(interface{}) {}

var _ cache.MutationCache = &noopMutationCache{}

func TestSyncNodeInfoToCDPreservesExistingNodesAcrossConcurrentWriters(t *testing.T) {
	t.Parallel()

	const (
		namespace = "default"
		name      = "cd-foo"
		uid       = "cd-uid"
		cliqueID  = "clique-1"
	)

	cd := &nvapi.ComputeDomain{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			UID:       uid,
		},
		Spec: nvapi.ComputeDomainSpec{
			NumNodes: 2,
		},
	}

	client := nvfake.NewSimpleClientset(cd.DeepCopy())

	newManager := func(nodeName, podIP string) *ComputeDomainStatusManager {
		return &ComputeDomainStatusManager{
			config: &ManagerConfig{
				clientsets: flags.ClientSets{
					Nvidia: client,
				},
				nodeName:               nodeName,
				podIP:                  podIP,
				cliqueID:               cliqueID,
				computeDomainName:      name,
				computeDomainNamespace: namespace,
				computeDomainUUID:      uid,
				maxNodesPerIMEXDomain:  8,
			},
			mutationCache: &noopMutationCache{},
		}
	}

	managerA := newManager("nodeA", "10.0.0.1")
	managerB := newManager("nodeB", "10.0.0.2")

	// Both managers read the same initial snapshot before either updates the CD status.
	staleA, err := client.ResourceV1beta1().ComputeDomains(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get staleA: %v", err)
	}
	staleB, err := client.ResourceV1beta1().ComputeDomains(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get staleB: %v", err)
	}

	if _, err := managerA.syncNodeInfoToCD(context.Background(), staleA); err != nil {
		t.Fatalf("sync managerA: %v", err)
	}
	if _, err := managerB.syncNodeInfoToCD(context.Background(), staleB); err != nil {
		t.Fatalf("sync managerB: %v", err)
	}

	finalCD, err := client.ResourceV1beta1().ComputeDomains(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get final ComputeDomain: %v", err)
	}

	if got := len(finalCD.Status.Nodes); got != 2 {
		t.Fatalf("expected both node updates to be present, got %d nodes: %#v", got, finalCD.Status.Nodes)
	}
}
