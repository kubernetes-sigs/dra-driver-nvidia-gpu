---
title: 0001 — VFIO-only GPU nodes
linkTitle: VFIO-only GPU nodes
weight: 1
description: Run the GPU kubelet plugin on dedicated passthrough nodes without a host NVIDIA driver.
---

| Field | Value |
| --- | --- |
| Status | provisional |
| Created | 2026-11-04 |
| Related issues | [#1555](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/issues/1555) |

## Summary

Add an explicit VFIO-only mode under the existing alpha `PassthroughSupport` gate.
Dedicated passthrough nodes can discover and allocate GPUs without host NVIDIA libraries, binaries, or kernel modules.
They publish only VFIO devices and leave released devices bound to VFIO.

## Motivation

KubeVirt passthrough workloads use the NVIDIA driver inside the guest.
Requiring the same driver on the host adds an unnecessary installation and lifecycle dependency when the node has no NVLink fabric.

The goals are driver-free startup, stable VFIO-only resource publication, and driver-free release and restart recovery.
Mixed container and VM workloads on the same node, fabric management without the host driver, and automatic mode transitions are outside this change.

## Why this belongs in the NVIDIA DRA driver

The GPU kubelet plugin owns device discovery, ResourceSlice publication, PCI rebinding, and claim cleanup.
DeviceClass selectors cannot remove its startup dependency on NVML.
GPU Operator remains responsible for configuring the host software; no upstream Kubernetes change is needed.

## Proposal

### User-facing example

Use these Helm values with the existing GPU resource enablement settings:

```yaml
vfioOnly: true
nvidiaDriverRoot: /
resources:
  gpus:
    enabled: true
  computeDomains:
    enabled: false
featureGates:
  PassthroughSupport: true
```

The binary exposes `--vfio-only`, also configurable through `VFIO_ONLY`.
Both default to false.
Deploy the plugin to the intended node pool with the existing node selector and toleration settings.

### Affected components and authoritative state owner

The GPU kubelet plugin remains the sole owner of prepared device state and its checkpoint.
Changes affect its discovery and cleanup paths, Helm validation and container environments, the shared init script, documentation, and tests.
There are no API, CRD, RBAC, checkpoint schema, controller, daemon, or webhook changes.

### Smallest valuable slice

Support dedicated nodes without NVSwitch/NVLink fabric using the existing VFIO allocation and CDI paths.

## Design

### API changes and feature gate

The user-facing additions are the boolean CLI flag, environment variable, and Helm value.
The existing `PassthroughSupport` gate guards the mode; no new gate or graduation is proposed.
IOMMU is required.
The chart rejects ComputeDomains and Fabric Manager partitioning in this mode.
The binary rejects Fabric Manager partitioning and detects NVSwitch PCI devices and switch-managed NVLink5 fabric before discovery.
NVML health monitoring is skipped because it requires a host NVIDIA driver.

### Device lifecycle

Use PCI discovery without probing NVML or locating NVIDIA binaries.
Publish no GPU or MIG siblings.
Preparation accepts unbound or VFIO-bound GPUs and rejects NVIDIA-bound GPUs rather than calling NVIDIA tools.
Release leaves devices on VFIO and skips NVIDIA sibling rediscovery.
Existing serialized Prepare/Unprepare calls and driver-switch retry protection continue to apply.

### Upgrade and downgrade

Defaults preserve existing behavior.
The configured mode stays fixed for the lifetime of the plugin and across restarts.
Drain GPU workloads and release claims before switching modes or rolling back to a version that requires a host NVIDIA driver.
Checkpoints retain their existing format.
GPU Operator must omit the host driver and host GPU clients from the dedicated node pool.

### Environment floor

Existing Kubernetes DRA and VFIO prerequisites apply.
Host NVIDIA driver versions no longer constrain VFIO-only startup.
The guest still needs its workload's NVIDIA driver.
Nodes requiring NVIDIA fabric management are excluded.

### Test plan

Unit tests cover startup and checkpoint recovery without driver files, PCI-only enumeration, published VFIO attributes, release without NVIDIA rediscovery, missing prerequisites, and fabric detection.
Helm tests cover environment propagation, invalid combinations, and the init script's driver-check bypass.
Run the repository race-enabled unit suite and Helm lint.
Hardware validation should cover a KubeVirt claim's prepare, VM execution, release, plugin restart with a prepared claim, and node reboot on a dedicated GPU node.
The mock-NVML lane does not prove real kernel rebinding or VM operation; those require hardware coverage.

## Risks and drawbacks

An explicit mode requires node-pool configuration and coordination with GPU Operator.
Changing it with existing claims can invalidate the assumptions under which those claims were prepared.
Fabric detection excludes known switch-managed systems; NVML health reporting is unavailable without the driver.

## Alternatives

Automatically falling back when NVML fails would turn transient host-driver problems into changes in the advertised resource types.
An explicit mode preserves failure behavior on ordinary GPU nodes.
The implementation reuses go-nvlib PCI discovery, VFIO variant selection, and the driver's existing CDI and checkpoint machinery.

## Open questions

Maintainers should confirm the public option name and the real-hardware lane used for driver-free passthrough coverage.
