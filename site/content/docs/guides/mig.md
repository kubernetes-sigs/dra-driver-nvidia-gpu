---
title: MIG (Multi-Instance GPU)
linkTitle: MIG
weight: 60
description: Allocate MIG slices to containers using the DRA Driver for NVIDIA GPUs.
---

MIG lets you partition a single NVIDIA GPU into multiple isolated GPU instances,
each with a fixed allocation of compute resources, memory, and memory bandwidth.
Unlike time-slicing, MIG instances have hardware-level isolation. Each
instance's memory and compute are fully separate.

Use MIG when you need predictable, isolated GPU resources for multiple concurrent
workloads on the same physical GPU.

## Feature status

MIG support has two modes:

- Static MIG: the node administrator pre-configures MIG partitions using
  `mig-parted` or `nvidia-smi`. The driver discovers existing partitions and
  exposes them as allocatable devices. No feature gate is required.
- Dynamic MIG: the driver creates and destroys partitions automatically
  based on workload requests. Requires the `DynamicMIG` feature gate (Alpha,
  disabled by default).

| Feature gate | Default | Stage |
|---|---|---|
| `DynamicMIG` | `false` | Alpha |

See the feature gates reference for all available gates.

## Prerequisites

- DRA Driver for NVIDIA GPUs must be installed. See [Installation](../install.md).
- The GPU must support MIG. MIG is available on NVIDIA data center GPUs with the
  Ampere architecture or newer. For the list of supported GPUs, see the
  [NVIDIA MIG User Guide](https://docs.nvidia.com/datacenter/tesla/mig-user-guide/#supported-gpus).
- For static MIG, the MIG partitions must be configured on the node before the
  driver starts.
- For dynamic MIG, the `DynamicMIG` feature gate must be enabled. See
  [Enabling Dynamic MIG](#enabling-dynamic-mig).
- For dynamic MIG on Kubernetes 1.34–1.35, the `DRAPartitionableDevices`
  Kubernetes feature gate must also be enabled on the kube-apiserver and
  kube-scheduler. It is enabled by default on Kubernetes 1.36 and later.

## How MIG devices appear in DRA

The driver exposes each MIG instance as a separate device under the
`mig.nvidia.com` DeviceClass. Devices have:

- `deviceClassName`: `mig.nvidia.com`
- `type` attribute (`gpu.nvidia.com/type`): `mig`
- `profile` attribute (`gpu.nvidia.com/profile`): the MIG profile string,
  for example `1g.5gb` or `3g.20gb`
- `parentUUID` attribute (`gpu.nvidia.com/parentUUID`): the UUID of the
  physical GPU hosting this instance
- Capacity (`gpu.nvidia.com/`): `memory`, `multiprocessors`, `copyEngines`,
  `decoders`, `encoders`, `jpegEngines`, `ofaEngines`

## MIG example

### Request any MIG device

To request any available MIG device without constraining the profile, use the
`mig.nvidia.com` DeviceClass with no selectors.

1. Create a file called `any-mig.yaml`:

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

2. Apply the manifest:

   ```bash
   kubectl apply -f any-mig.yaml
   ```

   Example output:

   ```
   resourceclaimtemplate.resource.k8s.io/any-mig created
   ```

### Select a MIG profile by name

Use a CEL selector to request a specific profile. Profile strings are advertised
by the driver and vary by GPU model.

1. Create a file called `mig-profile.yaml`:

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

2. Apply the manifest:

   ```bash
   kubectl apply -f mig-profile.yaml
   ```

   Example output:

   ```
   resourceclaimtemplate.resource.k8s.io/mig-profile created
   ```

### Select by capacity

To match any MIG device with sufficient resources without pinning to a profile
name, select by capacity:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  namespace: mig-example
  name: mig-by-capacity
spec:
  spec:
    devices:
      requests:
      - name: mig
        exactly:
          deviceClassName: mig.nvidia.com
          selectors:
          - cel:
              expression: |
                device.capacity['gpu.nvidia.com'].memory.isGreaterThan(quantity("10Gi"))
                &&
                device.capacity['gpu.nvidia.com'].multiprocessors.isGreaterThan(quantity("10"))
```

### Request multiple MIG devices from the same GPU

To ensure that multiple MIG devices in a single claim come from the same
physical GPU, add a `constraints` block with
`matchAttribute: "gpu.nvidia.com/parentUUID"`. Without this constraint, the
scheduler may allocate devices from different physical GPUs.

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

`requests: []` — an empty list means the constraint applies to all requests in
the claim.

### Create a Pod that uses a MIG device

Reference the `ResourceClaimTemplate` by name in `pod.spec.resourceClaims`.
Kubernetes creates one `ResourceClaim` per pod when it is scheduled.

1. Create a file called `mig-pod.yaml` (using the `any-mig` template from the
   first example):

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

2. Apply the manifest:

   ```bash
   kubectl apply -f mig-pod.yaml
   ```

3. Verify the pod is running:

   ```bash
   kubectl get pod -n mig-example mig-pod
   ```

4. Confirm the MIG device was allocated:

   ```bash
   kubectl exec -n mig-example mig-pod -c workload -- nvidia-smi -L
   ```

   Example output:

   ```
   MIG 1g.5gb Device 0: (UUID: MIG-...)
   ```

For additional examples, see the
[`demo/specs/`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/tree/main/demo/specs/)
directory in the repository.

## Enabling Dynamic MIG

Enable `DynamicMIG` with `helm upgrade`:

```bash
helm upgrade dra-driver-nvidia-gpu oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
  --namespace dra-driver-nvidia-gpu \
  --set featureGates.DynamicMIG=true
```

The GPU kubelet plugin must restart for the change to take effect. The rolling
update happens automatically when you upgrade the Helm release.

Dynamic MIG depends on the Kubernetes partitionable devices feature (KEP-4815).
On Kubernetes 1.34–1.35, you must also enable the `DRAPartitionableDevices`
feature gate on the kube-apiserver and kube-scheduler.
This feature gate is required to allow the scheduler
to allocate dynamically created MIG devices. On Kubernetes 1.36 and later,
`DRAPartitionableDevices` is enabled by default and no action is required.

{{< alert >}}
`DynamicMIG` is mutually exclusive with `PassthroughSupport`,
`NVMLDeviceHealthCheck`, and `MPSSupport`. The driver returns a validation
error at startup if any of these are enabled together.
{{< /alert >}}

## Limitations and considerations

- Hardware support. MIG is available only on NVIDIA data center GPUs with the
  Ampere architecture or newer. For the list of supported GPUs, see the
  [NVIDIA MIG User Guide](https://docs.nvidia.com/datacenter/tesla/mig-user-guide/#supported-gpus).
- TimeSlicing has no effect on MIG devices. Setting
  `strategy: TimeSlicing` in a `MigDeviceConfig` is accepted without error but
  does not affect hardware behavior. To share a MIG slice across containers, use
  Multi-Process Service (MPS) instead.
- MPS on MIG requires the `MPSSupport` feature gate to use MPS on MIG devices. `MPSSupport` is mutually exclusive with
  `DynamicMIG`.
- Profile availability depends on GPU model. Available MIG profiles differ
  between GPU SKUs. See the
  [NVIDIA MIG User Guide](https://docs.nvidia.com/datacenter/tesla/mig-user-guide/)
  for profiles available on your hardware.
- Static MIG partitions must be configured before the driver starts. In
  static mode, the driver does not modify the node's MIG configuration.
  Partitions added after the driver starts are not discovered until the GPU
  kubelet plugin restarts.
