---
title: Request full GPUs
linkTitle: Request full GPUs
weight: 30
aliases:
  - /docs/guides/allocating-gpus/
  - /docs/guides/cel-selectors/
  - /docs/guides/gpu-allocation/cel-selectors/
description: Request full GPUs with ResourceClaimTemplates and CEL selectors.
---

This guide covers how to request full GPUs (`gpu.nvidia.com`) for workloads, from requesting any available GPU to targeting a specific model or memory size with CEL selectors. 

A `ResourceClaimTemplate` defines the GPU request. You reference it from a pod in
`spec.resourceClaims`. Kubernetes creates one `ResourceClaim` per pod when it is
scheduled. Refer to the [Kubernetes DRA documentation](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/) for more details on the `ResourceClaim` and `ResourceClaimTemplate`.

For an overview of GPU allocation, refer to the [GPU allocation](../../concepts/gpu-allocation.md) concept documentation.

The examples on this page use the `gpu.nvidia.com` DeviceClass. 

## Targeting GPUs

Common Expression Language (CEL) selectors let you target which devices the scheduler can allocate to a
`ResourceClaim` by matching against driver-published attributes and capacity.
Available attributes and capacity values are listed in [ResourceSlice device attributes](../../reference/resourceslice-attributes.md).
Add a `selectors` block under `exactly` in a request to filter the pool of
candidate devices. The following snippets show the `devices` block of a
`ResourceClaimTemplate` spec for a few common requests.

Request only A100s by matching the `productName` attribute:

```yaml
devices:
  requests:
  - name: gpu
    exactly:
      deviceClassName: gpu.nvidia.com
      selectors:
      - cel:
          expression: |
            device.attributes['gpu.nvidia.com'].productName.lowerAscii().matches('^.*a100.*$')
```

Request only GPUs with more than 40 GiB of memory by matching the `memory` capacity:

```yaml
devices:
  requests:
  - name: gpu
    exactly:
      deviceClassName: gpu.nvidia.com
      selectors:
      - cel:
          expression: |
            device.capacity['gpu.nvidia.com'].memory.isGreaterThan(quantity("40Gi"))
```

Use the same CEL selector patterns with other DeviceClasses the driver provides,
such as `mig.nvidia.com`. Refer to the [MIG guide](mig.md) for MIG-specific examples.

Refer to the [Common Expression Language in Kubernetes](https://kubernetes.io/docs/reference/using-api/cel/)
reference for CEL syntax and available functions.

## Prerequisites

- The DRA Driver for NVIDIA GPUs must be installed with GPU allocation enabled.
  Refer to [Installation](../../install.md).
- At least one GPU node in the cluster with allocatable devices published in a
  ResourceSlice.
- Create the `gpu-example` namespace used in the examples on this page.

  ```bash
  kubectl create namespace gpu-example
  ```

## View available GPUs

Before requesting a GPU, confirm that the DRA Driver for NVIDIA GPUs has published
devices on your nodes:

```bash
kubectl get resourceslice
```

Example output:

```
NAME                                         NODE        DRIVER           POOL        AGE
00000-gpu.nvidia.com-node-name-2gdsm         node-name   gpu.nvidia.com   node-name   10m
```

If this list is empty, the driver is not yet advertising devices. Confirm the
driver is installed and running and that at least one node has supported GPUs.

ComputeDomain IMEX channels are published separately under the
`compute-domain.nvidia.com` driver. Refer to
[ComputeDomain workloads](../compute-domain-workloads.md) for details.

For the full device attributes and capacity, including the `productName`,
`architecture`, and `memory` values used in the selector examples below, use
`kubectl get resourceslice -o yaml`. Refer to [View available GPU resources](view-resources.md) for additional details on GPU resources in your cluster.

## Request any GPU

To request any available full GPU, use the `gpu.nvidia.com` DeviceClass with no
selectors.

1. Create a `ResourceClaimTemplate` manifest:

   ```yaml
   apiVersion: resource.k8s.io/v1
   kind: ResourceClaimTemplate
   metadata:
     namespace: gpu-example
     name: single-gpu
   spec:
     spec:
       devices:
         requests:
         - name: gpu
           exactly:
             deviceClassName: gpu.nvidia.com
   ```

   Save it as `single-gpu.yaml`.

2. Apply the manifest:

   ```bash
   kubectl apply -f single-gpu.yaml
   ```

   Example output:

   ```
   resourceclaimtemplate.resource.k8s.io/single-gpu created
   ```

3. Create a pod that references the template:

   ```yaml
   apiVersion: v1
   kind: Pod
   metadata:
     namespace: gpu-example
     name: gpu-pod
   spec:
     containers:
     - name: workload
       image: ubuntu:22.04
       command: ["bash", "-c"]
       args: ["nvidia-smi -L; sleep 9999"]
       resources:
         claims:
         - name: gpu
     resourceClaims:
     - name: gpu
       resourceClaimTemplateName: single-gpu
     tolerations:
     - key: "nvidia.com/gpu"
       operator: "Exists"
       effect: "NoSchedule"
   ```

   Save it as `gpu-pod.yaml`.

4. Apply the manifest:

   ```bash
   kubectl apply -f gpu-pod.yaml
   ```

   Example output:

   ```
   pod/gpu-pod created
   ```

5. Verify that the pod received a GPU. Once the pod is `Running` (`kubectl get pods -n gpu-example`), list the GPUs visible to the container:

   ```bash
   kubectl exec -n gpu-example gpu-pod -- nvidia-smi -L
   ```

   Example output:

   ```
   GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-00000000-0000-0000-0000-000000000000)
   ```

   The UUID output matches the `uuid` attribute of the corresponding device in the
   node's `ResourceSlice`. Refer to [View available GPU resources](view-resources.md).

## Request multiple GPUs in one pod

This example requires at least two allocatable GPUs in your cluster.

{{% alert color="warning" title="Fabric Manager partitions require one claim" %}}
The example below creates two independent single-GPU claims. It does not request
one two-GPU Fabric Manager partition. On a node with
`FabricManagerPartitioning` enabled, use one claim with `count: 2` and
`matchAttribute: gpu.nvidia.com/partition2` when both GPUs must form one
Fabric Manager partition. Each independent single-GPU claim must instead match a
reported single-GPU partition. See
[Fabric Manager partitioning](fabric-manager-partitioning.md#request-a-full-gpu-partition).
{{% /alert %}}

To give each container its own GPU, create separate device requests. This
example reuses the `single-gpu` `ResourceClaimTemplate` from [Request any GPU](#request-any-gpu).

1. Create the pod manifest:

   ```yaml
   apiVersion: v1
   kind: Pod
   metadata:
     namespace: gpu-example
     name: multi-gpu-pod
   spec:
     containers:
     - name: ctr0
       image: ubuntu:22.04
       command: ["bash", "-c"]
       args: ["nvidia-smi -L; sleep 9999"]
       resources:
         claims:
         - name: gpu0
     - name: ctr1
       image: ubuntu:22.04
       command: ["bash", "-c"]
       args: ["nvidia-smi -L; sleep 9999"]
       resources:
         claims:
         - name: gpu1
     resourceClaims:
     - name: gpu0
       resourceClaimTemplateName: single-gpu
     - name: gpu1
       resourceClaimTemplateName: single-gpu
     tolerations:
     - key: "nvidia.com/gpu"
       operator: "Exists"
       effect: "NoSchedule"
   ```

   Save it as `multi-gpu-pod.yaml`.

2. Apply the manifest:

   ```bash
   kubectl apply -f multi-gpu-pod.yaml
   ```

   Example output:

   ```
   pod/multi-gpu-pod created
   ```

3. Verify that each container received a distinct GPU by listing the GPUs visible to each container:

   ```bash
   kubectl exec -n gpu-example multi-gpu-pod -c ctr0 -- nvidia-smi -L
   kubectl exec -n gpu-example multi-gpu-pod -c ctr1 -- nvidia-smi -L
   ```

   Example output:

   ```
   GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-00000000-0000-0000-0000-000000000000)
   GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-11111111-1111-1111-1111-111111111111)
   ```

   Each container prints a different GPU UUID, confirming they were allocated
   separate devices.

## Share a GPU across containers in a pod

Multiple containers in the same pod can reference the same claim. This example
reuses the `single-gpu` `ResourceClaimTemplate` from [Request any GPU](#request-any-gpu).

1. Create the pod manifest:

   ```yaml
   apiVersion: v1
   kind: Pod
   metadata:
     namespace: gpu-example
     name: shared-gpu-pod
   spec:
     containers:
     - name: ctr0
       image: ubuntu:22.04
       command: ["bash", "-c"]
       args: ["nvidia-smi -L; sleep 9999"]
       resources:
         claims:
         - name: gpu
     - name: ctr1
       image: ubuntu:22.04
       command: ["bash", "-c"]
       args: ["nvidia-smi -L; sleep 9999"]
       resources:
         claims:
         - name: gpu
     resourceClaims:
     - name: gpu
       resourceClaimTemplateName: single-gpu
     tolerations:
     - key: "nvidia.com/gpu"
       operator: "Exists"
       effect: "NoSchedule"
   ```

   Save it as `shared-gpu-pod.yaml`.

2. Apply the manifest:

   ```bash
   kubectl apply -f shared-gpu-pod.yaml
   ```

   Example output:

   ```
   pod/shared-gpu-pod created
   ```

3. Verify that both containers see the same GPU:

   ```bash
   kubectl exec -n gpu-example shared-gpu-pod -c ctr0 -- nvidia-smi -L
   kubectl exec -n gpu-example shared-gpu-pod -c ctr1 -- nvidia-smi -L
   ```

   Example output:

   ```
   GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-00000000-0000-0000-0000-000000000000)
   GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-00000000-0000-0000-0000-000000000000)
   ```

   Both containers print the same GPU UUID, confirming they share the same
   physical device.

## Select a GPU by product name

Add a `selectors` block to target a specific GPU model. This example matches
any A100 GPU using a regular expression on the `productName` attribute.

1. Create the `ResourceClaimTemplate` manifest:

   ```yaml
   apiVersion: resource.k8s.io/v1
   kind: ResourceClaimTemplate
   metadata:
     namespace: gpu-example
     name: a100-gpu
   spec:
     spec:
       devices:
         requests:
         - name: gpu
           exactly:
             deviceClassName: gpu.nvidia.com
             selectors:
             - cel:
                 expression: |
                   device.attributes['gpu.nvidia.com'].productName.lowerAscii().matches('^.*a100.*$')
   ```

   Save it as `a100-gpu.yaml`.

2. Apply the manifest:

   ```bash
   kubectl apply -f a100-gpu.yaml
   ```

   Example output:

   ```
   resourceclaimtemplate.resource.k8s.io/a100-gpu created
   ```

3. Create a pod that references the template:

   ```yaml
   apiVersion: v1
   kind: Pod
   metadata:
     namespace: gpu-example
     name: a100-pod
   spec:
     containers:
     - name: workload
       image: ubuntu:22.04
       command: ["bash", "-c"]
       args: ["nvidia-smi -L; sleep 9999"]
       resources:
         claims:
         - name: gpu
     resourceClaims:
     - name: gpu
       resourceClaimTemplateName: a100-gpu
     tolerations:
     - key: "nvidia.com/gpu"
       operator: "Exists"
       effect: "NoSchedule"
   ```

   Save it as `a100-pod.yaml`.

4. Apply the manifest:

   ```bash
   kubectl apply -f a100-pod.yaml
   ```

5. Verify that the pod was allocated an A100:

   ```bash
   kubectl exec -n gpu-example a100-pod -- nvidia-smi -L
   ```

   Example output:

   ```
   GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-00000000-0000-0000-0000-000000000000)
   ```

   The product name includes `A100`, confirming the selector matched.

## Select a GPU by memory size

Match GPUs with sufficient memory without pinning to a specific model. This
is useful when you need, for example, more than 40 GiB to distinguish an A100
80GB from an A100 40GB.

1. Create the `ResourceClaimTemplate` manifest:

   ```yaml
   apiVersion: resource.k8s.io/v1
   kind: ResourceClaimTemplate
   metadata:
     namespace: gpu-example
     name: large-gpu
   spec:
     spec:
       devices:
         requests:
         - name: gpu
           exactly:
             deviceClassName: gpu.nvidia.com
             selectors:
             - cel:
                 expression: |
                   device.capacity['gpu.nvidia.com'].memory.isGreaterThan(quantity("40Gi"))
   ```

   Save it as `large-gpu.yaml`.

2. Apply the manifest:

   ```bash
   kubectl apply -f large-gpu.yaml
   ```

   Example output:

   ```
   resourceclaimtemplate.resource.k8s.io/large-gpu created
   ```

3. Create a pod that references the template:

   ```yaml
   apiVersion: v1
   kind: Pod
   metadata:
     namespace: gpu-example
     name: large-gpu-pod
   spec:
     containers:
     - name: workload
       image: ubuntu:22.04
       command: ["bash", "-c"]
       args: ["nvidia-smi -L; sleep 9999"]
       resources:
         claims:
         - name: gpu
     resourceClaims:
     - name: gpu
       resourceClaimTemplateName: large-gpu
     tolerations:
     - key: "nvidia.com/gpu"
       operator: "Exists"
       effect: "NoSchedule"
   ```

   Save it as `large-gpu-pod.yaml`.

4. Apply the manifest:

   ```bash
   kubectl apply -f large-gpu-pod.yaml
   ```

5. Verify that the pod was allocated a large-memory GPU:

   ```bash
   kubectl exec -n gpu-example large-gpu-pod -- nvidia-smi --query-gpu=memory.total --format=csv
   ```

   Example output:

   ```
   memory.total [MiB]
   81559 MiB
   ```

   The reported memory is greater than 40 GiB (`40 * 1024 = 40960 MiB`),
   confirming the selector matched.

## Combine attribute and capacity selectors

CEL expressions can combine multiple conditions in one selector. This example
requests a Hopper GPU with more than 80 GiB of memory:

1. Create the `ResourceClaimTemplate` manifest:

   ```yaml
   apiVersion: resource.k8s.io/v1
   kind: ResourceClaimTemplate
   metadata:
     namespace: gpu-example
     name: hopper-80g-gpu
   spec:
     spec:
       devices:
         requests:
         - name: gpu
           exactly:
             deviceClassName: gpu.nvidia.com
             selectors:
             - cel:
                 expression: |
                   device.attributes['gpu.nvidia.com'].architecture == 'Hopper'
                   &&
                   device.capacity['gpu.nvidia.com'].memory.isGreaterThan(quantity("80Gi"))
   ```

   Save it as `hopper-80g-gpu.yaml`.

2. Apply the manifest:

   ```bash
   kubectl apply -f hopper-80g-gpu.yaml
   ```

   Example output:

   ```
   resourceclaimtemplate.resource.k8s.io/hopper-80g-gpu created
   ```

3. Create a pod that references the template:

   ```yaml
   apiVersion: v1
   kind: Pod
   metadata:
     namespace: gpu-example
     name: hopper-80g-pod
   spec:
     containers:
     - name: workload
       image: ubuntu:22.04
       command: ["bash", "-c"]
       args: ["nvidia-smi -L; sleep 9999"]
       resources:
         claims:
         - name: gpu
     resourceClaims:
     - name: gpu
       resourceClaimTemplateName: hopper-80g-gpu
     tolerations:
     - key: "nvidia.com/gpu"
       operator: "Exists"
       effect: "NoSchedule"
   ```

   Save it as `hopper-80g-pod.yaml`.

4. Apply the manifest:

   ```bash
   kubectl apply -f hopper-80g-pod.yaml
   ```

5. Verify that the pod was allocated a Hopper GPU with more than 80 GiB of memory:

   ```bash
   kubectl exec -n gpu-example hopper-80g-pod -- nvidia-smi -L
   ```

   Example output:

   ```
   GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-00000000-0000-0000-0000-000000000000)
   ```

For the full list of attributes and capacity values you can use in selectors,
refer to [ResourceSlice device attributes](../../reference/resourceslice-attributes.md).

## Clean up

Delete the pods and resources created in this guide:

```bash
kubectl delete pod -n gpu-example gpu-pod multi-gpu-pod shared-gpu-pod a100-pod large-gpu-pod hopper-80g-pod
kubectl delete resourceclaimtemplate -n gpu-example single-gpu a100-gpu large-gpu hopper-80g-gpu
```

## What's next

- [Time-slicing](time-slicing.md) — share a single GPU across multiple containers using CUDA time-slicing.
- [MIG](mig.md) — partition a GPU into isolated hardware slices for multi-tenant workloads.
- [Fabric Manager partitioning](fabric-manager-partitioning.md) — constrain a full-GPU or VFIO claim to one complete NVSwitch partition.
