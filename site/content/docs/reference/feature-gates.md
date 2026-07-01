---
title: Feature gates
linkTitle: Feature gates
weight: 20
description: Available feature gates, their stages, defaults, and constraints.
---

Feature gates control experimental and beta functionality in the DRA Driver for NVIDIA GPUs. They follow [Kubernetes feature gate conventions](https://kubernetes.io/docs/reference/command-line-tools-reference/feature-gates/).

## Set feature gates

Set feature gates in your Helm values file:

```yaml
featureGates:
  TimeSlicingSettings: true
  MPSSupport: true
```

Or pass them at install time:

```bash
helm install nvidia-dra-driver-gpu nvidia/nvidia-dra-driver-gpu \
    --set "featureGates.TimeSlicingSettings=true"
```

## Available feature gates

| Feature gate | Stage | Default | Description |
|---|---|---|---|
| `TimeSlicingSettings` | Alpha | `false` | Enables customization of CUDA time-slicing settings in `GpuConfig`. |
| `MPSSupport` | Alpha | `false` | Enables Multi-Process Service (MPS) sharing strategy in `GpuConfig` and `MigDeviceConfig`. |
| `IMEXDaemonsWithDNSNames` | Beta | `true` | IMEX daemons use DNS names instead of raw IP addresses for peer communication. Required by `ComputeDomainCliques`. |
| `PassthroughSupport` | Alpha | `false` | Enables VFIO passthrough device allocation using `VfioDeviceConfig`. |
| `DynamicMIG` | Alpha | `false` | Enables dynamic MIG device allocation and reconfiguration. Kubernetes 1.33–1.35 requires `DRAPartitionableDevices` enabled on the kube-apiserver and kube-scheduler (see [Kubernetes feature gates](https://kubernetes.io/docs/reference/command-line-tools-reference/feature-gates/#DRAPartitionableDevices)). |
| `NVMLDeviceHealthCheck` | Alpha | `false` | Enables GPU health checking using NVML. |
| `ComputeDomainCliques` | Beta | `true` | Uses `ComputeDomainClique` CRD objects to track IMEX daemon membership. Requires `IMEXDaemonsWithDNSNames`. |
| `CrashOnNVLinkFabricErrors` | Beta | `true` | Causes the kubelet plugin to crash rather than fall back to non-fabric mode when NVLink fabric errors are detected. |
| `DeviceMetadata` | Alpha | `false` | Enables IOMMU API device exposure (`/dev/iommu` or `/dev/vfio/vfio`) for VFIO workloads via `VfioDeviceConfig`. Requires `PassthroughSupport`. |
| `FabricManagerPartitioning` | Alpha | `false` | Enables Fabric Manager (NVSwitch) partition management for Passthrough VFIO devices. Requires `PassthroughSupport`. |

## Constraints

The following feature gate combinations are mutually exclusive and can not be enabled together.

| Combination | Reason |
|---|---|
| `DynamicMIG` + `PassthroughSupport` | Mutually exclusive |
| `DynamicMIG` + `NVMLDeviceHealthCheck` | Mutually exclusive |
| `DynamicMIG` + `MPSSupport` | Mutually exclusive |
| `PassthroughSupport` + `NVMLDeviceHealthCheck` | Mutually exclusive |
| `MPSSupport` + `NVMLDeviceHealthCheck` | Mutually exclusive |

The feature gates below have the following dependencies:

| Feature gate | Requires |
|---|---|
| `ComputeDomainCliques` | `IMEXDaemonsWithDNSNames` |
| `DeviceMetadata` | `PassthroughSupport` |
| `FabricManagerPartitioning` | `PassthroughSupport` |
| `DynamicMIG` (Kubernetes 1.34–1.35) | `DRAPartitionableDevices` Kubernetes feature gate enabled on kube-apiserver and kube-scheduler |
