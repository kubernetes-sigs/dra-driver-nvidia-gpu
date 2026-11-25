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

// ExecInPod runs cmd in the given pod/container and returns its stdout and
// stderr separately so callers can surface each in failure messages.
func ExecInPod(ctx context.Context, cfg *rest.Config, cs *kubernetes.Clientset, ns, pod, container string, cmd []string) (stdout, stderr string, err error) {
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
		return "", "", fmt.Errorf("new SPDY executor: %w", err)
	}

	// Separate buffers: Stdout and Stderr stream concurrently, so sharing one
	// buffer would race and interleave the two streams.
	var outBuf, errBuf bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &outBuf, Stderr: &errBuf})
	if err != nil {
		return outBuf.String(), errBuf.String(), fmt.Errorf("exec %v in %s/%s[%s]: %w", cmd, ns, pod, container, err)
	}
	return outBuf.String(), errBuf.String(), nil
}
