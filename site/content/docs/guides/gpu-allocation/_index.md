---
title: GPU workloads
linkTitle: GPU workloads
weight: 10
description: Task-oriented walkthroughs for running GPU workloads.
---

Start with [GPU allocation](../../concepts/gpu-allocation.md) to choose a base
resource type: full GPU, MIG, or VFIO passthrough. Then use the corresponding
guide to request that resource.

Time-slicing and Multi-Process Service are optional sharing strategies.
Fabric Manager partitioning is an optional topology layer for full-GPU and
VFIO devices.
Refer to
[Fabric Manager partitioning](fabric-manager-partitioning.md) to select and
activate one complete NVSwitch partition.

For example manifests, refer to
[`demo/`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/tree/{{< param driver_release_tag >}}/demo)
in the repository.
