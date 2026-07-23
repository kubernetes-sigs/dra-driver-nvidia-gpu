---
title: KubeVirt VFIO GPU passthrough
linkTitle: KubeVirt VFIO GPU passthrough
weight: 50
aliases:
  - /docs/guides/kubevirt-vfio-gpu-passthrough/
description: Configure the DRA driver for NVIDIA GPUs for KubeVirt VFIO passthrough.
---

KubeVirt can attach VFIO GPU devices to virtual machines through Kubernetes Dynamic Resource Allocation (DRA). This guide covers the DRA Driver for NVIDIA GPUs configuration needed for KubeVirt passthrough workloads.

For KubeVirt feature gates, VM fields, and VM examples, refer to the [KubeVirt user guide](https://kubevirt.io/user-guide/).

## Feature status

This guide uses Alpha feature gates that are disabled by default:

| Feature gate | Stage | Default | Description |
|---|---|---|---|
| `PassthroughSupport` | Alpha | `false` | Enables VFIO passthrough allocation. |
| `DeviceMetadata` | Alpha | `false` | Exposes the VFIO API device required by KubeVirt. |
| `FabricManagerPartitioning` | Alpha | `false` | Optionally manages Fabric Manager partitions for supported multi-GPU VFIO claims. |

Refer to the [feature gates reference](../../reference/feature-gates.md) and [constraints](../../reference/feature-gates.md#constraints) for details.

## Prerequisites

- Meet the general driver [prerequisites](../../prerequisites.md).
- **IOMMU enabled** on GPU nodes. VFIO passthrough requires IOMMU; the GPU kubelet plugin fails to start with `PassthroughSupport` enabled if IOMMU is off.
- Enable the `PassthroughSupport` and `DeviceMetadata` feature gates in the DRA Driver.
- Use **KubeVirt v1.8.0 or later** with the **`GPUsWithDRA`** feature gate enabled. This gate is enabled by default from KubeVirt v1.9.0, so it only needs to be set explicitly on v1.8.x.

### NVIDIA Grace VFIO module selection

The GPU kubelet plugin automatically selects the most specific VFIO module variant that matches each GPU PCI modalias.
This selection supports NVIDIA Grace systems that provide a platform-specific `vfio_pci` variant and falls back to the standard `vfio-pci` module when no specific alias matches.
The chart mounts the host `/lib/modules` directory into the GPU kubelet plugin.
If your node image manages kernel modules outside the standard path, ensure that `/lib/modules/$(uname -r)/modules.alias` exists on the host before you enable `PassthroughSupport`.
When sysfs associates a graphics auxiliary PCI function with the GPU through a `consumer:pci:*` link, the passthrough helper unbinds that related function as part of the GPU transition.

## Limitations and considerations

For a GPU to switch between the `nvidia` and `vfio-pci` drivers, nothing on the host may hold an open handle on that GPU's `/dev/nvidia*` device nodes.

Make sure each of the following is either not using the GPU being prepared, or is configured to release it:

| Component | Notes |
|---|---|
| **display-manager (Xorg)** | **Must be disabled.** |
| **nvidia-device-plugin** | **Must be disabled.** Not supported on the same node as the DRA Driver, which already handles both container and passthrough allocation. Exclude it from these nodes with a node selector or taint. |
| **nvidia-persistenced** | On DRA Driver v0.4.0 or later, the DRA Driver automatically disables persistence mode on the target GPU before rebinding it to `vfio-pci` and restores it afterward, so the service can keep running. Older versions don't have this handling and require stopping the service manually. |
| **dcgm** | Recommended to disable this service. **Note:** DCGM v4.5.0+ can automatically release GPUs when switching between the `nvidia` and `vfio-pci` drivers. |
| **dcgm-exporter** | Recommended to disable this service. **Note:** dcgm-exporter v4.5.0+ has an experimental feature, `DCGM_EXPORTER_ENABLE_GPU_BIND_UNBIND_WATCH=true` (plus a poll frequency `DCGM_EXPORTER_GPU_BIND_UNBIND_POLL_INTERVAL=1s`), to automatically release GPUs when switching between the `nvidia` and `vfio-pci` drivers. Use with caution. |

## Install the DRA Driver

Install the driver with passthrough support and device metadata:

```bash
helm upgrade -i dra-driver-nvidia-gpu oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
    --version {{< param "driver_version" >}} \
    --namespace dra-driver-nvidia-gpu \
    --create-namespace \
    --set resources.gpus.enabled=true \
    --set resources.computeDomains.enabled=false \
    --set gpuResourcesEnabledOverride=true \
    --set nvidiaDriverRoot=/ \
    --set featureGates.PassthroughSupport=true \
    --set featureGates.DeviceMetadata=true
```

Set `nvidiaDriverRoot` based on how the NVIDIA driver is installed on your nodes:

- `/` for a host-installed driver.
- `/run/nvidia/driver` for a GPU Operator-managed driver.
- `/home/kubernetes/bin/nvidia` for a GKE-managed driver.

### Optional Fabric Manager partitioning

Fabric Manager partitioning has the following requirements and limitations:

- Enable both `PassthroughSupport` and `FabricManagerPartitioning`.
- Use a supported HGX or single-node NVL system with an NVSwitch-managed fabric.
- Run Fabric Manager on the node and configure it to report the supported partition topology through its management interface.
- Make the Fabric Manager client library available under `nvidiaDriverRoot` and keep the default `/run/nvidia-fabricmanager/socket` path available relative to that root.
- Use the feature only for VFIO devices.
- Apply at most one `VfioDeviceConfig` group in each `ResourceClaim`.

The initial implementation does not manage multi-node Fabric Manager partitions.
Designate and label nodes for Fabric Manager VFIO workloads so that other GPU services do not use the GPUs while the claim is prepared.

Enable the optional gate on the existing Helm release:

```bash
helm upgrade -i dra-driver-nvidia-gpu oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
    --version {{< param "driver_version" >}} \
    --namespace dra-driver-nvidia-gpu \
    --reuse-values \
    --set featureGates.FabricManagerPartitioning=true
```

With the gate enabled, VFIO ResourceSlice devices publish `gpuModuleId` and the available `partition1`, `partition2`, `partition4`, and `partition8` attributes.
The GPU kubelet plugin activates the exact Fabric Manager partition before it binds the selected GPUs to VFIO and deactivates the partition after the GPUs return to the NVIDIA driver.
For the attribute definitions, see [ResourceSlice device attributes](../../reference/resourceslice-attributes.md#fabric-manager-partition-attributes).

Verify that the driver registered the expected `DeviceClass` and advertised node resources:

```bash
kubectl get deviceclass
```

Example output:

```
NAME                  AGE
gpu.nvidia.com        25s
mig.nvidia.com        25s
vfio.gpu.nvidia.com   25s
```

```bash
kubectl get resourceslice
```

Example output:

```
NAME                                                            NODE                                   DRIVER           POOL                                   AGE
00000-gpu.nvidia.com-dra-driver-nvidia-gpu-cluster-worker-tr5pp dra-driver-nvidia-gpu-cluster-worker gpu.nvidia.com   dra-driver-nvidia-gpu-cluster-worker   3m17s
```

VFIO devices carry the `gpu.nvidia.com/type` attribute value `vfio`. For the VFIO
device attributes you can match with CEL selectors, refer to
[ResourceSlice device attributes](../../reference/resourceslice-attributes.md#vfio-passthrough-type-vfio).
To inspect ResourceSlice output on your cluster, refer to
[View available GPU resources](view-resources.md).

## VFIO passthrough claim template

For KubeVirt GPU passthrough, create a `ResourceClaimTemplate` that uses the `vfio.gpu.nvidia.com` `DeviceClass`. A `ResourceClaimTemplate` is namespace-scoped and defines the GPU request and its configuration. Multiple pods can reuse the same template: Kubernetes automatically creates one `ResourceClaim` per pod from it, and deletes that claim when the pod terminates.

### Creating the claim template

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: dra-gpu-claim-template
spec:
  spec:
    devices:
      config:
      - requests:
        - dra-gpu
        opaque:
          driver: gpu.nvidia.com
          parameters:
            apiVersion: resource.nvidia.com/v1beta1
            kind: VfioDeviceConfig
            iommu:
              backendPolicy: LegacyOnly
              enableAPIDevice: true
      requests:
      - name: dra-gpu
        exactly:
          allocationMode: ExactCount
          count: 1
          deviceClassName: vfio.gpu.nvidia.com
```

Save it as `vfio-gpu-rct.yaml`.

### Apply the manifest

```bash
kubectl apply -f vfio-gpu-rct.yaml
```

Example output:

```
resourceclaimtemplate.resource.k8s.io/dra-gpu-claim-template created
```

### VfioDeviceConfig parameters

The opaque `VfioDeviceConfig` block tells the DRA Driver which VFIO device nodes to mount into virt-launcher through CDI.

- **`enableAPIDevice: true`** — Mounts the VFIO control device `/dev/vfio/vfio` into the virt-launcher pod. KubeVirt **requires** this device to manage VFIO PCI assignments through libvirt.

- **`backendPolicy: LegacyOnly`** — Selects the legacy IOMMU VFIO backend (`/dev/vfio/<iommu-group>`). The alternative, `PreferIommuFD`, uses the IOMMUFD backend (`/dev/vfio/devices/vfio*`) when available on the host.

Keep `backendPolicy: LegacyOnly` for KubeVirt, which does not support the IOMMUFD backend yet.

### Select a Fabric Manager partition

For a two-GPU VM on a Fabric Manager node, change the request count to `2` and require both GPUs to share a size-two partition:

```yaml
devices:
  requests:
  - name: dra-gpu
    exactly:
      allocationMode: ExactCount
      count: 2
      deviceClassName: vfio.gpu.nvidia.com
  constraints:
  - requests:
    - dra-gpu
    matchAttribute: gpu.nvidia.com/partition2
```

The scheduler selects two VFIO GPUs with the same `partition2` value.
Claim preparation fails if the selected GPU set does not exactly match a partition reported by Fabric Manager.
Keep a single `VfioDeviceConfig` block for all VFIO requests in the claim.

## Troubleshooting

If a VM fails to start because its virt-launcher pod is stuck in `ContainerCreating` state, check the kubelet-plugin logs for prepare errors:

```bash
kubectl logs -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=kubelet-plugin -c gpus
```

A `NodePrepareResources` failure looks something like this:

```
Warning  FailedPrepareDynamicResources  22s   kubelet  Failed to prepare dynamic resources: prepare dynamic resources: NodePrepareResources: rpc error: code = DeadlineExceeded desc = context deadline exceeded
```

A `DeadlineExceeded` error for `NodePrepareResources` for the workload usually means a service still holds an open handle to the GPU blocking its driver switch. On the GPU node, the culprit process can be identified using:

```bash
for f in /proc/[0-9]*/fd/*; do t=$(readlink "$f" 2>/dev/null) || continue; case "$t" in /dev/nvidia[0-9]*) echo "PID $(echo "$f" | cut -d/ -f3) holds $t";; esac; done
```

Example output:

```
PID 1210233 holds /run/nvidia/driver/dev/nvidia0
PID 1237013 holds /run/nvidia/driver/dev/nvidia1
```

If the command returns an output, note the PID holding the device node of the GPU being prepared, identify the owning service, and stop those services.

The subsequent invocation of `NodePrepareResources` call for the workload should then succeed, and the pod should reach the Running state.

If no process is holding the GPU and the VM still fails to start, open a bug.
