---
title: Fabric Manager partitioning
linkTitle: Fabric Manager partitioning
weight: 48
description: Constrain full-GPU and VFIO claims to partitions managed by NVIDIA Fabric Manager.
---

Fabric Manager partitioning enables the DRA Driver for NVIDIA GPUs to activate
and deactivate NVSwitch fabric partitions for workloads on supported HGX and
single-node NVL systems.
It is an optional topology layer for full-GPU and VFIO allocations, not a
separate GPU resource type.

## Feature status

Choose a base resource type first:

| Base resource | Fabric Manager partitioning | Required feature gates |
|---|---|---|
| Full GPU (`gpu.nvidia.com`) | Supported | `FabricManagerPartitioning` |
| VFIO passthrough (`vfio.gpu.nvidia.com`) | Supported | `FabricManagerPartitioning`, `PassthroughSupport` |
| MIG slice (`mig.nvidia.com`) | Not supported | — |

`FabricManagerPartitioning` does not require `PassthroughSupport`.
Enable `PassthroughSupport` only when you also need VFIO devices.
For all other feature-gate dependencies and incompatibilities, refer to
[Feature gate constraints](../../reference/feature-gates.md#constraints).

## How partition matching works

Fabric Manager reports the valid groups of physical GPUs that it can manage as
partitions.
For example, it might report these two-GPU partitions:

| Partition | Physical GPUs |
|---|---|
| 4 | GPU modules 1 and 2 |
| 5 | GPU modules 3 and 4 |

The driver publishes the partition membership as device attributes.
Both GPUs in partition 4 receive `partition2: 4`, and both GPUs in partition 5
receive `partition2: 5`.

When kubelet prepares a claim, the driver collects all full-GPU and VFIO
physical GPUs allocated to that claim.
Their complete set must exactly match one partition reported by Fabric Manager.
A subset of a partition, a set that crosses partition boundaries, or a set that
combines multiple partitions fails Prepare and prevents the pod from starting.

For the example topology, the following allocations succeed or fail as shown:

| Allocated GPU modules | Result |
|---|---|
| 1 and 2 | Succeeds: exactly matches partition 4 |
| 3 and 4 | Succeeds: exactly matches partition 5 |
| 1 and 3 | Fails: crosses partition boundaries |
| 1 only | Succeeds only if Fabric Manager also reports a one-GPU partition containing GPU module 1 |

Use one `ResourceClaim` for one Fabric Manager partition.
Separate claims cannot use a `matchAttribute` constraint to ensure that their
combined GPUs form one partition.

## Prerequisites

Before enabling the feature:

- Meet the general driver [prerequisites](../../prerequisites.md).
- Use a supported HGX or single-node NVL system with an NVSwitch-managed
  fabric and a supported partition topology.
- Run NVIDIA Fabric Manager on each participating node with `FABRIC_MODE=1`,
  `FM_CMD_UNIX_SOCKET_PATH=/run/nvidia-fabricmanager/socket`, and
  `FABRIC_MODE_RESTART=1`.
- Ensure the target GPUs are visible to NVML when the GPU kubelet plugin
  starts, so that the driver can resolve their `gpuModuleID` values.

The driver verifies that it can find the Fabric Manager client library,
connect to Fabric Manager, and read the reported topology.
The driver cannot verify that the platform is in the supported-product list or
identify how Fabric Manager was started.
Verify the platform support and the environment variables configuration before enabling
the feature.

Migrate claims before enabling this feature.
On a participating Fabric Manager node, the gate affects every full-GPU claim
and, when `PassthroughSupport` is enabled, every VFIO claim.
Audit existing claim templates first.
A request for `count: N` without a matching `gpu.nvidia.com/partitionN`
constraint can receive an invalid GPU combination and then fail Prepare.

If unconstrained claims must continue to run, schedule them on other nodes
until their claim templates are partition-aware.

## Enable Fabric Manager partitioning

Enable the gate on the existing Helm release:

```bash
helm upgrade -i dra-driver-nvidia-gpu oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
    --version {{< param "driver_version" >}} \
    --namespace dra-driver-nvidia-gpu \
    --reuse-values \
    --set featureGates.FabricManagerPartitioning=true
```

For VFIO allocations, also enable `PassthroughSupport` and follow the
[KubeVirt VFIO GPU passthrough guide](kubevirt-vfio-gpu-passthrough.md) for
the remaining VFIO prerequisites:

```bash
helm upgrade -i dra-driver-nvidia-gpu oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
    --version {{< param "driver_version" >}} \
    --namespace dra-driver-nvidia-gpu \
    --reuse-values \
    --set featureGates.FabricManagerPartitioning=true \
    --set featureGates.PassthroughSupport=true
```

On nodes where the driver does not detect an NVSwitch or an NVLink 5
switch-managed fabric, it skips Fabric Manager initialization.

## Inspect the published partitions

Inspect the GPU ResourceSlices before creating a claim:

```bash
kubectl get resourceslice -o yaml
```

When the gate is active and Fabric Manager reports the topology, full-GPU and
VFIO devices include attributes similar to:

```yaml
attributes:
  gpuModuleID:
    int: 1
  partition1:
    int: 8
  partition2:
    int: 4
  partition4:
    int: 2
  partition8:
    int: 1
  type:
    string: gpu
```

`gpuModuleID` is the physical module identifier used by Fabric Manager.
`partitionN` is the ID of the reported N-GPU partition containing that device.
The driver publishes a `partitionN` attribute only when Fabric Manager reports
a partition of that size containing the GPU.
Do not assume that a particular size is available; inspect the ResourceSlice
first.

Partition and module IDs are local topology details.
Prefer `matchAttribute: gpu.nvidia.com/partitionN` over selecting a hardcoded
partition ID.
Refer to
[ResourceSlice device attributes](../../reference/resourceslice-attributes.md#fabric-manager-partition-attributes)
for the attribute reference.

## Request a full-GPU partition

This example requests one complete two-GPU partition:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: fm-full-gpu-partition-2
spec:
  spec:
    devices:
      requests:
      - name: gpus
        exactly:
          deviceClassName: gpu.nvidia.com
          allocationMode: ExactCount
          count: 2
      constraints:
      - requests:
        - gpus
        matchAttribute: gpu.nvidia.com/partition2
```

`matchAttribute` requires the selected GPUs to have the same size-two
partition ID.
Combined with `count: 2`, this selects all GPUs in that reported two-GPU
partition.
A count alone does not guarantee a valid partition.

Reference the two-GPU partition claim for the workload:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: fm-partition-pod
spec:
  containers:
  - name: ctr
    image: nvidia/cuda:12.0.0-base-ubuntu22.04
    command: ["bash", "-c"]
    args: ["nvidia-smi -L; sleep infinity"]
    resources:
      claims:
      - name: gpus
  resourceClaims:
  - name: gpus
    resourceClaimTemplateName: fm-full-gpu-partition-2
```

## Request a VFIO partition

For VFIO, use the same count and constraint with the `vfio.gpu.nvidia.com`
DeviceClass.
Keep all selected VFIO devices in one request and apply one `VfioDeviceConfig`
group to that request:

```yaml
devices:
  config:
  - requests:
    - gpus
    opaque:
      driver: gpu.nvidia.com
      parameters:
        apiVersion: resource.nvidia.com/v1beta1
        kind: VfioDeviceConfig
        iommu:
          backendPolicy: LegacyOnly
          enableAPIDevice: true
  requests:
  - name: gpus
    exactly:
      deviceClassName: vfio.gpu.nvidia.com
      allocationMode: ExactCount
      count: 2
  constraints:
  - requests:
    - gpus
    matchAttribute: gpu.nvidia.com/partition2
```

KubeVirt also requires `DeviceMetadata` and additional host configuration.
Refer to [KubeVirt VFIO GPU passthrough](kubevirt-vfio-gpu-passthrough.md).

## Verify the allocation

After creating the workload, confirm that its generated claim contains exactly
the expected number of devices:

```bash
kubectl get resourceclaim
kubectl get resourceclaim <claim-name> -o yaml
```

Review `status.allocation.devices.results` and compare the selected device
names with the node's ResourceSlice.
They must all share the requested `partitionN` value.

Confirm that the pod reaches `Running`:

```bash
kubectl get pod fm-partition-pod
kubectl exec fm-partition-pod -- nvidia-smi -L
```

For a two-GPU full-GPU claim, `nvidia-smi -L` should list two GPUs.

## Troubleshooting

### View GPU kubelet-plugin logs

Check the GPU kubelet-plugin logs for startup and Prepare errors:

```bash
kubectl logs -n dra-driver-nvidia-gpu \
    -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    -c gpus
```

| Error or symptom | Action |
|---|---|
| `fabric manager library not found` | Verify that `libnvfm.so` is installed under `nvidiaDriverRoot` in a standard library directory. |
| `Fabric Manager could not be opened` | Verify that Fabric Manager is running with `FABRIC_MODE=1` and that its socket or configured address is reachable from the plugin. |
| `GPU module set [...] does not match any FM partition` | Compare the allocated devices with their `partitionN` attributes. Use one claim with the correct `count: N` and `matchAttribute`. |
| `no gpuModuleID` or missing FM attributes | Verify NVML visibility and the reported FM topology. A GPU already bound to `vfio-pci` when the plugin starts might not have a resolvable module ID. |
| No FM attributes on the node | Confirm that the gate is enabled and that the driver detects an NVSwitch or NVLink 5 switch-managed fabric on the node. |

If a VFIO workload remains in `ContainerCreating` after its partition is
selected, continue with the VFIO-specific
[troubleshooting steps](kubevirt-vfio-gpu-passthrough.md#troubleshooting).

### View Fabric Manager partition status

On the host, list all partitions and their status:

```bash
/run/nvidia/fmpm \
  --unix-domain-socket /run/nvidia-fabricmanager/socket \
  -l
```

If a partition is activated, the response includes `isActive: 1`, as in this
example:

```json
{
  "gpuInfo": [
    {
      "maxNumNvLinks": 0,
      "numNvLinksAvailable": 0,
      "nvlinkLineRateMBps": 26562,
      "pciBusId": "00000000:00:00.0",
      "physicalId": 7,
      "uuid": ""
    }
  ],
  "isActive": 1,
  "numGpus": 1,
  "partitionId": 13
}
```
