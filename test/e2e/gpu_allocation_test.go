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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/dra-driver-nvidia-gpu/test/e2e/framework"
)

// Six DRA allocation scenarios. Each It() is independent; BeforeEach
// creates a unique namespace and AfterEach deletes it.

var _ = Describe("GPU Allocation", func() {
	var ns string

	BeforeEach(func() {
		ns = fmt.Sprintf("gpu-e2e-%d", time.Now().UnixNano()%1_000_000)
	})

	AfterEach(func(ctx SpecContext) {
		_ = cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
	})

	// BeforeSuite has already detected a ResourceSlice to run at all; this
	// It() just surfaces the assertion in the JUnit report.
	It("[install] driver publishes a ResourceSlice with gpu.nvidia.com", func(ctx SpecContext) {
		Expect(gpu.ProductName).NotTo(BeEmpty())
		Expect(gpu.DriverVersion).NotTo(BeEmpty())
		Expect(gpu.Memory.Value()).To(BeNumerically(">", 0))
	})

	It("[cel/productName] schedules a pod via product-name regex selector", func(ctx SpecContext) {
		yaml, err := framework.Render("product-type", map[string]any{
			"Namespace":      ns,
			"ProductPattern": framework.LowerKebab(gpu.ProductName),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(framework.ApplyYAML(ctx, yaml)).To(Succeed())

		phase, err := framework.WaitForPodPhase(ctx, cs, ns, "pod1",
			[]corev1.PodPhase{corev1.PodRunning, corev1.PodSucceeded},
			3*time.Minute)
		Expect(err).NotTo(HaveOccurred(), "pod1 never reached Running/Succeeded (phase=%s)", phase)

		claims, err := cs.ResourceV1().ResourceClaims(ns).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(claims.Items).NotTo(BeEmpty())
		Expect(claims.Items[0].Status.Allocation).NotTo(BeNil(),
			"claim %s has no allocation", claims.Items[0].Name)
	})

	It("[cel/driverVersion] schedules a pod via driver-version semver", func(ctx SpecContext) {
		yaml, err := framework.Render("driver-version", map[string]any{
			"Namespace":     ns,
			"DriverVersion": gpu.DriverVersion,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(framework.ApplyYAML(ctx, yaml)).To(Succeed())

		phase, err := framework.WaitForPodPhase(ctx, cs, ns, "test-gpu-driver-version",
			[]corev1.PodPhase{corev1.PodRunning, corev1.PodSucceeded},
			3*time.Minute)
		Expect(err).NotTo(HaveOccurred(), "pod never reached Running/Succeeded (phase=%s)", phase)
	})

	It("[cel/memory] schedules a pod via capacity.memory selector", func(ctx SpecContext) {
		// Request 90% of detected memory so the selector is non-trivial but
		// still satisfiable on the actual GPU.
		threshold := (gpu.MemoryGiB * 9) / 10
		if threshold < 1 {
			Skip(fmt.Sprintf("GPU memory too small for this test (%d Gi)", gpu.MemoryGiB))
		}
		yaml, err := framework.Render("memory-size", map[string]any{
			"Namespace":        ns,
			"RequiredMemoryGi": threshold,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(framework.ApplyYAML(ctx, yaml)).To(Succeed())

		phase, err := framework.WaitForPodPhase(ctx, cs, ns, "test-gpu-memory",
			[]corev1.PodPhase{corev1.PodRunning, corev1.PodSucceeded},
			3*time.Minute)
		Expect(err).NotTo(HaveOccurred(), "pod never reached Running/Succeeded (phase=%s)", phase)
	})

	It("[sharing] N pods share one ResourceClaim", func(ctx SpecContext) {
		const replicas = 4
		yaml, err := framework.Render("timeslicing", map[string]any{
			"Namespace": ns,
			"Replicas":  replicas,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(framework.ApplyYAML(ctx, yaml)).To(Succeed())

		// All four consumers must reach Ready; the Python suite's 2-of-4
		// tolerance applied to log-grep visibility, not to pod count.
		got, err := framework.WaitForPodsReady(ctx, cs, ns, "app=gpu-test", replicas, 5*time.Minute)
		Expect(err).NotTo(HaveOccurred(), "only %d/%d pods Ready", got, replicas)

		dep, err := cs.AppsV1().Deployments(ns).Get(ctx, "gpu-test", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(dep.Status.ReadyReplicas).To(BeEquivalentTo(replicas),
			"Deployment reports %d/%d ready", dep.Status.ReadyReplicas, replicas)

		// One allocation, reserved for every consumer.
		rc, err := cs.ResourceV1().ResourceClaims(ns).Get(ctx, "gpu-device", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(rc.Status.Allocation).NotTo(BeNil())
		Expect(len(rc.Status.ReservedFor)).To(BeEquivalentTo(replicas))
	})

	It("[negative] pod stays Pending when no device matches (h300)", func(ctx SpecContext) {
		yaml, err := framework.Render("error-handling", map[string]any{"Namespace": ns})
		Expect(err).NotTo(HaveOccurred())
		Expect(framework.ApplyYAML(ctx, yaml)).To(Succeed())

		// Let the scheduler settle, then assert the specific failure mode:
		// Pending + PodScheduled=False/Unschedulable with a DRA-related
		// message. Plain "not Running" would false-pass on ImagePullBackOff
		// or any other Pending cause.
		sleepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		<-sleepCtx.Done()

		pod, err := cs.CoreV1().Pods(ns).Get(ctx, "test-unavailable-gpu", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(pod.Status.Phase).To(Equal(corev1.PodPending),
			"pod must be Pending with no matching device; got phase=%s", pod.Status.Phase)

		var scheduled *corev1.PodCondition
		for i, c := range pod.Status.Conditions {
			if c.Type == corev1.PodScheduled {
				scheduled = &pod.Status.Conditions[i]
				break
			}
		}
		Expect(scheduled).NotTo(BeNil(), "PodScheduled condition missing")
		Expect(scheduled.Status).To(Equal(corev1.ConditionFalse),
			"PodScheduled must be False; got %s/%s", scheduled.Status, scheduled.Reason)
		Expect(scheduled.Reason).To(Equal("Unschedulable"))
		// The DRA scheduler plugin's verbatim phrase when a claim cannot
		// be satisfied is "cannot allocate all claims"; "allocate" is the
		// specific-enough signal to distinguish DRA rejection from a
		// generic taint/affinity Unschedulable.
		msg := strings.ToLower(scheduled.Message)
		Expect(msg).To(ContainSubstring("allocate"),
			"Unschedulable message does not mention DRA allocation: %q", scheduled.Message)

		claims, err := cs.ResourceV1().ResourceClaims(ns).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		for _, rc := range claims.Items {
			Expect(rc.Status.Allocation).To(BeNil(),
				"claim %s must not be allocated (no device matches h300)", rc.Name)
		}
	})

	It("[consumable-shares/unlimited] schedules multiple pods sharing the same GPU concurrently", func(ctx SpecContext) {
		slices, err := cs.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())

		hasConsumableShares := false
		for _, s := range slices.Items {
			if s.Spec.Driver != "gpu.nvidia.com" {
				continue
			}
			for _, d := range s.Spec.Devices {
				if d.AllowMultipleAllocations != nil && *d.AllowMultipleAllocations {
					hasConsumableShares = true
					break
				}
			}
		}
		if !hasConsumableShares {
			Skip("ConsumableShares feature is not enabled on the deployed cluster driver")
		}

		yaml, err := framework.Render("consumable-shares-unlimited", map[string]any{"Namespace": ns})
		Expect(err).NotTo(HaveOccurred())
		Expect(framework.ApplyYAML(ctx, yaml)).To(Succeed())

		for _, podName := range []string{"pod1", "pod2"} {
			phase, err := framework.WaitForPodPhase(ctx, cs, ns, podName,
				[]corev1.PodPhase{corev1.PodRunning, corev1.PodSucceeded},
				3*time.Minute)
			Expect(err).NotTo(HaveOccurred(), "pod %s never reached Running/Succeeded (phase=%s)", podName, phase)
		}

		claims, err := cs.ResourceV1().ResourceClaims(ns).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(claims.Items)).To(BeNumerically(">=", 2))
	})

	It("[consumable-shares/memory] schedules multiple pods with explicit fractional memory requests", func(ctx SpecContext) {
		slices, err := cs.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())

		hasConsumableShares := false
		for _, s := range slices.Items {
			if s.Spec.Driver != "gpu.nvidia.com" {
				continue
			}
			for _, d := range s.Spec.Devices {
				if d.AllowMultipleAllocations != nil && *d.AllowMultipleAllocations {
					hasConsumableShares = true
					break
				}
			}
		}
		if !hasConsumableShares {
			Skip("ConsumableShares feature is not enabled on the deployed cluster driver")
		}

		yaml, err := framework.Render("consumable-shares-memory", map[string]any{"Namespace": ns})
		Expect(err).NotTo(HaveOccurred())
		Expect(framework.ApplyYAML(ctx, yaml)).To(Succeed())

		for _, podName := range []string{"pod1", "pod2"} {
			phase, err := framework.WaitForPodPhase(ctx, cs, ns, podName,
				[]corev1.PodPhase{corev1.PodRunning, corev1.PodSucceeded},
				3*time.Minute)
			Expect(err).NotTo(HaveOccurred(), "pod %s never reached Running/Succeeded (phase=%s)", podName, phase)
		}
	})

	It("[mps/decoupled] schedules multiple pods sharing GPU via decoupled node-level MPS", func(ctx SpecContext) {
		slices, err := cs.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())

		hasConsumableShares := false
		for _, s := range slices.Items {
			if s.Spec.Driver != "gpu.nvidia.com" {
				continue
			}
			for _, d := range s.Spec.Devices {
				if d.AllowMultipleAllocations != nil && *d.AllowMultipleAllocations {
					hasConsumableShares = true
					break
				}
			}
		}
		if !hasConsumableShares {
			Skip("ConsumableShares feature is not enabled on the deployed cluster driver")
		}

		yaml, err := framework.Render("mps-decoupled", map[string]any{"Namespace": ns})
		Expect(err).NotTo(HaveOccurred())
		if err := framework.ApplyYAML(ctx, yaml); err != nil {
			if strings.Contains(err.Error(), "MPSSupport") {
				Skip("MPSSupport feature gate is not enabled on the deployed cluster driver")
			}
			Expect(err).NotTo(HaveOccurred())
		}

		for _, podName := range []string{"pod1", "pod2"} {
			phase, err := framework.WaitForPodPhase(ctx, cs, ns, podName,
				[]corev1.PodPhase{corev1.PodRunning, corev1.PodSucceeded},
				3*time.Minute)
			Expect(err).NotTo(HaveOccurred(), "pod %s never reached Running/Succeeded (phase=%s)", podName, phase)
		}
	})
})
