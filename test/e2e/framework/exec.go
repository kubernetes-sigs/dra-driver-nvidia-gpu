// Copyright The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package framework

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// ExecInPod runs cmd in the given pod/container and returns its combined stdout
// and stderr.
func ExecInPod(ctx context.Context, cfg *rest.Config, cs *kubernetes.Clientset, ns, pod, container string, cmd []string) (string, error) {
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(ns).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("new SPDY executor: %w", err)
	}

	var out bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &out, Stderr: &out})
	if err != nil {
		return out.String(), fmt.Errorf("exec %v in %s/%s[%s]: %w", cmd, ns, pod, container, err)
	}
	return out.String(), nil
}
