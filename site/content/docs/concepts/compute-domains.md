---
title: ComputeDomains
linkTitle: ComputeDomains
weight: 30
description: How compute-domain.nvidia.com provisions ephemeral Multi-Node NVLink fabrics via IMEX.
---

A `ComputeDomain` is a custom resource that sets up a group of nodes to run a multi-node workload using NVLink fabric. It is used to enable GPU memory sharing across nodes in hardware that supports Multi-Node NVLink (MNNVL), such as GB200 NVL72 or H100 NVLink configurations.

---

## IMEX lifecycle modes

The `resources.computeDomains.imex.mode` Helm value determines who manages the `nvidia-imex` daemon lifecycle.

| Mode | Lifecycle |
|---|---|
| `driverManaged` | By default, the DRA Driver creates one `nvidia-imex` DaemonSet for each `ComputeDomain` and tracks daemon readiness through `ComputeDomainClique` resources. |
| `hostManaged` | You run `nvidia-imex` as a host service with a ready command socket, and the DRA Driver does not create daemon DaemonSets or daemon claims; this mode requires the `HostManagedIMEXDaemon` feature gate. |

## How driver-managed mode works

Creating a `ComputeDomain` in the default `driverManaged` mode triggers the following sequence:

1. The `compute-domain-controller` watches for new `ComputeDomain` resources and creates a per-domain DaemonSet.
2. Each daemon pod in that DaemonSet runs `nvidia-imex`, which manages the NVLink fabric connection on its node.
3. Each daemon publishes its IP address, clique membership, and readiness via a `ComputeDomainClique` CR in the driver namespace.
4. The `compute-domain-controller` also creates a `ResourceClaimTemplate` per channel, making IMEX channels available for workload pods to claim.
5. When a workload pod claims a channel, the `compute-domain-kubelet-plugin` injects one or more selected IMEX channel devices (`/dev/nvidia-caps-imex-channels/channelN`) into the container.

For the full sequence diagram, see [Architecture › Driver-managed ComputeDomain flow](architecture.md#driver-managed-computedomain-flow).

## How host-managed mode works

Creating a `ComputeDomain` in `hostManaged` mode uses an existing host `nvidia-imex` service:

1. You run `nvidia-imex` on each participating GPU node and expose its command socket.
2. The `compute-domain-controller` creates the workload `ResourceClaimTemplate`.
3. When a workload pod claims a channel, the `compute-domain-kubelet-plugin` queries the host daemon through the daemon's command socket and requires a `READY` response. If the daemon is unavailable or not ready, claim preparation fails and is retried.
4. After the readiness check succeeds, the plugin injects channel 0 (`/dev/nvidia-caps-imex-channels/channel0`) into the workload container.
5. Deleting the `ComputeDomain` removes the workload claim template, but it does not stop or reconfigure the host service.

Host-managed mode currently supports domain isolation only. All ComputeDomains
that use the same host IMEX domain share channel 0. Because this mode has no
per-ComputeDomain daemon pods, the controller disables its
`IMEXDaemonsWithDNSNames` and `ComputeDomainCliques` behaviors and does not create
`ComputeDomainClique` objects. A `Ready` ComputeDomain therefore does not report
the health of the host service.

For service and socket configuration, see [Host-managed IMEX](../prerequisites.md#host-managed-imex).

---

## Prerequisites

See [Prerequisites](../prerequisites.md) for hardware and software requirements, including the ComputeDomain-specific requirements for Multi-Node NVLink hardware, `nvidia.com/gpu.clique` label ownership, and `nvidia-imex` service configuration.

---

## Get started

To create a `ComputeDomain` and run a workload that claims an IMEX channel, see the [ComputeDomain workloads guide](../guides/compute-domain-workloads.md).
