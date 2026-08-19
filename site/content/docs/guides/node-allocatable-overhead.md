---
title: Node-allocatable overhead
linkTitle: Node-allocatable overhead
weight: 65
description: >
  Declare per-device host memory and CPU overhead so the scheduler reserves it
  and the kubelet sizes cgroups accordingly (KEP-5517).
---

Workloads that use GPUs also consume ordinary node resources that never appear
in their pod spec: the NVIDIA driver and CUDA runtime pin host memory per
process, and auxiliary threads consume CPU. Without accounting, the scheduler
can overcommit node memory, and the kubelet sizes the pod's cgroups purely
from the pod spec, so a workload can be OOM-killed for host allocations made
on its behalf.

The `NodeAllocatableResources` feature gate enables
[KEP-5517](https://github.com/kubernetes/enhancements/issues/5517) support:
each published device carries a `nodeAllocatableResources` entry declaring the
host `memory` and `cpu` overhead a pod incurs by claiming it. The Kubernetes
scheduler debits these amounts from the node's allocatable budget (the same
ledger used by standard pod-spec requests) and records the result in
`pod.status.nodeAllocatableResourceClaimStatuses`; the kubelet reads that
status and adds the amounts to the pod-level cgroup requests and limits and to
the container-level limits.

Only the `overhead` branch of the KEP-5517 API is published. GPUs are not
themselves node resources, so the `mapping` branch (used by CPU or memory DRA
drivers) does not apply.

## Prerequisites

- Kubernetes 1.37 or newer with the `DRANodeAllocatableResources` feature gate
  (alpha) enabled on the API server, kube-scheduler, and kubelet. Without it,
  the API server silently drops the field from published ResourceSlices.
- The driver's `NodeAllocatableResources` feature gate enabled via
  `featureGates.NodeAllocatableResources=true`.

## Configuration

Overhead values are optional Kubernetes resource quantities, configured per
device class:

| Device class | Covers | Helm values |
|---|---|---|
| `gpu` | full GPUs | `nodeAllocatableOverhead.gpu.{memory,cpu}.{perPod,perContainer}` |
| `mig` | statically and dynamically partitioned MIG devices | `nodeAllocatableOverhead.mig.{memory,cpu}.{perPod,perContainer}` |
| `vfio` | passthrough devices | `nodeAllocatableOverhead.vfio.{memory,cpu}.{perPod,perContainer}` |

`perPod` is charged once per pod referencing a claim for the device;
`perContainer` is charged for each container referencing the claim. Empty or
zero values are omitted, and a class with no values publishes nothing, so the
default configuration changes nothing. Setting any value while the driver
feature gate is disabled is a startup error.

Example:

```bash
helm upgrade -i nvidia dra-driver-nvidia-gpu \
  --set featureGates.NodeAllocatableResources=true \
  --set nodeAllocatableOverhead.gpu.memory.perPod=256Mi \
  --set nodeAllocatableOverhead.gpu.memory.perContainer=64Mi \
  --set nodeAllocatableOverhead.gpu.cpu.perPod=500m \
  --set nodeAllocatableOverhead.mig.memory.perPod=128Mi
```

The equivalent kubelet-plugin flags are
`--node-allocatable-<class>-<resource>-overhead-per-<pod|container>` with
matching `NODE_ALLOCATABLE_*` environment variables.

## What to expect

With the example values above, a pod with a `128Mi` memory limit claiming one
full GPU results in:

- the `gpu-0` device in the node's ResourceSlice carrying
  `nodeAllocatableResources` with the configured overhead,
- `pod.status.nodeAllocatableResourceClaimStatuses` listing the claim, the
  referencing containers, and the charged overhead,
- a pod-level cgroup `memory.max` of 448Mi (128Mi limit + 256Mi perPod + 64Mi
  perContainer) and a pod-level `cpu.weight` reflecting the 500m CPU overhead,
- unschedulable (`Insufficient memory`) pods instead of node overcommit when
  the overhead does not fit the node's remaining allocatable budget.

The pod's QoS class is unchanged by DRA overhead (a KEP-5517 design decision):
a pod with no pod-spec requests remains BestEffort even when its claims carry
overhead. Give pods at least a small standard request to avoid BestEffort
eviction behavior.

## Caveats

- `vfio`: VM stacks such as KubeVirt typically account for pinned guest RAM
  through the VM launcher pod's own memory requests; configuring vfio overhead
  on top of that double counts. Leave the vfio values empty unless your VM
  stack does not account for it. Note that `PassthroughSupport` itself
  requires an IOMMU-capable host.
- Overhead values are supplied by the cluster administrator. Measure your
  workloads' real per-process host footprint before choosing values;
  over-reserving wastes node capacity, under-reserving reintroduces the OOM
  risk the feature exists to prevent.
- If the cluster-side `DRANodeAllocatableResources` gate is missing, the field
  is dropped by the API server and the resourceslice controller logs recurring
  `features are disabled` errors while overhead accounting silently stays off.
