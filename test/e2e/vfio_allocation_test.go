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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"

	"sigs.k8s.io/dra-driver-nvidia-gpu/test/e2e/framework"
)

// End-to-end coverage for the PassthroughSupport (VFIO) feature. Every spec
// uses the GPU node detected by the suite and skips unless that node publishes
// at least one vfio device, so the suite stays green on GPU-only clusters
// and CI lanes without IOMMU. The
// container never uses the passthrough GPU directly (it is bound to vfio-pci
// for VM consumption); the assertion is that DRA schedules the pod and the
// claim is allocated end to end.
var _ = Describe("VFIO Allocation", Serial, Label("vfio"), func() {
	var ns string

	BeforeEach(func(ctx SpecContext) {
		// Assign ns before the skip check: Ginkgo still runs AfterEach for a
		// spec skipped here, and an empty ns makes Namespaces().Get fail with
		// a non-NotFound error, failing the skipped spec on GPU-only clusters.
		ns = fmt.Sprintf("vfio-e2e-%d", time.Now().UnixNano()%1_000_000)
		requireVfio(ctx)
	})

	AfterEach(func(ctx SpecContext) {
		deleteNamespace(ctx, ns)
	})

	It("[vfio] 1-GPU passthrough claim is allocated", func(ctx SpecContext) {
		applyYAML(ctx, "vfio-single", map[string]any{"Namespace": ns})
		expectPodRunning(ctx, ns, "pod1")
		expectAllocatedClaims(ctx, ns, 1)
	})

	It("[vfio] passthrough is injected into the container", func(ctx SpecContext) {
		applyYAML(ctx, "vfio-single", map[string]any{"Namespace": ns})
		expectPodRunning(ctx, ns, "pod1")
		expectAllocatedClaims(ctx, ns, 1)
		expectPassthroughInPod(ctx, ns, "pod1", "ctr")
	})

	It("[vfio] gpu view remains published during passthrough and after release", func(ctx SpecContext) {
		before := countGPUDevices(ctx)

		applyYAML(ctx, "vfio-single", map[string]any{"Namespace": ns})
		expectPodRunning(ctx, ns, "pod1")
		expectAllocatedClaims(ctx, ns, 1)

		// Shared counters reserve the physical GPU while both views remain
		// published, even when the device is bound to the VFIO driver.
		expectGPUDeviceCount(ctx, before)

		// Published inventory also remains unchanged when the claim is released.
		deletePod(ctx, ns, "pod1")
		expectGPUDeviceCount(ctx, before)
	})

	It("[vfio] N-GPU passthrough claims are allocated to distinct devices", func(ctx SpecContext) {
		const replicas = 2
		requireVfioDevices(ctx, replicas)

		applyYAML(ctx, "vfio-multi", map[string]any{"Namespace": ns, "Replicas": replicas, "NodeName": gpu.NodeName})
		got, err := framework.WaitForPodsReady(ctx, cs, ns, "app=vfio-test", replicas, 5*time.Minute)
		Expect(err).NotTo(HaveOccurred(), "only %d/%d vfio pods Ready", got, replicas)

		expectAllocatedClaims(ctx, ns, replicas)
		expectDistinctDevices(ctx, ns)
	})

	It("[vfio/config] opaque VfioDeviceConfig claim is allocated", func(ctx SpecContext) {
		applyYAML(ctx, "vfio-config", map[string]any{
			"Namespace":       ns,
			"BackendPolicy":   "LegacyOnly",
			"EnableAPIDevice": true,
		})
		expectPodRunning(ctx, ns, "pod1")
		expectAllocatedClaims(ctx, ns, 1)
	})

	It("[vfio/mixed] gpu and vfio workloads coexist", func(ctx SpecContext) {
		// The gpu and vfio devices for one physical GPU are the same silicon
		// and mutually exclusive, so the two workloads need two GPUs to land on.
		requireGPUs(ctx, 2)

		// The VFIO claim consumes the physical GPU's shared counter capacity,
		// so the GPU workload must be allocated to another physical GPU.
		applyYAML(ctx, "vfio-single", map[string]any{"Namespace": ns})
		expectPodRunning(ctx, ns, "pod1")
		expectAllocatedClaims(ctx, ns, 1)

		applyYAML(ctx, "gpu-workload", map[string]any{"Namespace": ns, "NodeName": podNode(ctx, ns, "pod1")})
		expectPodRunning(ctx, ns, "pod-gpu")
		expectAllocatedClaims(ctx, ns, 2)
	})

	It("[vfio] 1-GPU passthrough claim survives a plugin restart", func(ctx SpecContext) {
		before := countGPUDevices(ctx)

		applyYAML(ctx, "vfio-single", map[string]any{"Namespace": ns})
		expectPodRunning(ctx, ns, "pod1")
		expectAllocatedClaims(ctx, ns, 1)
		expectGPUDeviceCount(ctx, before)

		Expect(framework.RestartKubeletPlugin(ctx, cs, podNode(ctx, ns, "pod1"), 5*time.Minute)).To(Succeed())

		// The prepared claim must still be allocated and the pod still Running
		// after the plugin recovers its checkpoint.
		expectAllocatedClaims(ctx, ns, 1)
		expectPodRunning(ctx, ns, "pod1")

		// Recovery restores the sibling inventory from the VFIO snapshot;
		// the allocated claim continues to reserve its shared counters.
		expectGPUDeviceCount(ctx, before)

		// Releasing the recovered claim must preserve the published inventory.
		deletePod(ctx, ns, "pod1")
		expectGPUDeviceCount(ctx, before)
	})

	It("[vfio] N-GPU passthrough claims survive a plugin restart", func(ctx SpecContext) {
		const replicas = 2
		requireVfioDevices(ctx, replicas)

		applyYAML(ctx, "vfio-multi", map[string]any{"Namespace": ns, "Replicas": replicas, "NodeName": gpu.NodeName})
		got, err := framework.WaitForPodsReady(ctx, cs, ns, "app=vfio-test", replicas, 5*time.Minute)
		Expect(err).NotTo(HaveOccurred(), "only %d/%d vfio pods Ready", got, replicas)
		expectAllocatedClaims(ctx, ns, replicas)

		Expect(framework.RestartKubeletPlugin(ctx, cs, gpu.NodeName, 5*time.Minute)).To(Succeed())

		got, err = framework.WaitForPodsReady(ctx, cs, ns, "app=vfio-test", replicas, 5*time.Minute)
		Expect(err).NotTo(HaveOccurred(), "only %d/%d vfio pods Ready after restart", got, replicas)
		expectAllocatedClaims(ctx, ns, replicas)
		expectDistinctDevices(ctx, ns)
	})

	Context("partitionable devices", Label("partitionable"), func() {
		It("Test1 publishes GPU and VFIO siblings consuming shared counters", func(ctx SpecContext) {
			pair := requireVfioDevicePair(ctx, "gpu")
			expectVfioSharedCounters(pair)
		})

		DescribeTable("rejects conflicting workloads on the same physical GPU",
			func(ctx SpecContext, siblingType string, vfioFirst bool) {
				pair := requireVfioDevicePair(ctx, siblingType)
				expectVfioSharedCounters(pair)
				By(fmt.Sprintf("targeting node %s, pool %s, PCI device %s", pair.nodeName, pair.poolName, pair.pciBusID))

				first, second := pair.sibling, pair.vfio
				if vfioFirst {
					first, second = second, first
				}
				applyYAMLPinnedWorkload(ctx, ns, "first", pair, first)
				expectPodRunning(ctx, ns, "first")
				claim := expectVfioPinnedAllocation(ctx, ns, "first", pair, first)
				expectVfioPairPublished(ctx, pair)
				if vfioFirst {
					expectVfioAllocatableSnapshot(ctx, claim, pair)
				}

				applyYAMLPinnedWorkload(ctx, ns, "second", pair, second)
				expectVfioConflictPending(ctx, ns)
				expectVfioPairPublished(ctx, pair)
			},
			Entry("Test2a keeps VFIO Pending while a GPU workload runs", "gpu", false),
			Entry("Test2b keeps GPU Pending while a VFIO workload runs", "gpu", true),
			Entry("Test3a keeps VFIO Pending while a dynamic MIG workload runs", "mig", false),
			Entry("Test3b keeps dynamic MIG Pending while a VFIO workload runs", "mig", true),
		)
	})
})

type vfioDevicePair struct {
	nodeName, poolName, pciBusID string
	vfio, sibling                resourceapi.Device
	counterSets                  map[string]resourceapi.CounterSet
}

// Read complete pools on the test node at their latest generation: counters and devices can
// live in separate slices, and publication can temporarily mix generations.
func readVfioResourcePools(ctx context.Context) ([][]resourceapi.ResourceSlice, error) {
	list, err := cs.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list GPU ResourceSlices: %w", err)
	}
	groups := make(map[string][]resourceapi.ResourceSlice)
	for _, slice := range list.Items {
		if slice.Spec.Driver != "gpu.nvidia.com" || slice.Spec.NodeName == nil || *slice.Spec.NodeName != gpu.NodeName {
			continue
		}
		key := *slice.Spec.NodeName + "/" + slice.Spec.Pool.Name
		group := groups[key]
		if len(group) == 0 || slice.Spec.Pool.Generation > group[0].Spec.Pool.Generation {
			groups[key] = []resourceapi.ResourceSlice{slice}
		} else if slice.Spec.Pool.Generation == group[0].Spec.Pool.Generation {
			groups[key] = append(group, slice)
		}
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var pools [][]resourceapi.ResourceSlice
	for _, key := range keys {
		group := groups[key]
		if int64(len(group)) != group[0].Spec.Pool.ResourceSliceCount {
			return nil, fmt.Errorf("pool %s generation %d is incomplete: got %d/%d slices",
				key, group[0].Spec.Pool.Generation, len(group), group[0].Spec.Pool.ResourceSliceCount)
		}
		pools = append(pools, group)
	}
	return pools, nil
}

func waitForVfioResourcePools(ctx context.Context) [][]resourceapi.ResourceSlice {
	var pools [][]resourceapi.ResourceSlice
	Eventually(func() error {
		var err error
		pools, err = readVfioResourcePools(ctx)
		return err
	}, 2*time.Minute, framework.DefaultPollInterval).WithContext(ctx).Should(Succeed())
	return pools
}

func requireVfioDevicePair(ctx context.Context, siblingType string) vfioDevicePair {
	for _, pool := range waitForVfioResourcePools(ctx) {
		var devices []resourceapi.Device
		counterSets := make(map[string]resourceapi.CounterSet)
		for _, slice := range pool {
			devices = append(devices, slice.Spec.Devices...)
			for _, set := range slice.Spec.SharedCounters {
				counterSets[set.Name] = set
			}
		}
		slices.SortFunc(devices, func(a, b resourceapi.Device) int { return strings.Compare(a.Name, b.Name) })
		for _, vfio := range devices {
			if framework.DRADeviceAttributeString(vfio, "type") != "vfio" {
				continue
			}
			pciBusID := framework.DRADeviceAttributeString(vfio, "resource.kubernetes.io/pciBusID")
			Expect(pciBusID).NotTo(BeEmpty(), "VFIO device %s has no PCI bus ID", vfio.Name)
			var sibling resourceapi.Device
			for _, device := range devices {
				if framework.DRADeviceAttributeString(device, "type") != siblingType || framework.DRADeviceAttributeString(device, "resource.kubernetes.io/pciBusID") != pciBusID {
					continue
				}
				// Static MIG devices have a concrete UUID. These scenarios need
				// abstract dynamic MIG devices and their shared counter contract.
				if siblingType == "mig" && framework.DRADeviceAttributeString(device, "uuid") != "" {
					continue
				}
				memory, hasMemory := device.Capacity["memory"]
				Expect(hasMemory).To(BeTrue(), "device %s has no memory capacity", device.Name)
				previous := sibling.Capacity["memory"]
				if sibling.Name == "" || memory.Value.Cmp(previous.Value) < 0 {
					sibling = device
				}
			}
			if sibling.Name != "" {
				return vfioDevicePair{
					nodeName:    *pool[0].Spec.NodeName,
					poolName:    pool[0].Spec.Pool.Name,
					pciBusID:    pciBusID,
					vfio:        vfio,
					sibling:     sibling,
					counterSets: counterSets,
				}
			}
		}
	}
	Skip(fmt.Sprintf("need published %s and VFIO siblings on one GPU (MIG cases require DynamicMIG)", siblingType))
	return vfioDevicePair{}
}

func expectVfioSharedCounters(pair vfioDevicePair) {
	Expect(pair.vfio.ConsumesCounters).To(HaveLen(1), "VFIO device must consume a shared counter set")
	setName := pair.vfio.ConsumesCounters[0].CounterSet
	set, ok := pair.counterSets[setName]
	Expect(ok).To(BeTrue(), "counter set %s missing from pool %s", setName, pair.poolName)
	share, ok := set.Counters["gpu-share"]
	Expect(ok).To(BeTrue(), "counter set %s has no gpu-share capacity", setName)
	Expect(share.Value.Value()).To(Equal(int64(100)))
	for _, device := range []resourceapi.Device{pair.vfio, pair.sibling} {
		Expect(device.ConsumesCounters).To(HaveLen(1), "device %s must consume shared counters", device.Name)
		consumption := device.ConsumesCounters[0]
		Expect(consumption.CounterSet).To(Equal(setName), "siblings must consume the same counter set")
		used, ok := consumption.Counters["gpu-share"]
		Expect(ok).To(BeTrue(), "device %s has no gpu-share consumption", device.Name)
		Expect(used.Value.Sign()).To(Equal(1), "device %s must consume positive GPU capacity", device.Name)
		Expect(used.Value.Cmp(share.Value)).To(BeNumerically("<=", 0))
		if framework.DRADeviceAttributeString(device, "type") != "mig" {
			Expect(used.Value.Cmp(share.Value)).To(BeZero(), "GPU and VFIO must consume the full GPU share")
		}
		for name, used := range consumption.Counters {
			capacity, ok := set.Counters[name]
			Expect(ok).To(BeTrue(), "device %s references missing counter %s", device.Name, name)
			Expect(used.Value.Cmp(capacity.Value)).To(BeNumerically("<=", 0))
		}
	}
}

func applyYAMLPinnedWorkload(ctx context.Context, ns, name string, pair vfioDevicePair, device resourceapi.Device) {
	deviceType := framework.DRADeviceAttributeString(device, "type")
	classes := map[string]string{"gpu": "gpu.nvidia.com", "mig": "mig.nvidia.com", "vfio": "vfio.gpu.nvidia.com"}
	applyYAML(ctx, "vfio-exclusion", map[string]any{
		"Namespace":       ns,
		"Name":            name,
		"NodeName":        pair.nodeName,
		"PCIBusID":        pair.pciBusID,
		"DeviceClassName": classes[deviceType],
		"Profile":         framework.DRADeviceAttributeString(device, "profile"),
		"Vfio":            deviceType == "vfio",
	})
}

func expectVfioPinnedAllocation(ctx context.Context, ns, name string, pair vfioDevicePair, device resourceapi.Device) *resourceapi.ResourceClaim {
	Expect(podNode(ctx, ns, name)).To(Equal(pair.nodeName))
	claim, err := cs.ResourceV1().ResourceClaims(ns).Get(ctx, name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(claim.Status.Allocation).NotTo(BeNil())
	Expect(claim.Status.Allocation.Devices.Results).To(HaveLen(1))
	result := claim.Status.Allocation.Devices.Results[0]
	Expect(result.Driver).To(Equal("gpu.nvidia.com"))
	Expect(result.Pool).To(Equal(pair.poolName))
	for _, pool := range waitForVfioResourcePools(ctx) {
		if *pool[0].Spec.NodeName != pair.nodeName || pool[0].Spec.Pool.Name != pair.poolName {
			continue
		}
		for _, slice := range pool {
			for _, allocated := range slice.Spec.Devices {
				if allocated.Name == result.Device {
					Expect(framework.DRADeviceAttributeString(allocated, "resource.kubernetes.io/pciBusID")).To(Equal(pair.pciBusID))
					Expect(framework.DRADeviceAttributeString(allocated, "type")).To(Equal(framework.DRADeviceAttributeString(device, "type")))
					Expect(framework.DRADeviceAttributeString(allocated, "profile")).To(Equal(framework.DRADeviceAttributeString(device, "profile")))
					return claim
				}
			}
		}
	}
	Fail(fmt.Sprintf("allocated device %s is missing from pool %s", result.Device, pair.poolName))
	return nil
}

func expectVfioPairPublished(ctx context.Context, pair vfioDevicePair) {
	Eventually(func(g Gomega) {
		pools, err := readVfioResourcePools(ctx)
		g.Expect(err).NotTo(HaveOccurred())
		var names []string
		for _, pool := range pools {
			if *pool[0].Spec.NodeName != pair.nodeName || pool[0].Spec.Pool.Name != pair.poolName {
				continue
			}
			for _, slice := range pool {
				for _, device := range slice.Spec.Devices {
					if framework.DRADeviceAttributeString(device, "resource.kubernetes.io/pciBusID") == pair.pciBusID {
						names = append(names, device.Name)
					}
				}
			}
		}
		g.Expect(names).To(ContainElements(pair.vfio.Name, pair.sibling.Name), "both sibling views must remain published")
	}, 2*time.Minute, framework.DefaultPollInterval).WithContext(ctx).Should(Succeed())
}

func expectVfioConflictPending(ctx context.Context, ns string) {
	check := func(g Gomega) {
		pod, err := cs.CoreV1().Pods(ns).Get(ctx, "second", metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pod.Status.Phase).To(Equal(corev1.PodPending))
		g.Expect(pod.Spec.NodeName).To(BeEmpty())
		var scheduled corev1.PodCondition
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled {
				scheduled = condition
			}
		}
		g.Expect(scheduled.Status).To(Equal(corev1.ConditionFalse))
		g.Expect(scheduled.Reason).To(Equal("Unschedulable"))
		g.Expect(strings.ToLower(scheduled.Message)).To(ContainSubstring("allocate"), "expected DRA scheduling rejection")
		claim, err := cs.ResourceV1().ResourceClaims(ns).Get(ctx, "second", metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(claim.Status.Allocation).To(BeNil(), "conflicting claim must remain unallocated")
		first, err := cs.CoreV1().Pods(ns).Get(ctx, "first", metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(first.Status.Phase).To(Equal(corev1.PodRunning))
		firstClaim, err := cs.ResourceV1().ResourceClaims(ns).Get(ctx, "first", metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(firstClaim.Status.Allocation).NotTo(BeNil())
	}
	Eventually(check, 2*time.Minute, framework.DefaultPollInterval).WithContext(ctx).Should(Succeed())
	Consistently(check, 30*time.Second, framework.DefaultPollInterval).WithContext(ctx).Should(Succeed())
}

func expectVfioAllocatableSnapshot(ctx context.Context, claim *resourceapi.ResourceClaim, pair vfioDevicePair) {
	pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		LabelSelector: framework.KubeletPluginSelector,
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", pair.nodeName).String(),
	})
	Expect(err).NotTo(HaveOccurred())
	var plugin *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == "gpus" && status.Ready {
				Expect(plugin).To(BeNil(), "multiple ready GPU plugin pods on node %s", pair.nodeName)
				plugin = pod
			}
		}
	}
	Expect(plugin).NotTo(BeNil(), "no ready GPU plugin pod on node %s", pair.nodeName)

	// CLI flags override the environment. Resolve environment-provided paths
	// inside the container so envFrom and valueFrom work as well as literal env.
	var directory string
	for _, container := range plugin.Spec.Containers {
		if container.Name != "gpus" {
			continue
		}
		args := append(slices.Clone(container.Command), container.Args...)
		for i, arg := range args {
			if value, ok := strings.CutPrefix(arg, "--kubelet-plugins-directory-path="); ok {
				directory = value
			} else if arg == "--kubelet-plugins-directory-path" {
				Expect(i+1).To(BeNumerically("<", len(args)), "plugin directory flag has no value")
				directory = args[i+1]
			}
		}
	}
	data, stderr, err := framework.ExecInPod(ctx, restConfig, cs, plugin.Namespace, plugin.Name, "gpus", []string{
		"sh", "-c", `cat "${1:-${KUBELET_PLUGINS_DIRECTORY_PATH:-/var/lib/kubelet/plugins}}/gpu.nvidia.com/checkpoint.json"`, "sh", directory,
	})
	Expect(err).NotTo(HaveOccurred(), "read checkpoint in %s/%s: stderr=%q", plugin.Namespace, plugin.Name, stderr)

	// Decode only the fields this test owns; importing the plugin's main
	// package or duplicating its complete checkpoint schema would couple e2e
	// coverage to unrelated persistence details.
	var checkpoint struct {
		V2 struct {
			PreparedClaims map[string]struct {
				CheckpointState string `json:"checkpointState"`
				PreparedDevices []struct {
					Devices []struct {
						Vfio *struct {
							Device struct {
								DeviceName string `json:"DeviceName"`
							} `json:"device"`
							AllocatableDevicesSnapshot string `json:"allocatableDevicesSnapshot"`
						} `json:"vfio"`
					} `json:"devices"`
				} `json:"preparedDevices"`
			} `json:"preparedClaims"`
		} `json:"v2"`
	}
	Expect(json.Unmarshal([]byte(data), &checkpoint)).To(Succeed(), "parse GPU plugin checkpoint")
	prepared, ok := checkpoint.V2.PreparedClaims[string(claim.UID)]
	Expect(ok).To(BeTrue(), "claim %s/%s (%s) missing from checkpoint", claim.Namespace, claim.Name, claim.UID)
	Expect(prepared.CheckpointState).To(Equal("PrepareCompleted"))
	var encoded string
	for _, group := range prepared.PreparedDevices {
		for _, device := range group.Devices {
			if device.Vfio != nil && device.Vfio.Device.DeviceName == pair.vfio.Name {
				encoded = device.Vfio.AllocatableDevicesSnapshot
			}
		}
	}
	Expect(encoded).NotTo(BeEmpty(), "VFIO device %s has no allocatableDevicesSnapshot", pair.vfio.Name)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	Expect(err).NotTo(HaveOccurred(), "snapshot must be standard base64")
	type identity struct {
		UUID     string `json:"uuid"`
		PCIBusID string `json:"pciBusID"`
	}
	var snapshot map[string]struct {
		GPU        *identity `json:"gpu"`
		Vfio       *identity `json:"vfio"`
		MigDynamic *struct {
			Parent *identity `json:"parent"`
		} `json:"migDynamic"`
	}
	Expect(json.Unmarshal(decoded, &snapshot)).To(Succeed(), "snapshot must contain an allocatable-device JSON map")
	Expect(snapshot).To(HaveKey(pair.vfio.Name))
	vfio := snapshot[pair.vfio.Name].Vfio
	Expect(vfio).NotTo(BeNil())
	Expect(vfio.PCIBusID).To(Equal(pair.pciBusID))
	Expect(vfio.UUID).To(Equal(framework.DRADeviceAttributeString(pair.vfio, "uuid")))
	Expect(snapshot).To(HaveKey(pair.sibling.Name))
	sibling := snapshot[pair.sibling.Name]
	if framework.DRADeviceAttributeString(pair.sibling, "type") == "gpu" {
		Expect(sibling.GPU).NotTo(BeNil())
		Expect(sibling.GPU.PCIBusID).To(Equal(pair.pciBusID))
		Expect(sibling.GPU.UUID).To(Equal(framework.DRADeviceAttributeString(pair.sibling, "uuid")))
	} else {
		Expect(sibling.MigDynamic).NotTo(BeNil())
		Expect(sibling.MigDynamic.Parent).NotTo(BeNil())
		Expect(sibling.MigDynamic.Parent.PCIBusID).To(Equal(pair.pciBusID))
		Expect(sibling.MigDynamic.Parent.UUID).To(Equal(framework.DRADeviceAttributeString(pair.sibling, "parentUUID")))
	}
}

// countVfioDevices returns the number of published vfio devices on the test node.
func countVfioDevices(ctx context.Context) int {
	slices, err := cs.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())

	count := 0
	for _, s := range slices.Items {
		if s.Spec.Driver != "gpu.nvidia.com" || s.Spec.NodeName == nil || *s.Spec.NodeName != gpu.NodeName {
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

// requireVfio skips the spec unless the test node publishes at least one
// vfio device (PassthroughSupport enabled + IOMMU-capable hardware).
func requireVfio(ctx context.Context) {
	if gpu.NodeName == "" {
		Skip("the detected GPU has no node name")
	}
	if countVfioDevices(ctx) == 0 {
		Skip(fmt.Sprintf("PassthroughSupport (vfio) is not enabled or no vfio device is published on node %s", gpu.NodeName))
	}
}

// requireVfioDevices skips the spec unless the test node publishes at least n vfio devices.
func requireVfioDevices(ctx context.Context, n int) {
	if got := countVfioDevices(ctx); got < n {
		Skip(fmt.Sprintf("need %d vfio devices on node %s, node publishes %d", n, gpu.NodeName, got))
	}
}

// countGPUDevices returns the number of published gpu-type devices on the test node.
func countGPUDevices(ctx context.Context) int {
	slices, err := cs.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())

	count := 0
	for _, s := range slices.Items {
		if s.Spec.Driver != "gpu.nvidia.com" || s.Spec.NodeName == nil || *s.Spec.NodeName != gpu.NodeName {
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

// requireGPUs skips the spec unless the test node publishes at least n gpu-type devices.
func requireGPUs(ctx context.Context, n int) {
	if got := countGPUDevices(ctx); got < n {
		Skip(fmt.Sprintf("need %d gpu devices on node %s, node publishes %d", n, gpu.NodeName, got))
	}
}

// expectGPUDeviceCount waits for the published gpu-type device count to settle
// at want. The driver republishes its ResourceSlice asynchronously after a
// claim is prepared or released, so the assertion must be eventual.
func expectGPUDeviceCount(ctx context.Context, want int) {
	Eventually(func() int { return countGPUDevices(ctx) }, 2*time.Minute, 5*time.Second).
		Should(Equal(want), "expected %d gpu-type devices published", want)
}

// vfioGroupNode matches a VFIO group node name in the output of `ls /dev/vfio`:
// a line that is only digits (the IOMMU group number), as opposed to the
// `vfio` control device. A match proves the GPU was handed through as a VFIO
// device.
var vfioGroupNode = regexp.MustCompile(`(?m)^[0-9]+$`)

// expectPassthroughInPod execs into the container and proves the GPU was handed
// through as a VFIO device with three independent checks:
//  1. NVIDIA_VISIBLE_DEVICES=void, so the nvidia-container-runtime is
//     neutralized and does not inject the GPU as a normal device.
//  2. the /dev/vfio directory exists (the VFIO subsystem is present).
//  3. a numeric /dev/vfio/<group> node exists (the GPU's IOMMU group was
//     handed through, as opposed to just the /dev/vfio/vfio control device).
func expectPassthroughInPod(ctx context.Context, ns, pod, container string) {
	env, envErr, err := framework.ExecInPod(ctx, restConfig, cs, ns, pod, container,
		[]string{"sh", "-c", `printenv NVIDIA_VISIBLE_DEVICES`})
	Expect(err).NotTo(HaveOccurred(), "exec printenv in %s/%s: stdout=%q stderr=%q", ns, pod, env, envErr)
	Expect(strings.TrimSpace(env)).To(Equal("void"),
		"NVIDIA_VISIBLE_DEVICES not void: stdout=%q stderr=%q", env, envErr)

	dir, dirErr, err := framework.ExecInPod(ctx, restConfig, cs, ns, pod, container,
		[]string{"sh", "-c", `test -d /dev/vfio && echo present`})
	Expect(err).NotTo(HaveOccurred(), "exec test -d /dev/vfio in %s/%s: stdout=%q stderr=%q", ns, pod, dir, dirErr)
	Expect(strings.TrimSpace(dir)).To(Equal("present"), "/dev/vfio directory missing: stderr=%q", dirErr)

	groups, groupsErr, err := framework.ExecInPod(ctx, restConfig, cs, ns, pod, container,
		[]string{"sh", "-c", `ls /dev/vfio`})
	Expect(err).NotTo(HaveOccurred(), "exec ls /dev/vfio in %s/%s: stdout=%q stderr=%q", ns, pod, groups, groupsErr)
	Expect(vfioGroupNode.MatchString(groups)).To(BeTrue(),
		"no /dev/vfio/<group> node present: stdout=%q stderr=%q", groups, groupsErr)
}

func expectPodRunning(ctx context.Context, ns, name string) {
	phase, err := framework.WaitForPodPhase(ctx, cs, ns, name,
		[]corev1.PodPhase{corev1.PodRunning}, 3*time.Minute)
	Expect(err).NotTo(HaveOccurred(), "pod %s never reached Running (phase=%s)", name, phase)
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
		for _, res := range rc.Status.Allocation.Devices.Results {
			dev := res.Device
			Expect(dev).NotTo(BeEmpty(), "claim %s allocated device name is empty", rc.Name)
			if prev, ok := seen[dev]; ok {
				Fail(fmt.Sprintf("device %s allocated to both %s and %s; passthrough must be exclusive", dev, prev, rc.Name))
			}
			seen[dev] = rc.Name
		}
	}
}
