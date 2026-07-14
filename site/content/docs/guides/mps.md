---
title: MPS
linkTitle: MPS
weight: 45
description: Share a single GPU between multiple containers using NVIDIA Multi-Process Service (MPS).
---

NVIDIA Multi-Process Service (MPS) lets multiple containers run their CUDA work
on one physical GPU *concurrently*, rather than in turns. An MPS control daemon
funnels the work from each client into the GPU at the same time, and you can cap
how much of the GPU each client may use through an active-thread percentage and a
pinned-memory limit.

Use MPS when you want several cooperating workloads to make forward progress on
the same GPU at once and you want coarse-grained control over how they share
compute and memory. If you instead need workloads to take turns on an idle GPU
with no configuration, see [Time-slicing](time-slicing.md).

## Feature status

MPS is controlled by the `MPSSupport` feature gate, an Alpha feature that is
disabled by default.

| Feature gate | Stage | Default |
|---|---|---|
| `MPSSupport` | Alpha | `false` |

See the [Feature gates reference](../reference/feature-gates.md) for all
available gates and their constraints.

## Prerequisites

- The DRA Driver for NVIDIA GPUs must be installed. See [Installation](../install.md).
- The `MPSSupport` feature gate must be enabled. See [Enabling the feature](#enabling-the-feature).
- `MPSSupport` cannot be enabled at the same time as `DynamicMIG` or
  `NVMLDeviceHealthCheck`. These combinations are mutually exclusive.
- To use multi-user mode (`multiUser: true`), the GPUs must be Volta
  architecture or newer.

## Enabling the feature

Enable the `MPSSupport` feature gate with `helm upgrade`:

```bash
helm upgrade dra-driver-nvidia-gpu oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
  --namespace dra-driver-nvidia-gpu \
  --set featureGates.MPSSupport=true
```

The GPU kubelet plugin and webhook must both restart for the change to take
effect. The rolling update happens automatically when you upgrade the Helm
release.

## How MPS works

When a pod that references an MPS-configured claim is scheduled, the GPU kubelet
plugin:

1. Sets the compute mode of the allocated GPU to `EXCLUSIVE_PROCESS`.
2. Starts a dedicated MPS control daemon for the claim — a Deployment named
   `mps-control-daemon-<id>` in the DRA Driver's namespace, running
   `nvidia-cuda-mps-control` on the same node as the workload.
3. Applies the settings from `mpsConfig` (active-thread percentage and
   pinned-memory limits) to that daemon.
4. Injects the MPS pipe and shared-memory directories into each container that
   shares the claim, so their CUDA processes connect through the daemon.

One control daemon is created per unique set of devices in a claim, and it is
removed when the claim is released.

{{% alert title="Note" %}}
Because MPS sets the GPU compute mode to `EXCLUSIVE_PROCESS` and time-slicing
requires `DEFAULT`, MPS and time-slicing cannot be active on the *same* physical
GPU at the same time. Both strategies can be used elsewhere in the same cluster.
{{% /alert %}}

## MPS configuration reference

MPS settings are provided under `sharing.mpsConfig` in a `GpuConfig`:

| Field | Type | Description |
|---|---|---|
| `defaultActiveThreadPercentage` | integer (0–100) | Portion of the GPU's threads made available to each MPS client. |
| `defaultPinnedDeviceMemoryLimit` | quantity (e.g. `10Gi`) | Pinned device-memory limit applied to every device in the claim. Must be at least 1 MB. |
| `defaultPerDevicePinnedMemoryLimit` | map of device index or UUID to quantity | Per-device pinned-memory limit that overrides `defaultPinnedDeviceMemoryLimit` for the listed devices. |
| `multiUser` | boolean | Runs the control daemon in multi-user mode so processes from different UIDs can share it. Defaults to disabled. Requires Volta or newer. |

All fields are optional. If `strategy: MPS` is set with no `mpsConfig`, the
driver starts a control daemon with default settings.

## MPS example

This example shares one GPU between two containers in the same pod, capping each
MPS client at 50% of the GPU's threads and 10 GiB of pinned device memory.

### Create a ResourceClaimTemplate

A `ResourceClaimTemplate` defines the GPU request and its MPS configuration.
Multiple pods can reuse the same template: Kubernetes automatically creates one
`ResourceClaim` per pod from it, and deletes that claim when the pod terminates.

1. Create a file called `mps-gpu.yaml`:

   ```yaml
   apiVersion: resource.k8s.io/v1
   kind: ResourceClaimTemplate
   metadata:
     namespace: mps-example
     name: shared-gpu
   spec:
     spec:
       devices:
         requests:
         - name: mps-gpu
           exactly:
             deviceClassName: gpu.nvidia.com
         config:
         - requests: ["mps-gpu"]
           opaque:
             driver: gpu.nvidia.com
             parameters:
               apiVersion: resource.nvidia.com/v1beta1
               kind: GpuConfig
               sharing:
                 strategy: MPS
                 mpsConfig:
                   defaultActiveThreadPercentage: 50
                   defaultPinnedDeviceMemoryLimit: 10Gi
   ```

   The `deviceClassName: gpu.nvidia.com` is required — it selects a full GPU.
   See [MPS configuration reference](#mps-configuration-reference) for the
   available `mpsConfig` fields.

2. Apply the manifest:

   ```bash
   kubectl create namespace mps-example
   kubectl apply -f mps-gpu.yaml
   ```

   Example output:

   ```
   resourceclaimtemplate.resource.k8s.io/shared-gpu created
   ```

### Create a Pod that references the ResourceClaimTemplate

Reference the `ResourceClaimTemplate` by name in `pod.spec.resourceClaims`.
Kubernetes creates one `ResourceClaim` per pod when it is scheduled.

To share a single GPU across containers in the same pod, each container
references the **same request name** (`request: mps-gpu`) within the claim. If
containers referenced different request names, each would receive a separate
GPU.

1. Create a file called `mps-pod.yaml`:

   ```yaml
   apiVersion: v1
   kind: Pod
   metadata:
     namespace: mps-example
     name: mps-pod
   spec:
     containers:
     - name: mps-ctr0
       image: <your-image>
       resources:
         claims:
         - name: shared-gpu
           request: mps-gpu
     - name: mps-ctr1
       image: <your-image>
       resources:
         claims:
         - name: shared-gpu
           request: mps-gpu
     resourceClaims:
     - name: shared-gpu
       resourceClaimTemplateName: shared-gpu
     tolerations:
     - key: "nvidia.com/gpu"
       operator: "Exists"
       effect: "NoSchedule"
   ```

   Key fields:

   - **`image`** — replace `<your-image>` with your workload container image.
   - **`resourceClaimTemplateName`** — must match the name of the
     `ResourceClaimTemplate` you created in the previous step.
   - **`request: mps-gpu`** — must match the request name defined in the
     template. Both containers using the same value is what causes them to share
     one GPU through MPS.
   - **Toleration** — allows the pod to schedule on nodes that have the
     `nvidia.com/gpu: NoSchedule` taint, which is common on GPU nodes. Remove it
     if your cluster does not use this taint.

2. Apply the manifest:

   ```bash
   kubectl apply -f mps-pod.yaml
   ```

   Example output:

   ```
   pod/mps-pod created
   ```

## Verify MPS is active

1. Confirm the pod is running and both containers are ready:

   ```bash
   kubectl get pod -n mps-example mps-pod
   ```

   Example output:

   ```
   NAME      READY   STATUS    RESTARTS   AGE
   mps-pod   2/2     Running   0          30s
   ```

2. Confirm both containers see the same GPU:

   ```bash
   kubectl exec -n mps-example mps-pod -c mps-ctr0 -- nvidia-smi -L
   kubectl exec -n mps-example mps-pod -c mps-ctr1 -- nvidia-smi -L
   ```

   Both commands return the same GPU UUID, confirming the containers share one
   device.

3. Confirm the MPS control daemon is running in the DRA Driver's namespace:

   ```bash
   kubectl get deployment -n dra-driver-nvidia-gpu -l app
   ```

   You should see a Deployment named `mps-control-daemon-<id>` for the claim.

4. On the GPU node, run `nvidia-smi` on the host. Processes routed through MPS
   appear with the `M+C` (MPS + Compute) process type, rather than the plain `C`
   (Compute) type used by non-MPS processes.


## Limitations and considerations

- Isolation between clients is limited to the
  configured active-thread percentage and pinned-memory limits. There are no
  hard throughput guarantees.
- MPS requires the
  `EXCLUSIVE_PROCESS` compute mode; time-slicing requires `DEFAULT`.
- `MPSSupport` cannot be enabled together
  with `DynamicMIG` or `NVMLDeviceHealthCheck`.
- Setting `multiUser: true`
  requires GPUs of Volta architecture or newer; otherwise the claim fails to
  prepare.
