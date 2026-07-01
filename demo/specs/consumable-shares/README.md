# Consumable Shares Demo

This directory contains example Kubernetes manifests demonstrating GPU sharing
using Dynamic Resource Allocation (DRA) with the `consumable-shares` feature
enabled.

## Configuration Modes

The GPU kubelet plugin supports three sharing modes configured via the
`--consumable-shares` CLI flag (or `CONSUMABLE_SHARES` environment variable in
Helm):

### 1. `memory`

When configured with `--consumable-shares=memory`:
- GPUs announce `allowMultipleAllocations: true`.
- The `memory` capacity entry gets a request policy where `default` equals
  total GPU memory, `min` is `1Gi`, and `step` is `1Gi`. This means that if the
ResourceClaim does not specify a `capacity.requests.memory` value, the claim
will allocate the entire GPU. Otherwise, it will allocate a share of the GPU,
and consume the specified amount from the memory value. Note that memory-based
allocation is advisory only; it is not enforced by the kernel or CUDA.

To try it out, you must run the driver with `--consumable-shares=memory`, and
then you can use this manifest:

```bash
kubectl apply -f memory-sharing.yaml
```

### 2. `unlimited`

When configured with `--consumable-shares=unlimited`:
- GPUs announce `allowMultipleAllocations: true`.
- The `memory` capacity entry gets a request policy where `default` is `0`,
  `min` is `0`, and `step` is `1Gi`. 
- Claims can share GPUs without specifying memory capacity limits, or can
  optionally specify memory requests.
- Claims that do not specify a memory capacity request will always allocate a
  share of the GPU, regardless of other claims that have already allocated the
device.

To try it out, you must run the driver with `--consumable-shares=unlimited`,
and then you can use this manifest:

```bash
kubectl apply -f unlimited-sharing.yaml
```

### 3. Integer value (e.g. `4`)

When configured with an integer such as `--consumable-shares=4`:
- GPUs announce `allowMultipleAllocations: true`.
- The `memory` capacity entry gets the same policy as `unlimited` (`default:
  0`).
- A `shares` capacity entry is added with value equal to `4`, and a request
  policy with `default: 1`, `min: 1`, `max: 4`, `step: 1`.
- Up to 4 claims (each consuming 1 share by default) can share a single
  physical or MIG GPU.

To try it out, you must run the driver with `--consumable-shares=4` (or some
other positive integer), and then you can use this manifest:

```bash
kubectl apply -f integer-sharing.yaml
```

## Scaling Replicas

Each demo manifest deploys a Kubernetes `Deployment` backed by a `ResourceClaimTemplate`. Whenever a new replica is created, Kubernetes automatically generates a unique `ResourceClaim` for that pod replica.

You can easily adjust the number of replicas to see how claims share devices across nodes:

```bash
# Scale up the unlimited sharing demo
kubectl scale deployment unlimited-sharing -n gpu-share-unlimited --replicas=6

# Check scheduled pods and generated resource claims
kubectl get pods,resourceclaims -n gpu-share-unlimited -o wide
```
