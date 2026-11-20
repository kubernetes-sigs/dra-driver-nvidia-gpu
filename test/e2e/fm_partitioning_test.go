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
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"

	"sigs.k8s.io/dra-driver-nvidia-gpu/test/e2e/framework"
)

const (
	fmGPUClass  = "gpu.nvidia.com"
	fmVfioClass = "vfio.gpu.nvidia.com"
	fmGPUImage  = "nvcr.io/nvidia/cuda:12.2.0-base-ubuntu22.04"
	fmVfioImage = "ubuntu:22.04"

	// fmPrepareMismatch is the Prepare error the plugin returns when a claim's
	// GPUs do not form exactly one Fabric Manager partition.
	fmPrepareMismatch = "does not match any FM partition"
)

// End-to-end coverage for the FabricManagerPartitioning feature. Every spec
// skips unless a node publishes Fabric Manager partition attributes
// (gpuModuleID / partitionN), so the suite stays green on clusters without an
// NVSwitch-managed fabric. Specs read the partition layout from the
// ResourceSlices instead of assuming a specific HGX topology, and assume the
// GPUs on the Fabric Manager node are otherwise idle.
//
// Fabric Manager state is not visible through the Kubernetes API, so
// activation and deactivation are asserted through their effects: Prepare
// succeeds only for a claim that forms exactly one partition, and a partition
// sharing a GPU with a still-active one cannot be activated.
var _ = Describe("Fabric Manager Partitioning", func() {
	var (
		ns   string
		topo fmTopology
	)

	BeforeEach(func(ctx SpecContext) {
		ns = ""
		topo = fabricTopology(ctx, "gpu")
		if len(topo) == 0 {
			Skip("FabricManagerPartitioning is not enabled or no node publishes Fabric Manager partition attributes")
		}
		ns = fmt.Sprintf("fm-e2e-%d", time.Now().UnixNano()%1_000_000)
	})

	AfterEach(func(ctx SpecContext) {
		if ns == "" {
			return
		}
		_ = cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
		// Specs pin specific partitions, so the next spec can only be prepared
		// once this one's claims are released and their partitions deactivated.
		Eventually(func() bool {
			_, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}, 5*time.Minute, 5*time.Second).Should(BeTrue(), "namespace %s never deleted", ns)
	})

	It("[fm] ResourceSlices publish a consistent partition topology", func(ctx SpecContext) {
		for _, node := range topo.nodes() {
			moduleOwner := map[int64]string{}
			for _, d := range topo[node] {
				Expect(d.ModuleID).To(BeNumerically(">", 0),
					"node %s: device %s publishes partitions without a gpuModuleID", node, d.Name)
				if prev, ok := moduleOwner[d.ModuleID]; ok {
					Fail(fmt.Sprintf("node %s: gpuModuleID %d published by both %s and %s", node, d.ModuleID, prev, d.Name))
				}
				moduleOwner[d.ModuleID] = d.Name
			}
			for _, size := range topo.sizes() {
				for id, members := range topo.partitions(node, size) {
					Expect(members).To(HaveLen(size),
						"node %s: partition%d=%d is published by %d devices, want %d", node, size, id, len(members), size)
				}
			}
		}

		// With PassthroughSupport on, a GPU's vfio device must carry the same
		// Fabric Manager view as its gpu device.
		vfio := fabricTopology(ctx, "vfio")
		for _, node := range vfio.nodes() {
			byPCI := map[string]fmDevice{}
			for _, d := range topo[node] {
				byPCI[d.PCIBusID] = d
			}
			for _, v := range vfio[node] {
				g, ok := byPCI[v.PCIBusID]
				if !ok {
					continue
				}
				Expect(v.ModuleID).To(Equal(g.ModuleID), "node %s: %s and %s disagree on gpuModuleID", node, v.Name, g.Name)
				Expect(v.Partitions).To(Equal(g.Partitions), "node %s: %s and %s disagree on partitions", node, v.Name, g.Name)
			}
		}
	})

	DescribeTable("[fm/gpu] N-GPU claim constrained to one partition is prepared",
		func(ctx SpecContext, size int) {
			if len(topo.completePartitions(size)) == 0 {
				Skip(fmt.Sprintf("no node publishes a complete %d-GPU Fabric Manager partition", size))
			}

			applyFMPartition(ctx, ns, "pod1", fmGPUClass, fmGPUImage, anyPartition(size))
			expectPodRunning(ctx, ns, "pod1")
			expectPartitionAllocation(ctx, topo, ns, "pod1", size)
			expectVisibleGPUs(ctx, ns, "pod1", size)
		},
		Entry("1 GPU", 1),
		Entry("2 GPUs", 2),
		Entry("4 GPUs", 4),
		Entry("8 GPUs", 8),
	)

	It("[fm/gpu] disjoint partitions of the same size are active concurrently", func(ctx SpecContext) {
		a, b, ok := topo.disjointPair()
		if !ok {
			Skip("no node publishes two complete Fabric Manager partitions of the same size")
		}

		applyFMPartition(ctx, ns, "pod-a", fmGPUClass, fmGPUImage, a)
		applyFMPartition(ctx, ns, "pod-b", fmGPUClass, fmGPUImage, b)
		expectPodRunning(ctx, ns, "pod-a")
		expectPodRunning(ctx, ns, "pod-b")
		Expect(expectPartitionAllocation(ctx, topo, ns, "pod-a", a.Size).ID).To(Equal(a.ID))
		Expect(expectPartitionAllocation(ctx, topo, ns, "pod-b", b.Size).ID).To(Equal(b.ID))
	})

	It("[fm/gpu] releasing a partition lets an overlapping partition be activated", func(ctx SpecContext) {
		small, large, ok := topo.nestedPair()
		if !ok {
			Skip("no node publishes two overlapping complete Fabric Manager partitions")
		}

		By(fmt.Sprintf("preparing partition%d=%d on %s", small.Size, small.ID, small.Node))
		applyFMPartition(ctx, ns, "small", fmGPUClass, fmGPUImage, small)
		expectPodRunning(ctx, ns, "small")
		Expect(expectPartitionAllocation(ctx, topo, ns, "small", small.Size).ID).To(Equal(small.ID))
		deletePod(ctx, ns, "small")

		// Fabric Manager refuses to activate a partition that shares a GPU with
		// an active one, so this only prepares if releasing "small" deactivated it.
		By(fmt.Sprintf("preparing overlapping partition%d=%d", large.Size, large.ID))
		applyFMPartition(ctx, ns, "large", fmGPUClass, fmGPUImage, large)
		expectPodRunning(ctx, ns, "large")
		Expect(expectPartitionAllocation(ctx, topo, ns, "large", large.Size).ID).To(Equal(large.ID))
		deletePod(ctx, ns, "large")

		By("preparing the smaller partition again")
		applyFMPartition(ctx, ns, "small-again", fmGPUClass, fmGPUImage, small)
		expectPodRunning(ctx, ns, "small-again")
	})

	It("[fm/negative] claim whose GPUs cross partition boundaries fails Prepare", func(ctx SpecContext) {
		pair, ok := topo.crossPartitionPair()
		if !ok {
			Skip("no node has two GPUs outside a common 2-GPU Fabric Manager partition")
		}

		applyFMPinned(ctx, ns, "pod1", pair, false)

		// The scheduler allocates the claim; only Prepare can reject it.
		expectPodEvent(ctx, ns, "pod1", fmPrepareMismatch)
		pod, err := cs.CoreV1().Pods(ns).Get(ctx, "pod1", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(pod.Status.Phase).To(Equal(corev1.PodPending),
			"pod with a non-partition GPU set must not start; got phase=%s", pod.Status.Phase)
		expectAllocatedClaims(ctx, ns, 1)

		// A rejected Prepare must not leave the GPUs unusable: once released,
		// each can still be prepared as part of a valid partition.
		deletePod(ctx, ns, "pod1")
		for i, d := range pair.Devices {
			p, ok := topo.partitionContaining(pair.Node, d.ModuleID)
			if !ok {
				continue
			}
			name := fmt.Sprintf("recover-%d", i)
			By(fmt.Sprintf("preparing partition%d=%d containing %s", p.Size, p.ID, d.Name))
			applyFMPartition(ctx, ns, name, fmGPUClass, fmGPUImage, p)
			expectPodRunning(ctx, ns, name)
			deletePod(ctx, ns, name)
		}
	})

	It("[fm/admin] admin-access claim across partitions skips partition activation", func(ctx SpecContext) {
		pair, ok := topo.crossPartitionPair()
		if !ok {
			Skip("no node has two GPUs outside a common 2-GPU Fabric Manager partition")
		}

		applyFMPinned(ctx, ns, "pod1", pair, true)
		requireAdminAccessPreserved(ctx, ns, "pod1")

		// Admin access is monitoring intent, not a workload: Prepare must not try
		// to activate a partition for it, so the non-partition set is accepted.
		expectPodRunning(ctx, ns, "pod1")
		expectAllocatedClaims(ctx, ns, 1)
	})

	It("[fm/gpu] prepared partition survives a plugin restart and is deactivated afterwards", func(ctx SpecContext) {
		small, large, ok := topo.nestedPair()
		if !ok {
			Skip("no node publishes two overlapping complete Fabric Manager partitions")
		}

		applyFMPartition(ctx, ns, "small", fmGPUClass, fmGPUImage, small)
		expectPodRunning(ctx, ns, "small")
		Expect(expectPartitionAllocation(ctx, topo, ns, "small", small.Size).ID).To(Equal(small.ID))

		node := podNode(ctx, ns, "small")
		Expect(framework.RestartKubeletPlugin(ctx, cs, node, 5*time.Minute)).To(Succeed())

		expectPodRunning(ctx, ns, "small")
		expectAllocatedClaims(ctx, ns, 1)
		// The restarted plugin rediscovers gpuModuleID and partitions from NVML
		// and Fabric Manager; the published topology must not change.
		Eventually(func() []fmDevice { return fabricTopology(ctx, "gpu")[node] }, 2*time.Minute, 5*time.Second).
			Should(Equal(topo[node]), "Fabric Manager attributes on %s changed across a plugin restart", node)

		// Unprepare after the restart must still deactivate the partition, which
		// is only provable by activating an overlapping one.
		deletePod(ctx, ns, "small")
		applyFMPartition(ctx, ns, "large", fmGPUClass, fmGPUImage, large)
		expectPodRunning(ctx, ns, "large")
	})

	It("[fm/vfio] passthrough claim constrained to one partition is prepared and released", func(ctx SpecContext) {
		vfio := fabricTopology(ctx, "vfio")
		p, ok := vfio.preferredPartition()
		if !ok {
			Skip("PassthroughSupport is not enabled or no vfio device publishes Fabric Manager partition attributes")
		}
		before := countGPUDevices(ctx)

		applyFMPartition(ctx, ns, "pod1", fmVfioClass, fmVfioImage, p)
		expectPodRunning(ctx, ns, "pod1")
		Expect(expectPartitionAllocation(ctx, vfio, ns, "pod1", p.Size).ID).To(Equal(p.ID))
		expectPassthroughInPod(ctx, ns, "pod1", "ctr")
		expectGPUDeviceCount(ctx, before-p.Size)

		deletePod(ctx, ns, "pod1")
		expectGPUDeviceCount(ctx, before)
		// Unprepare rebinds the GPUs to nvidia and must repopulate their Fabric
		// Manager attributes, or later full-GPU claims cannot match a partition.
		Eventually(func() []fmDevice { return fabricTopology(ctx, "gpu")[p.Node] }, 2*time.Minute, 5*time.Second).
			Should(Equal(topo[p.Node]), "Fabric Manager attributes on %s not restored after passthrough release", p.Node)

		large, ok := topo.overlappingLarger(p)
		if !ok {
			return
		}
		// Deactivation for VFIO claims runs only after the rebind, so an
		// overlapping full-GPU partition proves it happened.
		By(fmt.Sprintf("preparing overlapping full-GPU partition%d=%d", large.Size, large.ID))
		applyFMPartition(ctx, ns, "large", fmGPUClass, fmGPUImage, large)
		expectPodRunning(ctx, ns, "large")
	})
})

// fmDevice is the Fabric Manager view of one published device.
type fmDevice struct {
	Name       string
	UUID       string
	PCIBusID   string
	ModuleID   int64
	Partitions map[int]int64 // partition size -> partition ID
}

// fmPartition is one Fabric Manager partition as reconstructed from the
// partitionN attributes of the devices on a node. ID < 0 means "any partition
// of this size" when rendering a claim.
type fmPartition struct {
	Node    string
	Size    int
	ID      int64
	Members []fmDevice
}

func anyPartition(size int) fmPartition {
	return fmPartition{Size: size, ID: -1}
}

func (p fmPartition) overlaps(q fmPartition) bool {
	if p.Node != q.Node {
		return false
	}
	for _, m := range p.Members {
		if slices.ContainsFunc(q.Members, func(o fmDevice) bool { return o.ModuleID == m.ModuleID }) {
			return true
		}
	}
	return false
}

// fmPair is two GPUs on one node used to build a claim pinned by UUID.
type fmPair struct {
	Node    string
	Devices [2]fmDevice
}

// fmTopology maps node name to the devices of one type that publish Fabric
// Manager attributes, sorted by device name. Partition and module IDs are
// node-local, so all lookups are scoped to a node.
type fmTopology map[string][]fmDevice

var partitionAttr = regexp.MustCompile(`^(?:gpu\.nvidia\.com/)?partition([0-9]+)$`)

// fabricTopology returns the Fabric Manager view of every gpu.nvidia.com
// device of the given type ("gpu" or "vfio").
func fabricTopology(ctx context.Context, deviceType string) fmTopology {
	list, err := cs.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	return topologyFromSlices(list.Items, deviceType)
}

func topologyFromSlices(resourceSlices []resourceapi.ResourceSlice, deviceType string) fmTopology {
	topo := fmTopology{}
	for _, s := range resourceSlices {
		if s.Spec.Driver != "gpu.nvidia.com" || s.Spec.NodeName == nil {
			continue
		}
		for _, d := range s.Spec.Devices {
			if attr, ok := d.Attributes["type"]; !ok || attr.StringValue == nil || *attr.StringValue != deviceType {
				continue
			}
			dev := fmDevice{Name: d.Name, Partitions: map[int]int64{}}
			for name, attr := range d.Attributes {
				switch {
				case name == "uuid" && attr.StringValue != nil:
					dev.UUID = *attr.StringValue
				case name == "resource.kubernetes.io/pciBusID" && attr.StringValue != nil:
					dev.PCIBusID = *attr.StringValue
				case name == "gpuModuleID" && attr.IntValue != nil:
					dev.ModuleID = *attr.IntValue
				default:
					if m := partitionAttr.FindStringSubmatch(string(name)); m != nil && attr.IntValue != nil {
						size, _ := strconv.Atoi(m[1])
						dev.Partitions[size] = *attr.IntValue
					}
				}
			}
			if dev.ModuleID == 0 && len(dev.Partitions) == 0 {
				continue
			}
			node := *s.Spec.NodeName
			topo[node] = append(topo[node], dev)
		}
	}
	for node := range topo {
		slices.SortFunc(topo[node], func(a, b fmDevice) int { return strings.Compare(a.Name, b.Name) })
	}
	return topo
}

func (t fmTopology) nodes() []string {
	nodes := make([]string, 0, len(t))
	for node := range t {
		nodes = append(nodes, node)
	}
	slices.Sort(nodes)
	return nodes
}

// sizes returns every partition size published on any node, ascending.
func (t fmTopology) sizes() []int {
	var sizes []int
	for _, devs := range t {
		for _, d := range devs {
			for size := range d.Partitions {
				if !slices.Contains(sizes, size) {
					sizes = append(sizes, size)
				}
			}
		}
	}
	slices.Sort(sizes)
	return sizes
}

// partitions returns the size-N partitions on node, keyed by partition ID.
func (t fmTopology) partitions(node string, size int) map[int64][]fmDevice {
	out := map[int64][]fmDevice{}
	for _, d := range t[node] {
		if id, ok := d.Partitions[size]; ok {
			out[id] = append(out[id], d)
		}
	}
	return out
}

// completePartitions returns the size-N partitions whose full membership is
// published, ordered by node and then partition ID. A partition is incomplete
// when some member's device is hidden, e.g. a gpu view while bound to vfio-pci.
func (t fmTopology) completePartitions(size int) []fmPartition {
	var out []fmPartition
	for _, node := range t.nodes() {
		byID := t.partitions(node, size)
		ids := make([]int64, 0, len(byID))
		for id := range byID {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			if len(byID[id]) == size {
				out = append(out, fmPartition{Node: node, Size: size, ID: id, Members: byID[id]})
			}
		}
	}
	return out
}

// multiGPUFirst orders sizes so multi-GPU partitions, which exercise the
// partition constraint, are tried before 1-GPU ones.
func multiGPUFirst(sizes []int) []int {
	out := slices.DeleteFunc(slices.Clone(sizes), func(s int) bool { return s == 1 })
	if slices.Contains(sizes, 1) {
		out = append(out, 1)
	}
	return out
}

// preferredPartition returns the smallest complete multi-GPU partition, or a
// 1-GPU partition if that is all the topology has.
func (t fmTopology) preferredPartition() (fmPartition, bool) {
	for _, size := range multiGPUFirst(t.sizes()) {
		if parts := t.completePartitions(size); len(parts) > 0 {
			return parts[0], true
		}
	}
	return fmPartition{}, false
}

// disjointPair returns two complete partitions of the same size on one node.
// The driver rejects topologies where a GPU is in two same-size partitions, so
// they never share a GPU.
func (t fmTopology) disjointPair() (fmPartition, fmPartition, bool) {
	for _, size := range multiGPUFirst(t.sizes()) {
		parts := t.completePartitions(size)
		for i := 0; i+1 < len(parts); i++ {
			if parts[i].Node == parts[i+1].Node {
				return parts[i], parts[i+1], true
			}
		}
	}
	return fmPartition{}, fmPartition{}, false
}

// overlappingLarger returns a complete partition larger than p, on the same
// node, that shares at least one GPU with it. p may come from a different
// device type's topology; overlap is by gpuModuleID.
func (t fmTopology) overlappingLarger(p fmPartition) (fmPartition, bool) {
	for _, size := range t.sizes() {
		if size <= p.Size {
			continue
		}
		for _, q := range t.completePartitions(size) {
			if q.overlaps(p) {
				return q, true
			}
		}
	}
	return fmPartition{}, false
}

// nestedPair returns two complete partitions of different sizes on one node
// that share at least one GPU.
func (t fmTopology) nestedPair() (fmPartition, fmPartition, bool) {
	for _, size := range multiGPUFirst(t.sizes()) {
		for _, p := range t.completePartitions(size) {
			if q, ok := t.overlappingLarger(p); ok {
				return p, q, true
			}
		}
	}
	return fmPartition{}, fmPartition{}, false
}

// partitionContaining returns the smallest complete partition on node that
// contains the GPU with the given gpuModuleID.
func (t fmTopology) partitionContaining(node string, moduleID int64) (fmPartition, bool) {
	for _, size := range t.sizes() {
		for _, p := range t.completePartitions(size) {
			if p.Node == node && slices.ContainsFunc(p.Members, func(d fmDevice) bool { return d.ModuleID == moduleID }) {
				return p, true
			}
		}
	}
	return fmPartition{}, false
}

// crossPartitionPair returns two GPUs on one node that are not members of a
// common 2-GPU partition. A 2-GPU set can only match a 2-GPU partition, so a
// claim for exactly these two GPUs must fail Prepare.
func (t fmTopology) crossPartitionPair() (fmPair, bool) {
	for _, node := range t.nodes() {
		devs := t[node]
		for i := range devs {
			for j := i + 1; j < len(devs); j++ {
				pi, iok := devs[i].Partitions[2]
				pj, jok := devs[j].Partitions[2]
				if iok && jok && pi == pj {
					continue
				}
				return fmPair{Node: node, Devices: [2]fmDevice{devs[i], devs[j]}}, true
			}
		}
	}
	return fmPair{}, false
}

func applyFMPartition(ctx context.Context, ns, name, deviceClass, image string, p fmPartition) {
	yaml, err := framework.Render("fm-partition", map[string]any{
		"Namespace":   ns,
		"Name":        name,
		"DeviceClass": deviceClass,
		"Image":       image,
		"Size":        p.Size,
		"PartitionID": p.ID,
		"Node":        p.Node,
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(framework.ApplyYAML(ctx, yaml)).To(Succeed())
}

func applyFMPinned(ctx context.Context, ns, name string, pair fmPair, adminAccess bool) {
	yaml, err := framework.Render("fm-pinned-gpus", map[string]any{
		"Namespace":   ns,
		"Name":        name,
		"Image":       fmGPUImage,
		"UUIDs":       []string{pair.Devices[0].UUID, pair.Devices[1].UUID},
		"AdminAccess": adminAccess,
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(framework.ApplyYAML(ctx, yaml)).To(Succeed())
}

// requireAdminAccessPreserved skips the spec when the API server dropped
// adminAccess from the claim template, which it does silently when the
// DRAAdminAccess feature gate is disabled.
func requireAdminAccessPreserved(ctx context.Context, ns, name string) {
	rct, err := cs.ResourceV1().ResourceClaimTemplates(ns).Get(ctx, name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	for _, r := range rct.Spec.Spec.Devices.Requests {
		if r.Exactly == nil || r.Exactly.AdminAccess == nil || !*r.Exactly.AdminAccess {
			Skip("DRAAdminAccess is not enabled on the API server; adminAccess was dropped from the claim")
		}
	}
}

// podClaimDevices returns the gpu.nvidia.com devices allocated to the pod's
// generated "gpus" claim.
func podClaimDevices(ctx context.Context, ns, pod string) []string {
	p, err := cs.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())

	var claimName string
	for _, s := range p.Status.ResourceClaimStatuses {
		if s.Name == "gpus" && s.ResourceClaimName != nil {
			claimName = *s.ResourceClaimName
		}
	}
	Expect(claimName).NotTo(BeEmpty(), "pod %s/%s has no generated claim", ns, pod)

	rc, err := cs.ResourceV1().ResourceClaims(ns).Get(ctx, claimName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(rc.Status.Allocation).NotTo(BeNil(), "claim %s/%s has no allocation", ns, claimName)

	var devices []string
	for _, r := range rc.Status.Allocation.Devices.Results {
		if r.Driver == "gpu.nvidia.com" {
			devices = append(devices, r.Device)
		}
	}
	return devices
}

// expectPartitionAllocation asserts the pod's claim holds exactly the members
// of one size-N partition on the pod's node, and returns that partition.
func expectPartitionAllocation(ctx context.Context, topo fmTopology, ns, pod string, size int) fmPartition {
	node := podNode(ctx, ns, pod)
	devices := podClaimDevices(ctx, ns, pod)
	Expect(devices).To(HaveLen(size), "pod %s/%s allocated %v, want %d devices", ns, pod, devices, size)

	byName := map[string]fmDevice{}
	for _, d := range topo[node] {
		byName[d.Name] = d
	}
	var id int64
	for i, name := range devices {
		d, ok := byName[name]
		Expect(ok).To(BeTrue(), "allocated device %s publishes no Fabric Manager attributes on %s", name, node)
		pid, ok := d.Partitions[size]
		Expect(ok).To(BeTrue(), "allocated device %s has no partition%d attribute", name, size)
		if i == 0 {
			id = pid
		}
		Expect(pid).To(Equal(id), "pod %s/%s allocated devices from different %d-GPU partitions: %v", ns, pod, size, devices)
	}
	members := topo.partitions(node, size)[id]
	Expect(members).To(HaveLen(size), "partition%d=%d on %s is not fully published", size, id, node)
	return fmPartition{Node: node, Size: size, ID: id, Members: members}
}

var nvidiaSMIGPU = regexp.MustCompile(`(?m)^GPU [0-9]+:`)

// expectVisibleGPUs asserts nvidia-smi inside the container lists exactly n GPUs.
func expectVisibleGPUs(ctx context.Context, ns, pod string, n int) {
	out, err := framework.ExecInPod(ctx, restConfig, cs, ns, pod, "ctr", []string{"nvidia-smi", "-L"})
	Expect(err).NotTo(HaveOccurred(), "exec nvidia-smi in %s/%s: %s", ns, pod, out)
	Expect(nvidiaSMIGPU.FindAllString(out, -1)).To(HaveLen(n), "nvidia-smi -L in %s/%s:\n%s", ns, pod, out)
}

// expectPodEvent waits for an event on the pod whose message contains substr.
func expectPodEvent(ctx context.Context, ns, pod, substr string) {
	sel := fields.OneTermEqualSelector("involvedObject.name", pod).String()
	Eventually(func() string {
		events, err := cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: sel})
		if err != nil {
			return ""
		}
		var msgs strings.Builder
		for _, e := range events.Items {
			msgs.WriteString(e.Message)
			msgs.WriteString("\n")
		}
		return msgs.String()
	}, 3*time.Minute, 5*time.Second).Should(ContainSubstring(substr), "no event on pod %s/%s mentions %q", ns, pod, substr)
}
