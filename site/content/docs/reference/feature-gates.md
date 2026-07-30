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
| `FabricManagerPartitioning` | Alpha | `false` | Enables Fabric Manager partition discovery and lifecycle management for full GPUs and, with `PassthroughSupport`, VFIO devices on supported HGX and single-node NVL systems. Requires Fabric Manager with `FABRIC_MODE=1`; each full-GPU or VFIO claim on a participating node must exactly match one published partition. See [Fabric Manager partitioning](../guides/gpu-allocation/fabric-manager-partitioning.md). |
| `HostManagedIMEXDaemon` | Alpha | `false` | Allows `resources.computeDomains.imex.mode=hostManaged`, in which you manage the host `nvidia-imex` service instead of the DRA Driver creating per-ComputeDomain daemon DaemonSets, without changing the lifecycle unless you also set the mode. Driver-managed DNS daemon naming and `ComputeDomainClique` tracking are not used in this mode. See [Validate host-managed IMEX](../guides/compute-domain-workloads.md#validate-host-managed-imex). |
| `DRAListTypeAttributes` | Alpha | `false` | Publishes list-valued DRA device attributes, including `resource.kubernetes.io/numaNode` as a one-element list, and requires the Kubernetes feature gate with the same name on the kube-apiserver and kube-scheduler. See [NUMA locality](resourceslice-attributes.md#numa-locality). |


## Constraints

The following feature gate combinations are mutually exclusive and cannot be enabled together.

| Combination | Reason |
|---|---|
| `DynamicMIG` + `PassthroughSupport` | Mutually exclusive |
| `DynamicMIG` + `NVMLDeviceHealthCheck` | Mutually exclusive |
| `DynamicMIG` + `MPSSupport` | Mutually exclusive |
| `PassthroughSupport` + `NVMLDeviceHealthCheck` | Mutually exclusive |

The feature gates below have the following dependencies:

| Feature gate | Requires |
|---|---|
| `ComputeDomainCliques` | `IMEXDaemonsWithDNSNames` |
| `DeviceMetadata` | `PassthroughSupport` |
