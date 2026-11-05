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
	"sync"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// PodReadinessCallback is called with the current pod readiness state.
type PodReadinessCallback func(ctx context.Context, ready bool) error

// PodManager watches for changes to its own pod and reports readiness.
type PodManager struct {
	config             *ManagerConfig
	updatePodReadiness PodReadinessCallback
	waitGroup          sync.WaitGroup
	cancelContext      context.CancelFunc
	podInformer        cache.SharedIndexInformer
	informerFactory    informers.SharedInformerFactory

	// ready is the readiness last reported, nil before the first report.
	ready atomic.Pointer[bool]
}

// NewPodManager creates a new PodManager instance.
func NewPodManager(config *ManagerConfig, updatePodReadiness PodReadinessCallback) *PodManager {
	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		config.clientsets.Core,
		informerResyncPeriod,
		informers.WithNamespace(config.podNamespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = fmt.Sprintf("metadata.name=%s", config.podName)
		}),
	)

	podInformer := informerFactory.Core().V1().Pods().Informer()

	p := &PodManager{
		config:             config,
		updatePodReadiness: updatePodReadiness,
		podInformer:        podInformer,
		informerFactory:    informerFactory,
	}

	return p
}

// Start starts the pod manager.
func (pm *PodManager) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	pm.cancelContext = cancel

	_, err := pm.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		// Use `WithKey` with hard-coded key, to cancel any previous update task
		// (we want to make sure that the latest pod status update wins).
		AddFunc: func(obj any) {
			pm.config.workQueue.EnqueueWithKey(obj, "pod", pm.addOrUpdate)
		},
		UpdateFunc: func(oldObj, newObj any) {
			pm.config.workQueue.EnqueueWithKey(newObj, "pod", pm.addOrUpdate)
		},
	})
	if err != nil {
		return fmt.Errorf("error adding event handlers for Pod informer: %w", err)
	}

	pm.waitGroup.Add(1)
	go func() {
		defer pm.waitGroup.Done()
		pm.informerFactory.Start(ctx.Done())
	}()

	if !cache.WaitForCacheSync(ctx.Done(), pm.podInformer.HasSynced) {
		return fmt.Errorf("informer cache sync for Pod failed")
	}

	return nil
}

// Stop stops the pod manager.
func (pm *PodManager) Stop() error {
	if pm.cancelContext != nil {
		pm.cancelContext()
	}

	pm.waitGroup.Wait()
	klog.Infof("Terminating: pod manager")
	return nil
}

// addOrUpdate handles pod add/update events and reports readiness.
func (pm *PodManager) addOrUpdate(ctx context.Context, obj any) error {
	// Re-pull the pod from the cache to get the latest version
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		return fmt.Errorf("error getting cache key: %w", err)
	}
	obj, exists, err := pm.podInformer.GetStore().GetByKey(key)
	if err != nil {
		return fmt.Errorf("error getting pod from cache: %w", err)
	}
	if !exists {
		return nil
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("failed to cast object to Pod")
	}

	ready := pm.isPodReady(pod)
	pm.ready.Store(&ready)
	if err := pm.updatePodReadiness(ctx, ready); err != nil {
		return fmt.Errorf("pod readiness callback failed: %w", err)
	}

	return nil
}

// lastReadiness returns the readiness last reported, and whether there was
// one.
func (pm *PodManager) lastReadiness() (ready, known bool) {
	if p := pm.ready.Load(); p != nil {
		return *p, true
	}
	return false, false
}

// isPodReady determines if a pod is ready based on its conditions.
func (pm *PodManager) isPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}

	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}
