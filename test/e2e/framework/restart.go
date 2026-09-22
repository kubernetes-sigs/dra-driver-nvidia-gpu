// Copyright The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package framework

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

// KubeletPluginSelector matches the gpu-kubelet-plugin DaemonSet pods. The
// Helm chart labels every component pod with "<chart>-component: <name>";
// with the default (non-overridden) chart name this is the label below.
const KubeletPluginSelector = "dra-driver-nvidia-gpu-component=kubelet-plugin"

// RestartKubeletPlugin deletes the kubelet-plugin pod on the given node and
// waits for its DaemonSet-managed replacement to become Ready. It exercises
// the driver's checkpoint-recovery path: prepared claims must survive a
// plugin process restart. Returns an error if no plugin pod is found or the
// replacement never becomes Ready.
func RestartKubeletPlugin(ctx context.Context, cs *kubernetes.Clientset, node string, timeout time.Duration) error {
	nodeSel := fields.OneTermEqualSelector("spec.nodeName", node).String()
	list, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		LabelSelector: KubeletPluginSelector,
		FieldSelector: nodeSel,
	})
	if err != nil {
		return fmt.Errorf("list kubelet-plugin pods on node %s: %w", node, err)
	}
	if len(list.Items) == 0 {
		return fmt.Errorf("no kubelet-plugin pod (%s) found on node %s", KubeletPluginSelector, node)
	}

	old := list.Items[0]
	if err := cs.CoreV1().Pods(old.Namespace).Delete(ctx, old.Name, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("delete kubelet-plugin pod %s/%s: %w", old.Namespace, old.Name, err)
	}

	return wait.PollUntilContextTimeout(ctx, DefaultPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		pods, err := cs.CoreV1().Pods(old.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: KubeletPluginSelector,
			FieldSelector: nodeSel,
		})
		if err != nil {
			return false, err
		}
		for _, p := range pods.Items {
			// Skip the terminating old pod and any not-yet-scheduled shell.
			if p.UID == old.UID || p.Status.Phase != corev1.PodRunning {
				continue
			}
			ready := len(p.Status.ContainerStatuses) > 0
			for _, c := range p.Status.ContainerStatuses {
				if !c.Ready {
					ready = false
					break
				}
			}
			if ready {
				return true, nil
			}
		}
		return false, nil
	})
}
