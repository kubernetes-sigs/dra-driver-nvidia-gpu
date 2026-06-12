---
title: ComputeDomain workloads
linkTitle: ComputeDomain workloads
weight: 30
description: Create a ComputeDomain, claim a channel, and run a Multi-Node NVLink workload.
---

For background on what a `ComputeDomain` is and how it fits together, see
[ComputeDomains](../../concepts/compute-domains.md).

## Prerequisites

See [Prerequisites](../../prerequisites.md) for hardware and software requirements, including the ComputeDomain-specific requirements for Multi-Node NVLink hardware, GPU Feature Discovery, and `nvidia-imex` service configuration.

## Create a ComputeDomain

The minimal `ComputeDomain` spec requires only the name of the `ResourceClaimTemplate` the controller will create for channel allocation:

```yaml
apiVersion: resource.nvidia.com/v1beta1
kind: ComputeDomain
metadata:
  name: my-compute-domain
spec:
  numNodes: 0
  channel:
    resourceClaimTemplate:
      name: imex-channel-0
```

`numNodes` is deprecated. Set it to `0` (the recommended value when `IMEXDaemonsWithDNSNames` is enabled — its default state).

After applying this resource, the controller creates:

- A per-domain `DaemonSet` of `compute-domain-daemon` pods, one per GPU node.
- A `ResourceClaimTemplate` named `imex-channel-0` (or whatever name you gave it), which workload pods use to request a channel.

## Use the channel in a workload

Reference the `ResourceClaimTemplate` name you set in `spec.channel.resourceClaimTemplate.name` when writing your workload:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-workload
spec:
  containers:
  - name: app
    image: my-image
    command: ["bash", "-c"]
    args: ["ls -la /dev/nvidia-caps-imex-channels; sleep 9999"]
    resources:
      claims:
      - name: imex-channel-0
  resourceClaims:
  - name: imex-channel-0
    resourceClaimTemplateName: imex-channel-0
```

The pod will not start until the local IMEX daemon is ready.

### Channel allocation modes

The `spec.channel.allocationMode` field controls how many IMEX channels are injected:

| Mode | Value | Description |
|---|---|---|
| Single | `Single` (default) | Injects a single IMEX channel into the workload container |
| All | `All` | Injects all available IMEX channels (up to the hardware maximum) |

Use `All` for workloads that need access to every channel in the IMEX domain.

## Check status

```bash
kubectl get computedomain my-compute-domain -o yaml
```

The `status.status` field reports `Ready` when all expected IMEX daemons have joined. The `status.nodes` list shows each node's IP, clique ID, and individual daemon status.

To see the `ComputeDomainClique` objects created for this domain:

```bash
kubectl get computedomainclique -n nvidia-dra-driver-gpu
```

## Feature gates

Both feature gates that affect ComputeDomains are Beta and enabled by default. You do not need to set them for standard operation.

| Feature gate | Stage | Default | Effect |
|---|---|---|---|
| `IMEXDaemonsWithDNSNames` | Beta | `true` | Daemons communicate using DNS names instead of raw IP addresses. This is the recommended mode and required by `ComputeDomainCliques`. |
| `ComputeDomainCliques` | Beta | `true` | Uses `ComputeDomainClique` CRD objects to track daemon membership per clique instead of storing that information in `ComputeDomain.status.nodes`. Requires `IMEXDaemonsWithDNSNames`. |

To disable a Beta gate (for example, to test a downgrade path):

```yaml
featureGates:
  ComputeDomainCliques: false
  IMEXDaemonsWithDNSNames: false
```

See [Feature gates](../reference/feature-gates/) for all available gates.