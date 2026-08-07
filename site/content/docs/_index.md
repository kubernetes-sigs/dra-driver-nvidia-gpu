---
title: DRA Driver for NVIDIA GPUs Overview
linkTitle: Docs
weight: 10
description: A Kubernetes DRA driver for NVIDIA GPUs, MIG, VFIO, and Multi-Node NVLink.
type: docs
cascade:
  type: docs
---
<meta name="algolia-site-verification"  content="80E709C1F52CF68E" />

The DRA Driver for NVIDIA GPUs is a Kubernetes [Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/) driver that enables flexible GPU allocation and provisioning of multi-node NVLink fabrics for Kubernetes workloads.
It requires Kubernetes 1.32 or later. Starting in Kubernetes 1.34, DRA is enabled by default. On Kubernetes 1.32 and 1.33, the `DynamicResourceAllocation` feature gate must be enabled.

The driver manages two types of resources:

- **GPUs** (`gpu.nvidia.com`) — allocates full GPUs, Multi-Instance GPU (MIG) slices, and Virtual Function I/O (VFIO) passthrough devices with fine-grained sharing and configuration control.
- **ComputeDomains** (`compute-domain.nvidia.com`) — provisions ephemeral multi-node NVLink fabrics using IMEX, enabling pods on different nodes to share GPU memory at full NVLink bandwidth.


The [Kubernetes device plugin framework](https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/device-plugins/) treats hardware resources as opaque countable integers. A workload either gets a whole unit or nothing at all. The device plugin model does not provide a way to express sharing, per-workload configuration, capability constraints, or topology requirements.

DRA is a replacement for the device plugin architecture itself, not just a different driver for the same interface. It brings hardware resource management closer to the Persistent Volume model: a workload declares what it needs in a `ResourceClaim`, and the driver fulfills it. This separation of declaration from consumption enables capabilities that are fundamentally out of scope for the device plugin framework:

- Share a single GPU across multiple containers or pods (time-slicing or MPS)
- Request GPUs by capability — memory size, compute capacity, architecture — rather than just count
- Attach per-workload configuration (sharing strategy, MIG profile, MPS thread limits) directly to the claim
- Allocate MIG devices dynamically without pre-configuring profiles on the node
- Provision scoped, ephemeral NVLink fabrics that exist only for the lifetime of the workload

## Use cases

### GPU allocation

Request GPUs, MIG slices, or VFIO passthrough devices in a ResourceClaim, with an optional sharing strategy such as time-slicing or MPS (Multi-Process Service).
Refer to [Architecture](concepts/architecture.md) for how requests are fulfilled.

### Multi-node NVLink with ComputeDomains

Create a `ComputeDomain` resource to provision an ephemeral NVLink fabric across nodes.
Workload pods claim a channel from the domain and receive the IMEX device mounts needed for direct cross-node GPU memory access.
The fabric is torn down automatically when the workload finishes.

## Get started

- [Install and validate](install.md)

## Learn more

- [Architecture](concepts/architecture.md) — components, request flows, and reference links.
- [API reference](reference/api/) — opaque config types for `ResourceClaim` specs (release {{< param driver_release_tag >}}).
- [Feature gates](reference/feature-gates/) — available feature flags and their defaults.
- [Helm chart values](reference/helm-values/) — chart configuration for release {{< param driver_version >}}.