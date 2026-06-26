---
title: KubeVirt VFIO GPU passthrough
linkTitle: KubeVirt VFIO GPU passthrough
weight: 40
description: Configure the DRA driver for NVIDIA GPUs for KubeVirt VFIO passthrough.
---

KubeVirt can attach VFIO GPU devices to virtual machines through Kubernetes Dynamic Resource Allocation (DRA). This guide covers the NVIDIA DRA driver configuration needed for KubeVirt passthrough workloads.

For KubeVirt feature gates, VMI fields, and VM examples, see the [KubeVirt user guide](https://kubevirt.io/user-guide/).

## Prerequisites

- Meet the general [driver prerequisites](../prerequisites.md).
- **IOMMU enabled** on GPU nodes. VFIO passthrough requires IOMMU; the GPU kubelet plugin fails to start with `PassthroughSupport` enabled if IOMMU is off.
- Use **NVIDIA DRA driver v0.4.0 or later**, which supports KEP-5304 device metadata.
- Install the driver with **`PassthroughSupport`** and **`DeviceMetadata`** featuregates enabled.

When deploying through the GPU Operator, CDI is enabled by default in GPU Operator v25.10.0 and later. See the [GPU Operator DRA installation guide](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/dra-intro-install.html).

## Install the NVIDIA DRA driver

Install the driver with passthrough support and device metadata:

```bash
helm upgrade -i dra-driver-nvidia-gpu oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
    --version 0.4.0 \
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
kubectl get resourceslice -o wide
```

### Stop services and processes using NVIDIA GPUs

Before a GPU can be rebound to `vfio-pci`, stop services and workloads that keep open handles on NVIDIA GPU device nodes such as `/dev/nvidia0` and `/dev/nvidia1`.

```bash
sudo systemctl stop nvidia-dcgm dcgm nvidia-persistenced nvsm
```

Verify that no other process holds an open handle on GPU devices:

```bash
sudo lsof /dev/nvidia[^-]
```

This command should produce no output.

Common processes that block GPU passthrough include:

- **Xorg** — Identify and disable the display manager:

```bash
  sudo systemctl status display-manager
  sudo systemctl disable <display-manager-name>
```

- **vectorAdd** — A sample CUDA application; stop the process if it is running.
- **nvidia-device-plugin** — Disable or uninstall the legacy NVIDIA device plugin.
- **dcgm-exporter** — Disable or uninstall DCGM exporter.
- **NVIDIA GPU Operator** — If deployed via Helm, its pods may run `nvidia-device-plugin` and `dcgm-exporter`. Disable or uninstall the operator on GPU nodes where you use DRA passthrough.
- **nvidia-persistenced** — Disabling this service is optional. The DRA driver can handle it automatically.
- **nvidia-dcgm/dcgm** — Disabling this service is optional when running driver v4.5.0 or later.

## VFIO passthrough claim template

For KubeVirt GPU passthrough, create a `ResourceClaimTemplate` that uses the `vfio.gpu.nvidia.com` `DeviceClass`.

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

#### VfioDeviceConfig parameters

The opaque `VfioDeviceConfig` block tells the NVIDIA DRA driver which VFIO device nodes to mount into virt-launcher through CDI.

**`enableAPIDevice: true`** — Mounts the VFIO control device `/dev/vfio/vfio` into the virt-launcher pod. KubeVirt **requires** this device to manage VFIO PCI assignments through libvirt.

**`backendPolicy: LegacyOnly`** — Selects the legacy IOMMU VFIO backend (`/dev/vfio/<iommu-group>`). The alternative, `PreferIommuFD`, uses the IOMMUFD backend (`/dev/iommu` and `/dev/vfio/devices/vfio*`) when available on the host.

When KubeVirt adds IOMMUFD support, `backendPolicy` can be changed to `PreferIommuFD`. Until then, use `LegacyOnly`.
