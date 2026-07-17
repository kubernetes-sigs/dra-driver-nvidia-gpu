# Consumable Shares Demo

This directory contains example Kubernetes manifests demonstrating GPU sharing
using Dynamic Resource Allocation (DRA) with the `consumable-shares` feature
enabled.

## Configuration Modes

The GPU kubelet plugin supports three sharing modes configured via the
`--consumable-shares` CLI flag (or `CONSUMABLE_SHARES` environment variable in
Helm):

### 1. `unlimited`

When configured with `--consumable-shares=unlimited`:
- GPUs announce `allowMultipleAllocations: true`.
- The `memory` capacity entry gets a request policy where `default` is `0`,
  `min` is `0`, `step` is `1Mi`, and `max` is the total GPU memory (rounded down to 1MiB).
- Claims can share GPUs without specifying memory capacity limits, or can
  optionally specify memory requests.
- Claims that do not specify a memory capacity request will allocate 0 quota,
  allowing an unlimited number of claims to co-allocate on the device.

To try it out, run the driver with `--consumable-shares=unlimited`, and apply:

```bash
kubectl apply -f unlimited-sharing.yaml
```

### 2. `memory`

When configured with `--consumable-shares=memory`:
- GPUs announce `allowMultipleAllocations: true`.
- The `memory` capacity entry gets a request policy where `default` equals
  total GPU memory (rounded down to 1MiB), `min` is `1Mi`, and `step` is `1Mi`.
- If a ResourceClaim does not specify an explicit `capacity.requests.memory` value,
  the claim will allocate the entire GPU memory quota (consuming the whole device).
- If a ResourceClaim specifies a fractional memory request (e.g. `4Gi`), it consumes
  only that requested amount, allowing multiple fractional claims to co-allocate up to the
  total GPU capacity.

To try it out, run the driver with `--consumable-shares=memory`, and apply:

```bash
kubectl apply -f memory-sharing.yaml
```

### 3. Positive Integer (e.g. `4` or `10`)

When configured with an integer such as `--consumable-shares=4`:
- GPUs announce `allowMultipleAllocations: true`.
- The `memory` capacity entry gets the same policy as `unlimited` (`default: 0`).
- A `shares` capacity entry is added with value equal to `N` (e.g. `4`), and a request
  policy with `default: 1`, `min: 1`, `max: 4`, `step: 1`.
- Up to `N` claims (each consuming 1 share by default) can share a single physical or MIG GPU.

To try it out, run the driver with `--consumable-shares=4` (or any positive integer), and apply:

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
