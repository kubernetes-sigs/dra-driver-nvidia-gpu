---
title: GPU sharing
linkTitle: GPU sharing
weight: 25
description: Choose how workloads share GPUs and compare user-mediated and system-mediated allocation.
---

GPU sharing enables multiple workloads to use one physical GPU.
First, choose one of two allocation models:

1. In user-mediated sharing, you configure workloads to reference one `ResourceClaim`.
2. In system-mediated sharing, Kubernetes can allocate independent `ResourceClaim` objects to the same device.

Next, choose how the GPU provides resources to those workloads through CUDA time-slicing, NVIDIA Multi-Process Service (MPS), or NVIDIA Multi-Instance GPU (MIG) partitioning.

## User-mediated sharing

Use user-mediated sharing when you know which containers or pods can share a GPU.

Multiple workloads reference the same `ResourceClaim`, so Kubernetes allocates the device once and schedules the pods on a node that can access it.
Because a `ResourceClaim` is namespace-scoped, all pods that reference it must belong to the same namespace.

The workloads can use default CUDA time-slicing, configured CUDA time-slicing, or MPS.

## System-mediated sharing

Use system-mediated sharing when your workloads need independent claims and you want Kubernetes to decide which claims share a device.

Each workload uses a separate `ResourceClaim`, and the Kubernetes scheduler can allocate the same GPU or MIG device to multiple claims.
Because the claims are separate, the pods can belong to different namespaces.

System-mediated sharing uses consumable capacity to determine whether another claim fits on a device.
Consumable capacity affects scheduling only; it does not enforce memory, compute, or performance limits at runtime.
MPS is not supported with system-mediated sharing.

## Compare the allocation models

| Characteristic | User-mediated sharing | System-mediated sharing |
|---|---|---|
| How workloads receive the device | Multiple workloads reference one `ResourceClaim` | Each workload uses an independent `ResourceClaim` |
| Who selects the workloads that share | You | The Kubernetes scheduler |
| Namespace support | All workloads must be in the claim's namespace | Workloads can be in different namespaces |
| Best fit | A known group of cooperating workloads | Independent workloads, replicas, or workloads managed by different teams |

## Choose how the GPU runs workloads

| Option | Choose this option when | Important consideration |
|---|---|---|
| Default CUDA time-slicing | Workloads can share the GPU on a best-effort basis | No memory or performance isolation |
| Configured CUDA time-slicing | A full GPU requires a non-default context-switch interval | No memory or performance isolation |
| MPS | Cooperating CUDA applications require concurrent execution and coarse-grained resource controls | No hard throughput guarantee; user-mediated sharing only |
| Separate MIG devices | Workloads must not affect one another through the GPU | Requires MIG-capable hardware |

MIG is a partitioning mechanism, not an allocation model.
Workloads allocated separate MIG devices receive hardware isolation; workloads sharing the same MIG device do not receive isolation from one another.

For more information about full GPUs, MIG devices, and the resource types that support each option, refer to [GPU allocation](gpu-allocation.md).

## Choose how to share a GPU

| Workload goal | Recommended configuration |
|---|---|
| A known group of workloads in one namespace can share a GPU on a best-effort basis | Reference one `ResourceClaim` and use default CUDA time-slicing |
| A known group requires a non-default CUDA context-switch interval | Reference one `ResourceClaim` and configure time-slicing |
| Cooperating CUDA applications require concurrent execution and coarse-grained resource controls | Reference one `ResourceClaim` and configure MPS |
| Independent workloads or replicas can share GPUs, including across namespaces | Use independent claims with consumable capacity |
| Workloads must not affect one another through the GPU | Allocate an exclusive full GPU or a separate MIG device to each workload |

## Feature availability

Configured CUDA time-slicing, MPS, and consumable capacity are Alpha driver features and are disabled by default.
Consumable capacity also has Kubernetes version requirements.

Refer to [Feature gates](../reference/feature-gates.md) for availability and compatibility requirements.

## Configure GPU sharing

Use the [Time-slicing guide](../guides/gpu-allocation/time-slicing.md) to configure a CUDA time-slice interval.

Use the [MPS guide](../guides/mps.md) to configure concurrent execution and MPS resource controls.

Use the [Consumable capacity guide](../guides/gpu-allocation/consumable-capacity.md) to configure independent claims and scheduler capacity accounting.

Example manifests for user-mediated sharing are available in [`demo/specs/quickstart/`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/tree/{{< param driver_release_tag >}}/demo/specs/quickstart).

Example manifests for system-mediated sharing are available in [`demo/specs/consumable-shares/`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/tree/{{< param driver_release_tag >}}/demo/specs/consumable-shares).
