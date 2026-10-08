// Copyright The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"regexp"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/dra-driver-nvidia-gpu/test/e2e/framework"
)

// End-to-end coverage for the PassthroughSupport (VFIO) feature. Every spec
// skips unless the deployed driver publishes at least one vfio device, so the
// suite stays green on GPU-only clusters and CI lanes without IOMMU. The
// container never uses the passthrough GPU directly (it is bound to vfio-pci
// for VM consumption); the assertion is that DRA schedules the pod and the
// claim is allocated end to end.
var _ = Describe("VFIO Allocation", func() {
	var ns string

	BeforeEach(func(ctx SpecContext) {
		requireVfio(ctx)
		ns = fmt.Sprintf("vfio-e2e-%d", time.Now().UnixNano()%1_000_000)
	})

	AfterEach(func(ctx SpecContext) {
		_ = cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
	})

	It("[vfio] 1-GPU passthrough claim is allocated", func(ctx SpecContext) {
		applyVfio(ctx, "vfio-single", map[string]any{"Namespace": ns})
		expectPodRunning(ctx, ns, "pod1")
		expectAllocatedClaims(ctx, ns, 1)
	})

	It("[vfio] passthrough is injected into the container", func(ctx SpecContext) {
		applyVfio(ctx, "vfio-single", map[string]any{"Namespace": ns})
		expectPodRunning(ctx, ns, "pod1")
		expectAllocatedClaims(ctx, ns, 1)
		expectPassthroughInPod(ctx, ns, "pod1", "ctr")
	})

	It("[vfio] gpu view disappears during passthrough and returns after release", func(ctx SpecContext) {
		before := countGPUDevices(ctx)

		applyVfio(ctx, "vfio-single", map[string]any{"Namespace": ns})
		expectPodRunning(ctx, ns, "pod1")
		expectAllocatedClaims(ctx, ns, 1)

		// The passthrough GPU is bound to vfio-pci, so its `gpu` view is removed
		// from the ResourceSlice while the claim is prepared.
		expectGPUDeviceCount(ctx, before-1)

		// Releasing the claim (deleting the pod and its generated claim) must
		// switch the GPU back to the nvidia driver and republish its `gpu` view.
		deletePod(ctx, ns, "pod1")
		expectGPUDeviceCount(ctx, before)
	})

	It("[vfio] N-GPU passthrough claims are allocated to distinct devices", func(ctx SpecContext) {
		const replicas = 2
		requireVfioDevices(ctx, replicas)

		applyVfio(ctx, "vfio-multi", map[string]any{"Namespace": ns, "Replicas": replicas})
		got, err := framework.WaitForPodsReady(ctx, cs, ns, "app=vfio-test", replicas, 5*time.Minute)
		Expect(err).NotTo(HaveOccurred(), "only %d/%d vfio pods Ready", got, replicas)

		expectAllocatedClaims(ctx, ns, replicas)
		expectDistinctDevices(ctx, ns)
	})

	It("[vfio/config] opaque VfioDeviceConfig claim is allocated", func(ctx SpecContext) {
		applyVfio(ctx, "vfio-config", map[string]any{"Namespace": ns})
		expectPodRunning(ctx, ns, "pod1")
		expectAllocatedClaims(ctx, ns, 1)
	})

	It("[vfio/mixed] gpu and vfio workloads coexist", func(ctx SpecContext) {
		// The gpu and vfio devices for one physical GPU are the same silicon
		// and mutually exclusive, so the two workloads need two GPUs to land on.
		requireGPUs(ctx, 2)

		// Apply the two claims sequentially, not in one manifest. If both pods
		// are scheduled at once the scheduler can place them on the same GPU
		// (gpu-0 and gpu-vfio-0 are the same silicon); kubelet then serializes
		// Prepare, the losing claim's device is gone, and its pod never runs.
		// Preparing the vfio claim first binds that GPU to vfio-pci and removes
		// its `gpu` sibling, so the full-GPU pod can only land on the other GPU.
		applyVfio(ctx, "mixed-vfio", map[string]any{"Namespace": ns})
		expectPodRunning(ctx, ns, "pod-vfio")
		expectAllocatedClaims(ctx, ns, 1)

		applyVfio(ctx, "mixed-gpu", map[string]any{"Namespace": ns})
		expectPodRunning(ctx, ns, "pod-gpu")
		expectAllocatedClaims(ctx, ns, 2)
	})

	It("[vfio] 1-GPU passthrough claim survives a plugin restart", func(ctx SpecContext) {
		// Capture the baseline before preparing the claim: once the passthrough
		// GPU is bound to vfio-pci its `gpu` view is already gone, so sampling
		// after the pod is Running would record N-1, not N.
		before := countGPUDevices(ctx)

		applyVfio(ctx, "vfio-single", map[string]any{"Namespace": ns})
		expectPodRunning(ctx, ns, "pod1")
		expectAllocatedClaims(ctx, ns, 1)
		expectGPUDeviceCount(ctx, before-1)

		Expect(framework.RestartKubeletPlugin(ctx, cs, podNode(ctx, ns, "pod1"), 5*time.Minute)).To(Succeed())

		// The prepared claim must still be allocated and the pod still Running
		// after the plugin recovers its checkpoint.
		expectAllocatedClaims(ctx, ns, 1)
		expectPodRunning(ctx, ns, "pod1")

		// The passthrough GPU's `gpu` view must stay absent across the restart;
		// recovery must not republish a device that is still bound to vfio-pci.
		expectGPUDeviceCount(ctx, before-1)

		// Releasing the claim after a restart must switch the GPU back to the
		// nvidia driver and republish its `gpu` view, proving teardown works
		// against recovered checkpoint state.
		deletePod(ctx, ns, "pod1")
		expectGPUDeviceCount(ctx, before)
	})

	It("[vfio] N-GPU passthrough claims survive a plugin restart", func(ctx SpecContext) {
		const replicas = 2
		requireVfioDevices(ctx, replicas)

		applyVfio(ctx, "vfio-multi", map[string]any{"Namespace": ns, "Replicas": replicas})
		got, err := framework.WaitForPodsReady(ctx, cs, ns, "app=vfio-test", replicas, 5*time.Minute)
		Expect(err).NotTo(HaveOccurred(), "only %d/%d vfio pods Ready", got, replicas)
		expectAllocatedClaims(ctx, ns, replicas)

		Expect(framework.RestartKubeletPlugin(ctx, cs, firstPodNode(ctx, ns, "app=vfio-test"), 5*time.Minute)).To(Succeed())

		got, err = framework.WaitForPodsReady(ctx, cs, ns, "app=vfio-test", replicas, 5*time.Minute)
		Expect(err).NotTo(HaveOccurred(), "only %d/%d vfio pods Ready after restart", got, replicas)
		expectAllocatedClaims(ctx, ns, replicas)
		expectDistinctDevices(ctx, ns)
	})
})

// countVfioDevices returns the number of published vfio devices across all
// gpu.nvidia.com ResourceSlices.
func countVfioDevices(ctx context.Context) int {
	slices, err := cs.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())

	count := 0
	for _, s := range slices.Items {
		if s.Spec.Driver != "gpu.nvidia.com" {
			continue
		}
		for _, d := range s.Spec.Devices {
			if attr, ok := d.Attributes["type"]; ok && attr.StringValue != nil && *attr.StringValue == "vfio" {
				count++
			}
		}
	}
	return count
}

// requireVfio skips the spec unless the deployed driver publishes at least one
// vfio device (PassthroughSupport enabled + IOMMU-capable hardware).
func requireVfio(ctx context.Context) {
	if countVfioDevices(ctx) == 0 {
		Skip("PassthroughSupport (vfio) is not enabled or no vfio device is published on the deployed cluster")
	}
}

// requireVfioDevices skips the spec unless at least n vfio devices are published.
func requireVfioDevices(ctx context.Context, n int) {
	if got := countVfioDevices(ctx); got < n {
		Skip(fmt.Sprintf("need %d vfio devices, cluster publishes %d", n, got))
	}
}

// countGPUDevices returns the number of published gpu-type devices across all
// gpu.nvidia.com ResourceSlices.
func countGPUDevices(ctx context.Context) int {
	slices, err := cs.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())

	count := 0
	for _, s := range slices.Items {
		if s.Spec.Driver != "gpu.nvidia.com" {
			continue
		}
		for _, d := range s.Spec.Devices {
			if attr, ok := d.Attributes["type"]; ok && attr.StringValue != nil && *attr.StringValue == "gpu" {
				count++
			}
		}
	}
	return count
}

// requireGPUs skips the spec unless at least n gpu-type devices are published.
func requireGPUs(ctx context.Context, n int) {
	if got := countGPUDevices(ctx); got < n {
		Skip(fmt.Sprintf("need %d gpu devices, cluster publishes %d", n, got))
	}
}

// expectGPUDeviceCount waits for the published gpu-type device count to settle
// at want. The driver republishes its ResourceSlice asynchronously after a
// claim is prepared or released, so the assertion must be eventual.
func expectGPUDeviceCount(ctx context.Context, want int) {
	Eventually(func() int { return countGPUDevices(ctx) }, 2*time.Minute, 5*time.Second).
		Should(Equal(want), "expected %d gpu-type devices published", want)
}

// podNode returns the node a pod landed on. Preferred over gpu.NodeName for
// node-targeted actions: the pod's placement is authoritative, whereas
// gpu.NodeName is just the first GPU DetectGPU happened to report.
func podNode(ctx context.Context, ns, name string) string {
	pod, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "get pod %s/%s", ns, name)
	Expect(pod.Spec.NodeName).NotTo(BeEmpty(), "pod %s/%s not scheduled", ns, name)
	return pod.Spec.NodeName
}

// firstPodNode returns the node of the first pod matching labelSelector.
func firstPodNode(ctx context.Context, ns, labelSelector string) string {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	Expect(err).NotTo(HaveOccurred())
	Expect(pods.Items).NotTo(BeEmpty(), "no pods match %q in %s", labelSelector, ns)
	node := pods.Items[0].Spec.NodeName
	Expect(node).NotTo(BeEmpty(), "pod %s/%s not scheduled", ns, pods.Items[0].Name)
	return node
}

// deletePod deletes a pod and waits for it to be gone, which also releases its
// generated ResourceClaim (owned by the pod).
func deletePod(ctx context.Context, ns, name string) {
	Expect(cs.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{})).To(Succeed())
	Eventually(func() bool {
		_, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		return err != nil
	}, 2*time.Minute, 5*time.Second).Should(BeTrue(), "pod %s/%s never deleted", ns, name)
}

var vfioGroupNode = regexp.MustCompile(`(?m)^[0-9]+$`)

// expectPassthroughInPod execs into the container and proves the GPU was handed
// through as a VFIO device: a numeric /dev/vfio/<group> node is present and the
// nvidia-container-runtime was neutralized via NVIDIA_VISIBLE_DEVICES=void.
func expectPassthroughInPod(ctx context.Context, ns, pod, container string) {
	out, err := framework.ExecInPod(ctx, restConfig, cs, ns, pod, container,
		[]string{"sh", "-c", `printf 'ENV=%s\n' "$NVIDIA_VISIBLE_DEVICES"; ls /dev/vfio`})
	Expect(err).NotTo(HaveOccurred(), "exec in %s/%s: %s", ns, pod, out)
	Expect(out).To(ContainSubstring("ENV=void"), "NVIDIA_VISIBLE_DEVICES not void:\n%s", out)
	Expect(vfioGroupNode.MatchString(out)).To(BeTrue(), "no /dev/vfio/<group> node present:\n%s", out)
}

func applyVfio(ctx context.Context, tmpl string, vars map[string]any) {
	yaml, err := framework.Render(tmpl, vars)
	Expect(err).NotTo(HaveOccurred())
	Expect(framework.ApplyYAML(ctx, yaml)).To(Succeed())
}

func expectPodRunning(ctx context.Context, ns, name string) {
	phase, err := framework.WaitForPodPhase(ctx, cs, ns, name,
		[]corev1.PodPhase{corev1.PodRunning, corev1.PodSucceeded}, 3*time.Minute)
	Expect(err).NotTo(HaveOccurred(), "pod %s never reached Running/Succeeded (phase=%s)", name, phase)
}

func expectAllocatedClaims(ctx context.Context, ns string, want int) {
	claims, err := cs.ResourceV1().ResourceClaims(ns).List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())

	allocated := 0
	for _, rc := range claims.Items {
		if rc.Status.Allocation != nil {
			allocated++
		}
	}
	Expect(allocated).To(Equal(want),
		"expected %d allocated claims in %s, got %d", want, ns, allocated)
}

// expectDistinctDevices asserts every allocated claim in the namespace holds a
// different physical device (passthrough is exclusive, not shared).
func expectDistinctDevices(ctx context.Context, ns string) {
	claims, err := cs.ResourceV1().ResourceClaims(ns).List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())

	seen := map[string]string{}
	for _, rc := range claims.Items {
		Expect(rc.Status.Allocation).NotTo(BeNil(), "claim %s has no allocation", rc.Name)
		Expect(rc.Status.Allocation.Devices.Results).NotTo(BeEmpty(), "claim %s has no allocated devices", rc.Name)
		dev := rc.Status.Allocation.Devices.Results[0].Device
		Expect(dev).NotTo(BeEmpty(), "claim %s allocated device name is empty", rc.Name)
		if prev, ok := seen[dev]; ok {
			Fail(fmt.Sprintf("device %s allocated to both %s and %s; passthrough must be exclusive", dev, prev, rc.Name))
		}
		seen[dev] = rc.Name
	}
}
