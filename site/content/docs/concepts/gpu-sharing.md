---
title: GPU sharing
linkTitle: GPU sharing
weight: 25
description: Choose how workloads share GPUs and compare user-mediated and system-mediated allocation.
---

GPU sharing enables multiple workloads to use one physical GPU.
You can choose one of two allocation models:

- In user-mediated sharing, you configure workloads to reference one `ResourceClaim`.
- In system-mediated sharing, Kubernetes can allocate independent `ResourceClaim` objects to the same device.

Workloads that share a GPU use NVIDIA CUDA time-slicing or Multi-Process Service (MPS).

## User-mediated sharing

Use user-mediated sharing when you know which containers or pods can share a GPU.

Multiple workloads reference the same `ResourceClaim`, so Kubernetes allocates the device and schedules the pods on a node that can access the GPU.
Because a `ResourceClaim` is namespace-scoped, all pods that reference the claim must belong to the same namespace.

You can share a device within one pod or across pods:

- To share a device across containers in one pod, reference the same claim from each container in the pod specification.
- To share a device across pods, create a `ResourceClaim` and reference it by name from each pod.
  A `ResourceClaimTemplate` creates a separate claim for each pod, so pods that use a template do not share a device.

By default, CUDA time-slices the workloads that share a GPU.
To change the time-slice interval, configure time-slicing.
To run the workloads concurrently, use MPS.

## System-mediated sharing

Use system-mediated sharing when workloads are managed independently, such as replicas or workloads in different namespaces.

Each workload uses a separate `ResourceClaim`, and the Kubernetes scheduler can allocate the same GPU or Multi-Instance GPU (MIG) device to multiple claims.
Because the claims are separate, the pods can belong to different namespaces.

System-mediated sharing uses consumable capacity.
Each claim requests a share of a device, such as an amount of GPU memory.
The scheduler allocates a claim to a device only if the request fits in the remaining capacity.
Nothing enforces the request after a workload starts, so a workload can use more than it requested.
If you enable consumable capacity for the driver, no workload can use MPS.

## Compare the allocation models

The following table compares the two allocation models:

| Characteristic | User-mediated sharing | System-mediated sharing |
|---|---|---|
| How workloads receive the device | Multiple workloads reference one `ResourceClaim` | Each workload uses an independent `ResourceClaim` |
| Who selects the workloads that share | You | The Kubernetes scheduler |
| Namespace support | All workloads must be in the claim's namespace | Workloads can be in different namespaces |
| Best fit | A known group of cooperating workloads | Independent workloads, replicas, or workloads managed by different teams |

## Choose a sharing configuration

The following table recommends a configuration for common workload goals:

| Workload goal | Recommended configuration | Important consideration |
|---|---|---|
| Workloads in one namespace can share a GPU on a best-effort basis | Reference one `ResourceClaim` and use [default CUDA time-slicing](../guides/gpu-allocation/allocating-gpus.md#share-a-gpu-across-containers-in-a-pod) | No memory or performance isolation |
| Workloads require a non-default CUDA context-switch interval | Reference one `ResourceClaim` and [configure time-slicing](../guides/gpu-allocation/time-slicing.md) | Full GPUs only, no memory or performance isolation |
| CUDA applications require concurrent execution and coarse-grained resource controls | Reference one `ResourceClaim` and [configure MPS](../guides/mps.md) | No hard throughput guarantee, cannot be used if consumable capacity is enabled |
| Independent workloads or replicas can share GPUs, including across namespaces | Use independent claims with [consumable capacity](../guides/gpu-allocation/consumable-capacity.md) | Scheduling accounting only, no runtime memory or compute limits |
| Workloads must not affect one another through the GPU | Allocate an exclusive full GPU or a [separate MIG device](../guides/gpu-allocation/mig.md) to each workload | MIG requires MIG-capable hardware |

Workloads allocated separate MIG devices receive hardware isolation.
Workloads sharing the same MIG device do not receive isolation from one another.

For more information about full GPUs, MIG devices, and the resource types that support each option, refer to [GPU allocation](gpu-allocation.md).

## Feature availability

Configured CUDA time-slicing, MPS, and consumable capacity are Alpha driver features and are disabled by default.
The following table lists the requirements for each capability:

| Capability | Driver feature gate | Kubernetes requirement |
|---|---|---|
| Default CUDA time-slicing | None | None |
| Configured CUDA time-slicing | `TimeSlicingSettings` | None |
| MPS | `MPSSupport` | None |
| Consumable capacity | `ConsumableShares`, plus a `consumableShares` Helm value | v1.34 or later, with the [`DRAConsumableCapacity`](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#consumable-capacity) feature gate enabled on v1.34 and v1.35 |

You cannot enable `MPSSupport` and `DynamicMIG` together.
For all feature gate constraints, refer to [Feature gates](../reference/feature-gates.md).

## Example manifests

Example manifests for user-mediated sharing are available in [`demo/specs/quickstart/`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/tree/{{< param driver_release_tag >}}/demo/specs/quickstart).

Example manifests for system-mediated sharing are available in [`demo/specs/consumable-shares/`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/tree/{{< param driver_release_tag >}}/demo/specs/consumable-shares).
