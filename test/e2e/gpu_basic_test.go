// Copyright The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build e2e

package e2e

import (
	"fmt"
	"regexp"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/dra-driver-nvidia-gpu/test/e2e/framework"
)

// These cases migrate the allocation assertions from tests/bats/test_gpu_basic.bats.
// They assume the driver and its full-GPU inventory are already configured.
var _ = Describe("full GPU basic workloads", Label("gpu", "full-gpu"), func() {
	var ns string

	BeforeEach(func() {
		ns = fmt.Sprintf("gpu-e2e-basic-%d", time.Now().UnixNano()%1_000_000)
	})

	AfterEach(func(ctx SpecContext) {
		if ns != "" {
			_ = cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
		}
	})

	It("allocates distinct full GPUs to two pods", Label("fastfeedback", "multi-gpu"), func(ctx SpecContext) {
		requireFullGPUs(ctx, 2)
		applyWorkload(ctx, "two-pods-distinct-gpus", ns)

		uuid1 := podGPUUUID(ctx, ns, "pod1", "ctr")
		uuid2 := podGPUUUID(ctx, ns, "pod2", "ctr")
		Expect(uuid1).NotTo(Equal(uuid2), "independent full-GPU claims must receive distinct GPUs")
	})

	It("shares one full-GPU ResourceClaim between two pods", Label("fastfeedback"), func(ctx SpecContext) {
		requireFullGPUs(ctx, 1)
		applyWorkload(ctx, "two-pods-shared-gpu", ns)

		uuid1 := podGPUUUID(ctx, ns, "pod1", "ctr")
		uuid2 := podGPUUUID(ctx, ns, "pod2", "ctr")
		Expect(uuid1).To(Equal(uuid2), "both pods must receive the device from their shared ResourceClaim")

		claim, err := cs.ResourceV1().ResourceClaims(ns).Get(ctx, "rc-single-gpu-full", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(claim.Status.Allocation).NotTo(BeNil())
		Expect(claim.Status.ReservedFor).To(HaveLen(2))
	})

	It("shares one full-GPU ResourceClaimTemplate claim between two containers", Label("fastfeedback"), func(ctx SpecContext) {
		requireFullGPUs(ctx, 1)
		applyWorkload(ctx, "two-containers-shared-gpu", ns)

		uuid0 := podGPUUUID(ctx, ns, "pod1", "ctr0")
		uuid1 := podGPUUUID(ctx, ns, "pod1", "ctr1")
		Expect(uuid0).To(Equal(uuid1), "both containers must receive the device from their shared claim")
	})
})

var gpuUUIDPattern = regexp.MustCompile(`GPU-[[:xdigit:]-]+`)

func requireFullGPUs(ctx SpecContext, minimum int) {
	count, err := framework.CountDevicesByType(ctx, cs, "gpu")
	Expect(err).NotTo(HaveOccurred())
	if count < minimum {
		Skip(fmt.Sprintf("requires %d full GPU device(s), but ResourceSlices advertise %d", minimum, count))
	}
}

func applyWorkload(ctx SpecContext, name, namespace string) {
	yaml, err := framework.Render(name, map[string]any{"Namespace": namespace})
	Expect(err).NotTo(HaveOccurred())
	Expect(framework.ApplyYAML(ctx, yaml)).To(Succeed())
}

func podGPUUUID(ctx SpecContext, namespace, pod, container string) string {
	Expect(framework.WaitForPodReady(ctx, cs, namespace, pod, 3*time.Minute)).To(Succeed())

	logs, err := framework.PodLogs(ctx, cs, namespace, pod, container)
	Expect(err).NotTo(HaveOccurred())
	uuid := gpuUUIDPattern.FindAllString(logs, -1)
	Expect(uuid).To(HaveLen(1), "expected one full-GPU UUID in logs: %q", logs)

	return uuid[0]
}
