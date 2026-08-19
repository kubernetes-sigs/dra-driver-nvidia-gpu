# Node-Allocatable Overhead Demo

This directory demonstrates [KEP-5517 node allocatable
resources](https://github.com/kubernetes/enhancements/issues/5517): the driver
declares, per published device, how much of the node's standard `memory` and
`cpu` budget a pod consuming that device implicitly costs (for example host
memory pinned by the GPU driver and CUDA runtime). The Kubernetes scheduler
then debits those amounts from the node's allocatable budget, and the kubelet
inflates the pod-level and container-level cgroup limits so the workload is
not throttled or OOM-killed for using memory it was never able to declare in
its pod spec.

Only the `overhead` branch of the API is used: GPUs are not themselves node
resources, so there is nothing to express via `mapping`.

## Prerequisites

- Kubernetes 1.37 or newer with the `DRANodeAllocatableResources` feature gate
  (alpha) enabled on the API server, scheduler, and kubelet.
- The driver's `NodeAllocatableResources` feature gate enabled, with overhead
  values configured (see below). Setting overhead values without the driver
  gate is a startup error.

## Configuration

Overhead values are configured per device class through Helm values (or the
matching `--node-allocatable-<class>-<resource>-overhead-per-<granularity>`
flags / `NODE_ALLOCATABLE_*` environment variables of the GPU kubelet plugin):

```bash
helm upgrade -i ... \
  --set featureGates.NodeAllocatableResources=true \
  --set nodeAllocatableOverhead.gpu.memory.perPod=256Mi \
  --set nodeAllocatableOverhead.gpu.memory.perContainer=64Mi \
  --set nodeAllocatableOverhead.gpu.cpu.perPod=500m
```

The three device classes are `gpu` (full GPUs), `mig` (statically and
dynamically partitioned MIG devices), and `vfio` (passthrough devices). A
class with no configured values publishes nothing. `perPod` is charged once
per pod referencing a claim; `perContainer` is charged for each container
referencing it.

Caution for `vfio`: VM stacks such as KubeVirt typically already account for
pinned guest RAM through the VM pod's own memory requests, so configuring
vfio overhead on top of that double counts. Additionally, `PassthroughSupport`
requires an IOMMU-capable host.

## Running the demo

```bash
kubectl apply -f gpu-overhead-pod.yaml
```

The pod requests a `128Mi` memory limit and one full GPU. With the example
configuration above, observe the three effects:

1. The published device carries the overhead:

   ```bash
   kubectl get resourceslices -o yaml | grep -B2 -A8 nodeAllocatableResources
   ```

2. The scheduler records what it charged in the pod status:

   ```bash
   kubectl get pod gpu-overhead-pod \
     -o jsonpath='{.status.nodeAllocatableResourceClaimStatuses}' | jq
   ```

3. The kubelet inflates the pod's cgroup ceiling: the pod-level
   `memory.max` becomes `469762048` (448Mi = 128Mi spec limit + 256Mi perPod
   + 64Mi perContainer) instead of the spec-only 128Mi, and the pod's
   `cpu.weight` reflects the 500m CPU overhead even though the pod spec
   requests no CPU.

Because the scheduler debits the overhead against the same ledger used for
standard pod-spec requests, a pod whose spec request fits the node but whose
claim overhead does not will remain `Pending` with `Insufficient memory`
rather than overcommitting the node.
