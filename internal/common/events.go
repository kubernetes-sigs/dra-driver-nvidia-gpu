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

package common

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// EmitCheckpointCorruptionEvent reports checkpoint corruption against the
// kubelet plugin Pod. Event creation is best-effort and must not prevent plugin
// startup.
func EmitCheckpointCorruptionEvent(
	ctx context.Context,
	core coreclientset.Interface,
	component string,
	nodeName string,
	podName string,
	namespace string,
	message string,
) {
	if core == nil {
		klog.Warning("skip checkpoint corruption event: no core clientset available")
		return
	}
	if podName == "" {
		klog.Warning("skip checkpoint corruption event: pod name is empty")
		return
	}

	now := metav1.Now()
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName + "-checkpoint-corrupted",
			Namespace: namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: "v1",
			Kind:       "Pod",
			Name:       podName,
			Namespace:  namespace,
		},
		Reason:  "CheckpointCorrupted",
		Message: message,
		Type:    corev1.EventTypeWarning,
		Source: corev1.EventSource{
			Component: component,
			Host:      nodeName,
		},
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}

	eventCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if _, err := core.CoreV1().Events(namespace).Create(
		eventCtx,
		event,
		metav1.CreateOptions{},
	); err != nil && !apierrors.IsAlreadyExists(err) {
		klog.Errorf(
			"failed to emit checkpoint corruption event for pod %q: %v",
			podName,
			err,
		)
	}
}
