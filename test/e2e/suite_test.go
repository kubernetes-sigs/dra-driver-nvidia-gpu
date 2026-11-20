// Copyright The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

//go:build e2e

// Package e2e runs the DRA driver end-to-end suite against a running
// cluster that already has the driver installed plus at least one NVIDIA
// GPU. Invoked via `make test-e2e`, which passes -tags=e2e. Standard
// `go test ./...` skips this package.
package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"sigs.k8s.io/dra-driver-nvidia-gpu/test/e2e/framework"
)

var (
	cs         *kubernetes.Clientset
	restConfig *rest.Config
	gpu        *framework.GPUDetails
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "dra-driver-nvidia-gpu e2e suite")
}

var _ = BeforeSuite(func() {
	var err error
	cs, restConfig, err = framework.NewClientset()
	Expect(err).NotTo(HaveOccurred(), "build kubernetes client")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	gpu, err = framework.DetectGPU(ctx, cs)
	Expect(err).NotTo(HaveOccurred(), "detect GPU from ResourceSlice; is the DRA driver running?")

	fmt.Fprintf(GinkgoWriter,
		"Detected GPU: node=%s product=%q driver=%s memory=%s\n",
		gpu.NodeName, gpu.ProductName, gpu.DriverVersion, gpu.Memory.String())
})

func applyYAML(ctx context.Context, tmpl string, vars map[string]any) {
	yaml, err := framework.Render(tmpl, vars)
	Expect(err).NotTo(HaveOccurred())
	Expect(framework.ApplyYAML(ctx, yaml)).To(Succeed())
}

// podNode returns the node a pod landed on.
func podNode(ctx context.Context, ns, name string) string {
	pod, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "get pod %s/%s", ns, name)
	Expect(pod.Spec.NodeName).NotTo(BeEmpty(), "pod %s/%s not scheduled", ns, name)
	return pod.Spec.NodeName
}

// deletePod deletes a pod and waits for it to be gone, which also releases its
// generated ResourceClaim (owned by the pod).
func deletePod(ctx context.Context, ns, name string) {
	Expect(cs.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{})).To(Succeed())
	Eventually(func() bool {
		_, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}, 2*time.Minute, 5*time.Second).Should(BeTrue(), "pod %s/%s never deleted", ns, name)
}

// deleteNamespace deletes a namespace and waits for it to be fully gone.
// Deletion blocks on vfio claim unpreparation (driver switch is slow), so a
// leftover claim could otherwise race into the next spec.
func deleteNamespace(ctx context.Context, name string) {
	err := cs.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
	Expect(err == nil || apierrors.IsNotFound(err)).To(BeTrue(), "delete namespace %q: %v", name, err)
	Eventually(func() bool {
		_, err := cs.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}, 2*time.Minute, 5*time.Second).Should(BeTrue(), "namespace %q never deleted", name)
}
