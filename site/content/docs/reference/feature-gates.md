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
helm install dra-driver-nvidia-gpu oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
    --version {{< param "driver_version" >}} \
    --namespace dra-driver-nvidia-gpu \
    --set "featureGates.TimeSlicingSettings=true"
```

## Available feature gates

| Feature gate | Stage | Default | Description |
|---|---|---|---|
| `TimeSlicingSettings` | Alpha | `false` | Enables customization of CUDA time-slicing settings in `GpuConfig`. |
| `MPSSupport` | Alpha | `false` | Enables Multi-Process Service (MPS) sharing strategy in `GpuConfig` and `MigDeviceConfig`. |
| `IMEXDaemonsWithDNSNames` | Beta | `true` | IMEX daemons use DNS names instead of raw IP addresses for peer communication. Required by `ComputeDomainCliques`. |
| `PassthroughSupport` | Alpha | `false` | Enables VFIO passthrough device allocation using `VfioDeviceConfig`. |
| `DynamicMIG` | Alpha | `false` | Enables dynamic MIG device allocation and reconfiguration. See [MIG](../guides/gpu-allocation/mig.md). Kubernetes v1.34–v1.35 requires `DRAPartitionableDevices` enabled on the kube-apiserver and kube-scheduler (enabled by default on Kubernetes v1.36 and later) (see [Kubernetes feature gates](https://kubernetes.io/docs/reference/command-line-tools-reference/feature-gates/#DRAPartitionableDevices)). |
| `NVMLDeviceHealthCheck` | Alpha | `false` | Enables GPU health checking using NVML. |
| `ComputeDomainCliques` | Beta | `true` | Uses `ComputeDomainClique` CRD objects to track IMEX daemon membership. Requires `IMEXDaemonsWithDNSNames`. |
| `CrashOnNVLinkFabricErrors` | Beta | `true` | Causes the kubelet plugin to crash rather than fall back to non-fabric mode when NVLink fabric errors are detected. |
| `DeviceMetadata` | Alpha | `false` | Enables IOMMU API device exposure (`/dev/iommu` or `/dev/vfio/vfio`) for VFIO workloads via `VfioDeviceConfig`. Requires `PassthroughSupport`. |
| `FabricManagerPartitioning` | Alpha | `false` | Enables Fabric Manager (NVSwitch) partition management in single-node NVL systems for full-GPU (`gpu.nvidia.com`) devices and Passthrough VFIO devices. Requires Fabric Manager running with `FABRIC_MODE=1`. When combined with `PassthroughSupport`, both device types are partitioned; otherwise only full-GPU devices. **Prepare requires the allocated physical-GPU set to match an FM partition exactly**; non-matching claims (including ordinary multi-GPU full-GPU workloads without a `matchAttribute` on `gpu.nvidia.com/partitionN`) fail Prepare. Do not enable on nodes that still run unconstrained full-GPU claims. |
| `SeamlessUpgrades` | Alpha | `false` | Enables seamless (zero-downtime) rolling updates of the kubelet plugin DaemonSet: each plugin instance derives unique, pod-UID-suffixed kubelet registration and DRA socket names, so that an old and a new instance can serve concurrently during a surge-based rolling update. Use together with a `kubeletPlugin.updateStrategy` of `maxSurge: 1`, `maxUnavailable: 0` (both must be set: partially overriding only `maxSurge` merges with the default `maxUnavailable: "100%"` and is rejected). Requires a kubelet with support for multiple registrations of the same DRA driver (Kubernetes v1.33 and later). |

## Constraints

The following feature gate combinations are mutually exclusive and cannot be enabled together.

| Combination | Reason |
|---|---|
| `DynamicMIG` + `PassthroughSupport` | Mutually exclusive |
| `DynamicMIG` + `NVMLDeviceHealthCheck` | Mutually exclusive |
| `DynamicMIG` + `MPSSupport` | Mutually exclusive |
| `PassthroughSupport` + `NVMLDeviceHealthCheck` | Mutually exclusive |
| `MPSSupport` + `NVMLDeviceHealthCheck` | Mutually exclusive |
| `SeamlessUpgrades` + `NVMLDeviceHealthCheck` | Mutually exclusive: device taints are instance-local in-memory state, and two concurrently publishing plugin instances would overwrite each other's ResourceSlices |
| `SeamlessUpgrades` + `PassthroughSupport` | Mutually exclusive: sibling-device removal on prepare/unprepare is instance-local in-memory state, and two concurrently publishing plugin instances would overwrite each other's ResourceSlices |

The feature gates below have the following dependencies:

| Feature gate | Requires |
|---|---|
| `ComputeDomainCliques` | `IMEXDaemonsWithDNSNames` |
| `DeviceMetadata` | `PassthroughSupport` |
| `DynamicMIG` (Kubernetes 1.34–1.35) | `DRAPartitionableDevices` Kubernetes feature gate enabled on kube-apiserver and kube-scheduler |
