---
title: MIG (Multi-Instance GPU)
linkTitle: MIG
weight: 45
aliases:
  - /docs/guides/mig/
  - /docs/guides/dynamic-mig/
  - /docs/guides/gpu-allocation/dynamic-mig/
description: Allocate MIG slices to containers using static or dynamic MIG.
---

MIG lets you partition a single NVIDIA GPU into multiple isolated GPU instances,
each with a fixed allocation of compute resources, memory, and memory bandwidth.
Unlike time-slicing, MIG instances have hardware-level isolation. Each
instance's memory and compute are fully separate.

Use MIG when you need predictable, isolated GPU resources for multiple concurrent
workloads on the same physical GPU. For how MIG compares to time-slicing and the
other sharing modes, refer to
[Choosing a resource type](../../concepts/gpu-allocation.md#choosing-a-resource-type).

Each MIG instance is exposed under the `mig.nvidia.com` DeviceClass and carries
the `gpu.nvidia.com/type` attribute value `mig`. Beyond the parent-GPU
attributes shared with full GPUs, MIG devices add a `profile` (for example
`1g.5gb`) and a `parentUUID`, and advertise per-slice capacity such as `memory`
and `multiprocessors`.

For the full list of MIG attributes and capacity you can match with CEL
selectors, refer to
[ResourceSlice device attributes](../../reference/resourceslice-attributes.md#mig-slice-type-mig).

This guide covers static and dynamic MIG setup and workload examples. For how
static and dynamic MIG work in the driver, refer to
[Static and dynamic MIG](../../concepts/gpu-allocation.md#static-and-dynamic-mig)
on the GPU allocation concept page. For how to inspect ResourceSlice
objects, refer to [View available GPU resources](view-resources.md). For
how to write CEL selectors, refer to
[Request full GPUs](allocating-gpus.md#select-a-gpu-by-product-name).

## Dynamic MIG feature status

| Feature gate | Default | Stage |
|---|---|---|
| `DynamicMIG` | `false` | Alpha |

`DynamicMIG` is mutually exclusive with the following feature gates. The driver
returns a validation error at startup if any of these are enabled together:

- `PassthroughSupport`
- `NVMLDeviceHealthCheck`
- `MPSSupport`

Refer to the [feature gates reference](../../reference/feature-gates.md) for all
available gates.

Static MIG support is enabled by default; no feature gate is required.

## Prerequisites

- The DRA Driver for NVIDIA GPUs must be installed. Refer to [Installation](../../install.md).
- One or more GPUs in your cluster must support MIG. MIG is available on NVIDIA
  data center GPUs with the Ampere architecture or newer. For the list of
  supported GPUs, refer to the
  [NVIDIA MIG User Guide](https://docs.nvidia.com/datacenter/tesla/mig-user-guide/#supported-gpus).
- For static MIG, create the MIG partitions on the node before the driver starts.
  Refer to [Configure static MIG](#configure-static-mig).
- For dynamic MIG, enable the `DynamicMIG` feature gate. Refer to
  [Enabling dynamic MIG](#enabling-dynamic-mig).


## Enabling dynamic MIG

Enable the `DynamicMIG` feature gate to use dynamic MIG. For how partition
creation and teardown work, refer to
[How dynamic MIG works](../../concepts/gpu-allocation.md#how-dynamic-mig-works).

Enable `DynamicMIG` with `helm upgrade --install`. This installs the driver if
it is not already present, or upgrades an existing release:

```bash
helm upgrade --install dra-driver-nvidia-gpu oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
  --namespace dra-driver-nvidia-gpu --create-namespace \
  --set featureGates.DynamicMIG=true \
  --set gpuResourcesEnabledOverride=true
```

The GPU kubelet plugin must restart for the change to take effect. The rolling
update happens automatically when you upgrade the Helm release.

Dynamic MIG depends on the Kubernetes partitionable devices feature (KEP-4815).
On Kubernetes v1.34–v1.35, you must also enable the `DRAPartitionableDevices`
feature gate on the kube-apiserver and kube-scheduler. This feature gate is
required to allow the scheduler to allocate dynamically created MIG devices. On
Kubernetes v1.36 and later, `DRAPartitionableDevices` is enabled by default and
no action is required.

## Limitations and considerations

- MIG is available only on NVIDIA data center GPUs with the Ampere architecture
  or newer. Available profiles depend on the GPU model. For supported GPUs and
  profiles, refer to the
  [NVIDIA MIG User Guide](https://docs.nvidia.com/datacenter/tesla/mig-user-guide/#supported-gpus).
- TimeSlicing has no effect on MIG devices. Setting `strategy: TimeSlicing` in a
  `MigDeviceConfig` does not affect hardware behavior. To share a MIG slice
  across containers, use Multi-Process Service (MPS) instead with the
  `MPSSupport` feature gate. Note that `MPSSupport` is mutually exclusive with
  `DynamicMIG`; they cannot be used together.
- Static MIG partitions must be configured before the driver starts. In static
  mode, the driver does not modify the node's MIG configuration. Partitions added
  after the driver starts are not discovered until the GPU kubelet plugin
  restarts.
- Dynamic MIG replaces static discovery. On a MIG-capable node, the driver
  manages partitions itself and does not use pre-created static partitions.
  MIG partitions the driver did not create are destroyed when the GPU kubelet
  plugin starts, so do not enable `DynamicMIG` on nodes with partitions you want
  to keep.
- On dynamic MIG nodes, let the driver
  create and destroy partitions through workload requests. Do not run
  `mig-parted` or `nvidia-smi mig` while the GPU kubelet plugin is running, because
  manual changes can conflict with the driver's partition state and cause pod
  preparation or cleanup to fail.
- On Ampere GPUs (for example, A100), enable MIG mode on the GPU before the
  DRA Driver starts. Ampere cannot toggle MIG mode without a GPU reset. If MIG
  mode is off, the driver falls back to full-GPU allocation and advertises no
  MIG partitions. Hopper and later architectures enable MIG mode on demand.
- Dynamic MIG is not supported on vGPU guests.
- NVIDIA DCGM Exporter cannot attribute MIG metrics to pods on dynamic MIG nodes.
  With `KUBERNETES_ENABLE_DRA=true`, the exporter identifies pods by the MIG
  instance UUID, but dynamic MIG advertises partitions that do not exist yet, so
  their `ResourceSlice` entries have no `uuid` attribute and the UUID created
  during pod preparation is never published.
  The exporter drops those mappings and reports MIG metrics without pod labels.

## Examples

The following examples work with static or dynamic MIG.

### Create the example namespace

The examples use the `mig-example` namespace. Create it first:

```bash
kubectl create namespace mig-example
```

### Request any MIG device

To request any available MIG device without constraining the profile, use the
`mig.nvidia.com` DeviceClass with no selectors.

1. Create the `ResourceClaimTemplate`:

   ```yaml
   apiVersion: resource.k8s.io/v1
   kind: ResourceClaimTemplate
   metadata:
     namespace: mig-example
     name: any-mig
   spec:
     spec:
       devices:
         requests:
         - name: mig
           exactly:
             deviceClassName: mig.nvidia.com
   ```

   Save it as `any-mig.yaml`.

2. Apply the manifest:

   ```bash
   kubectl apply -f any-mig.yaml
   ```

3. Create a pod that references the template:

   ```yaml
   apiVersion: v1
   kind: Pod
   metadata:
     namespace: mig-example
     name: mig-pod
   spec:
     containers:
     - name: workload
       image: ubuntu:22.04
       command: ["bash", "-c"]
       args: ["nvidia-smi -L; sleep 9999"]
       resources:
         claims:
         - name: mig
     resourceClaims:
     - name: mig
       resourceClaimTemplateName: any-mig
     tolerations:
     - key: "nvidia.com/gpu"
       operator: "Exists"
       effect: "NoSchedule"
   ```

   Save it as `mig-pod.yaml`. `ubuntu:22.04` doesn't include `nvidia-smi`;
   the DRA Driver's CDI integration injects `nvidia-smi` and the host driver's
   libraries into the container at start, so a plain OS image is sufficient
   for verification.

4. Apply the manifest:

   ```bash
   kubectl apply -f mig-pod.yaml
   ```

5. Verify that the pod received a MIG device:

   ```bash
   kubectl get pod -n mig-example mig-pod
   kubectl exec -n mig-example mig-pod -c workload -- nvidia-smi -L
   ```

### Select a MIG profile

Use a CEL selector to request a specific profile. Profile strings are advertised
by the driver and vary by GPU model. For CEL selector syntax, refer to
[Request full GPUs](allocating-gpus.md#select-a-gpu-by-product-name).

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  namespace: mig-example
  name: mig-profile
spec:
  spec:
    devices:
      requests:
      - name: mig
        exactly:
          deviceClassName: mig.nvidia.com
          selectors:
          - cel:
              expression: "device.attributes['gpu.nvidia.com'].profile == '1g.5gb'"
```

Save it as `mig-profile.yaml` and apply with `kubectl apply -f mig-profile.yaml`.
Reference it from a pod with `resourceClaimTemplateName: mig-profile`, following
the pod pattern in [Request any MIG device](#request-any-mig-device).

### Request multiple MIG devices from the same GPU

To ensure that multiple MIG devices in a single claim come from the same
physical GPU, add a `constraints` block with
`matchAttribute: "gpu.nvidia.com/parentUUID"`:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  namespace: mig-example
  name: multi-mig
spec:
  spec:
    devices:
      requests:
      - name: mig-small
        exactly:
          deviceClassName: mig.nvidia.com
          selectors:
          - cel:
              expression: "device.attributes['gpu.nvidia.com'].profile == '1g.5gb'"
      - name: mig-medium
        exactly:
          deviceClassName: mig.nvidia.com
          selectors:
          - cel:
              expression: "device.attributes['gpu.nvidia.com'].profile == '2g.10gb'"
      constraints:
      - requests: []
        matchAttribute: "gpu.nvidia.com/parentUUID"
```

Save it as `multi-mig.yaml` and apply with `kubectl apply -f multi-mig.yaml`.
Reference it from a pod with `resourceClaimTemplateName: multi-mig`, following
the pod pattern in [Request any MIG device](#request-any-mig-device).

For additional examples, refer to the
[`demo/specs/`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/tree/{{< param driver_release_tag >}}/demo/specs/)
directory in the repository.

