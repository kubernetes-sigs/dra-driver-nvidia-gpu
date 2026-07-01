---
title: KubeVirt VFIO GPU passthrough
linkTitle: KubeVirt VFIO GPU passthrough
weight: 50
description: Configure the DRA driver for NVIDIA GPUs for KubeVirt VFIO passthrough.
---

KubeVirt can attach VFIO GPU devices to virtual machines through Kubernetes Dynamic Resource Allocation (DRA). This guide covers the DRA Driver for NVIDIA GPUs configuration needed for KubeVirt passthrough workloads.

For KubeVirt feature gates, VM fields, and VM examples, see the [KubeVirt user guide](https://kubevirt.io/user-guide/).

## Feature status

This guide uses two Alpha feature gates, both disabled by default:

| Feature gate | Default | Stage |
|---|---|---|
| `PassthroughSupport` | `false` | Alpha |
| `DeviceMetadata` | `false` | Alpha |

See the [Feature gates](../reference/feature-gates/) and [constraints](../reference/feature-gates/#constraints) for details.

## Prerequisites

- Meet the general driver [Prerequisites](../prerequisites/).
- **IOMMU enabled** on GPU nodes. VFIO passthrough requires IOMMU; the GPU kubelet plugin fails to start with `PassthroughSupport` enabled if IOMMU is off.
- Use **DRA Driver for NVIDIA GPUs v0.4.0 or later** with the **`PassthroughSupport`** and **`DeviceMetadata`** feature gates enabled.
- Use **KubeVirt v1.8.0 or later** with the **`GPUsWithDRA`** feature gate enabled. This gate is enabled by default from KubeVirt v1.9.0, so it only needs to be set explicitly on v1.8.x.

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
