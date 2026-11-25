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
