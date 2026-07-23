---
title: ResourceSlice device attributes
linkTitle: ResourceSlice attributes
weight: 40
description: NVIDIA GPU, MIG, and VFIO device attributes and capacity published by the DRA Driver for NVIDIA GPUs in ResourceSlices.
---

The DRA Driver for NVIDIA GPUs publishes each allocatable device — full GPUs, MIG
slices, and VFIO passthrough devices — as an entry under `spec.devices` in a
node's `ResourceSlice`. This page is a reference for the NVIDIA-specific
attributes and capacity on those entries: what each field means and the exact
names you reference in CEL selectors.

For the generic `ResourceSlice` type schema — every field on `ResourceSlice`,
`Device`, `DeviceAttribute`, `DeviceCapacity`, and related types — see the
[Kubernetes API reference for ResourceSlice v1](https://kubernetes.io/docs/reference/kubernetes-api/resource/resource-slice-v1/).
For how the driver publishes slices, see
[Publishing GPUs in ResourceSlices](../concepts/gpu-allocation.md#publishing-gpus-in-resourceslices).
To inspect the ResourceSlices on your own cluster, see
[View available GPU resources](../guides/gpu-allocation/view-resources.md).

## Device types

The DRA Driver for NVIDIA GPUs publishes all devices under a single driver,
`gpu.nvidia.com`, with one pool per node. The NVIDIA-specific `type` attribute on
each device identifies the kind of device, and the built-in DeviceClasses select
on it:

| `type` | DeviceClass | Device |
|---|---|---|
| `gpu` | `gpu.nvidia.com` | Full physical GPU |
| `mig` | `mig.nvidia.com` | MIG slice |
| `vfio` | `vfio.gpu.nvidia.com` | VFIO passthrough device |

The sections below show a representative `spec.devices[]` entry for each type.
`kubectl` prints map keys alphabetically (so `type` and `uuid` appear last), the
`#` comments are annotations rather than part of the real output, and the values
are illustrative — confirm them on your own cluster.

## Full GPU (type: gpu)

```yaml
- attributes:
    addressingMode:
      string: HMM                   # memory addressing mode, when available
    architecture:
      string: Ampere                # GPU architecture
    brand:
      string: Nvidia                # GPU brand
    cudaComputeCapability:
      version: 8.0.0                # CUDA compute capability
    cudaDriverVersion:
      version: 13.0.0              # CUDA driver version
    driverVersion:
      version: 580.126.20          # NVIDIA driver version
    productName:
      string: NVIDIA A100-PCIE-40GB # product name reported by NVML
    resource.kubernetes.io/pciBusID:
      string: 0000:65:00.0          # PCI bus address in BDF notation, when available
    resource.kubernetes.io/pcieRoot:
      string: pci0000:64            # PCIe root complex identifier, when available
    resource.kubernetes.io/numaNode:
      int: 0                         # NUMA node, when available
    type:
      string: gpu                   # device kind: gpu, mig, or vfio
    uuid:
      string: GPU-2fa81118-5a5f-aa66-7660-471eed407181
  capacity:
    memory:
      value: 40Gi                   # total GPU memory
    # On MIG-capable GPUs with partition metadata, additional capacities
    # (multiprocessors, copyEngines, decoders, encoders, jpegEngines, ofaEngines)
    # may also appear here.
  name: gpu-0
```

## MIG slice (type: mig)

```yaml
- attributes:
    addressingMode:
      string: HMM
    architecture:
      string: Ampere                # inherited from the parent GPU
    brand:
      string: Nvidia
    cudaComputeCapability:
      version: 8.0.0
    cudaDriverVersion:
      version: 13.0.0
    driverVersion:
      version: 580.126.20
    parentUUID:
      string: GPU-2fa81118-5a5f-aa66-7660-471eed407181  # physical GPU hosting this instance
    productName:
      string: NVIDIA A100-PCIE-40GB # inherited from the parent GPU
    profile:
      string: 1g.5gb                # MIG profile, e.g. 1g.5gb or 3g.20gb
    resource.kubernetes.io/pciBusID:
      string: 0000:65:00.0
    resource.kubernetes.io/pcieRoot:
      string: pci0000:64
    resource.kubernetes.io/numaNode:
      int: 0                         # inherited from the parent GPU, when available
    type:
      string: mig                   # device kind
    uuid:
      string: MIG-1a2b3c4d-5e6f-7a8b-9c0d-1e2f3a4b5c6d
  capacity:
    copyEngines:
      value: "1"                    # dedicated copy engines
    decoders:
      value: "0"                    # dedicated video decoders
    encoders:
      value: "0"                    # dedicated video encoders
    jpegEngines:
      value: "0"                    # dedicated JPEG engines
    memory:
      value: 4864Mi                 # dedicated slice memory (usable amount, below the "5gb" label)
    multiprocessors:
      value: "14"                   # streaming multiprocessors dedicated to the slice
    ofaEngines:
      value: "0"                    # dedicated optical-flow accelerators
  name: gpu-0-mig-1g.5gb-0
```

## VFIO passthrough (type: vfio)

```yaml
- attributes:
    deviceID:
      string: "0x20b0"              # PCI device ID
    gpuModuleId:
      int: 1                         # Fabric Manager GPU module ID, when enabled
    iommuFDEnabled:
      bool: true                    # whether the IOMMUFD backend is enabled
    partition1:
      int: 8                         # ID of the size-1 Fabric Manager partition
    partition2:
      int: 4                         # ID of the size-2 Fabric Manager partition
    partition4:
      int: 2                         # ID of the size-4 Fabric Manager partition
    partition8:
      int: 1                         # ID of the size-8 Fabric Manager partition
    productName:
      string: NVIDIA A100-PCIE-40GB # product name reported by NVML
    resource.kubernetes.io/numaNode:
      int: 0                         # NUMA node, when available
    resource.kubernetes.io/pciBusID:
      string: 0000:65:00.0          # PCI bus address in BDF notation, when available
    resource.kubernetes.io/pcieRoot:
      string: pci0000:64            # PCIe root complex identifier, when available
    type:
      string: vfio                  # device kind
    uuid:
      string: GPU-2fa81118-5a5f-aa66-7660-471eed407181
    vendorID:
      string: "0x10de"             # PCI vendor ID (0x10de = NVIDIA)
  capacity:
    addressableMemory:
      value: 40Gi                   # addressable device memory
  name: gpu-vfio-0
```

## NUMA locality

The GPU kubelet plugin publishes the standard `resource.kubernetes.io/numaNode` attribute for full GPUs, MIG devices, and VFIO devices when the PCI NUMA node is available and non-negative.
By default, the attribute uses the scalar `int` form shown in the examples.
When you enable both the driver and Kubernetes `DRAListTypeAttributes` feature gates, the attribute uses a one-element `ints` list:

```yaml
resource.kubernetes.io/numaNode:
  ints:
  - 0
```

The GPU kubelet plugin omits the attribute when it cannot determine locality.
Use `resource.kubernetes.io/numaNode` in a `matchAttribute` constraint when devices in a multi-device claim must have compatible NUMA locality.

## Fabric Manager partition attributes

When you enable `FabricManagerPartitioning`, the GPU kubelet plugin publishes Fabric Manager attributes on VFIO devices when Fabric Manager reports the corresponding data.

| Attribute | Meaning |
|---|---|
| `gpuModuleId` | Physical GPU module identifier reported by NVML and used by Fabric Manager. |
| `partition1` | Fabric Manager partition ID for the one-GPU partition that contains this GPU. |
| `partition2` | Fabric Manager partition ID for the two-GPU partition that contains this GPU. |
| `partition4` | Fabric Manager partition ID for the four-GPU partition that contains this GPU. |
| `partition8` | Fabric Manager partition ID for the eight-GPU partition that contains this GPU. |

The GPU kubelet plugin emits each `partitionN` attribute only when Fabric Manager reports a partition of that size containing the GPU.
To request two VFIO GPUs from the same two-GPU Fabric Manager partition, add this constraint to the claim:

```yaml
constraints:
- requests:
  - gpus
  matchAttribute: gpu.nvidia.com/partition2
```

You can also use a CEL selector for a known node-local module identifier:

```text
device.attributes['gpu.nvidia.com'].gpuModuleId == 1
```

Partition IDs and module IDs describe the local Fabric Manager topology, so use a `matchAttribute` constraint when you need portable co-placement instead of selecting a hardcoded partition ID.
See [`FabricManagerPartitioning`](feature-gates.md) for the gate and its prerequisites.

## Attribute naming: bare keys vs CEL domain

The same attribute has two naming forms. In the serialized `ResourceSlice`,
driver attributes appear as **bare keys** (`type`, `productName`, and so on)
because their domain is implied by the driver name. In a **CEL selector**, you
address them through that domain, `device.attributes['gpu.nvidia.com'].type`. The
standardized PCI attributes are the exception: they are stored fully qualified as
`resource.kubernetes.io/pciBusID`, `resource.kubernetes.io/pcieRoot`, and
`resource.kubernetes.io/numaNode`.

In selectors, attributes are read with
`device.attributes['gpu.nvidia.com'].<name>` and capacity with
`device.capacity['gpu.nvidia.com'].<name>`. For example, to match a GPU with more
than 40 GiB of memory:

```
device.capacity['gpu.nvidia.com'].memory.isGreaterThan(quantity("40Gi"))
```

For full selector examples, see
[Request full GPUs](../guides/gpu-allocation/allocating-gpus.md#select-a-gpu-by-product-name).
CEL-based device selection is a standard Kubernetes DRA feature; see the
[Kubernetes DRA documentation](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
for the complete selector syntax.
