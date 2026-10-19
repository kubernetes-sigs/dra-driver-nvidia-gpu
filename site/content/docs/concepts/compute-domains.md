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

By default, host-managed mode supports domain isolation only: all ComputeDomains
that use the same host IMEX domain share channel 0.
With the Alpha `ComputeDomainChannelReservations` feature gate, each ComputeDomain gets its own channels instead, so several ComputeDomains can share a node and the host IMEX domain without seeing each other's memory.
See [Channel reservations](#channel-reservations).
Because this mode has no
per-ComputeDomain daemon pods, the controller disables its
`IMEXDaemonsWithDNSNames` and `ComputeDomainCliques` behaviors and does not create
`ComputeDomainClique` objects. A `Ready` ComputeDomain therefore does not report
the health of the host service.

For service and socket configuration, see [Host-managed IMEX](../prerequisites.md#host-managed-imex).

## Channel reservations

With the Alpha `ComputeDomainChannelReservations` feature gate, the `compute-domain-controller` reserves IMEX channels for each ComputeDomain and records them in `status.channels`:

- A ComputeDomain can request several workload `ResourceClaimTemplate` objects with `spec.channel.resourceClaimTemplate.count`.
  Each template is pinned to one reserved channel, so every worker using that template gets the same channel on every node, and workloads using different templates are isolated from each other.
- Before a workload pod is bound to a node, the controller admits its channel through a DRA binding condition; a rejected allocation makes the scheduler try another placement.
- In `driverManaged` mode, a ComputeDomain uses channels `0` through `count-1` of its own IMEX domain, and each node runs the IMEX daemon of one ComputeDomain.
  The first admitted claim of a ComputeDomain on a node reserves the node for it with a `DeviceTaintRule`, so the scheduler keeps other ComputeDomains' workloads off the node.
  The node is released once no claim of that ComputeDomain is allocated on it and its IMEX daemon on the node has been torn down.
- In `hostManaged` mode, channels are unique across all ComputeDomains in the cluster, and several ComputeDomains can share a node.
  Channel 0 stays shared by ComputeDomains created before the gate was enabled, which keeps their running workloads working, and new ComputeDomains get channels from 1.
  The controller creates a `DeviceTaintRule` for each reserved channel, so the scheduler only allocates it to claims of the owning ComputeDomain.
- The first claim of a ComputeDomain on a node starts or checks the IMEX daemon; later claims from any of its templates only get their channel injected.
- In `hostManaged` mode, channels are released after the ComputeDomain and all ResourceClaims that reference it are deleted.
- The controller is the only supported writer of the `DeviceTaintRule` objects labeled `resource.nvidia.com/computeDomainTaintRule`.
  It reverts deleted or changed rules, and claims stay checked at admission in the meantime.

This requires Kubernetes 1.36 or later with DRA device taints and binding conditions enabled.
The controller uses `DeviceTaintRule` from `resource.k8s.io/v1` when the cluster serves it (1.37 and later) and from `resource.k8s.io/v1beta2` otherwise; it fails to start if neither is served.
Apply the ComputeDomain CRD of the release before upgrading, as Helm does not upgrade CRDs.
Enabling the gate does not disrupt existing ComputeDomains, and it can be enabled on the kubelet plugins and the controller in any order:

- In `driverManaged` mode, the controller replaces their claim templates with pinned ones requesting the same channels. Claims created from the old templates keep working.
- In `hostManaged` mode, they keep sharing channel 0. Recreate a ComputeDomain to move it to its own channel.

Before disabling the gate, drain and recreate the ComputeDomains created with it.

Every pod with a channel claim waits for the controller's admission before it is bound.
The controller admits roughly 50 to 100 pods per second with its default API client limits (`controller.kubeAPI`).
Run a single active controller, either with leader election or with the default update strategy, which replaces the controller pod without a surge pod.

---

## Prerequisites

See [Prerequisites](../prerequisites.md) for hardware and software requirements, including the ComputeDomain-specific requirements for Multi-Node NVLink hardware, `nvidia.com/gpu.clique` label ownership, and `nvidia-imex` service configuration.

---

## Get started

To create a `ComputeDomain` and run a workload that claims an IMEX channel, see the [ComputeDomain workloads guide](../guides/compute-domain-workloads.md).
