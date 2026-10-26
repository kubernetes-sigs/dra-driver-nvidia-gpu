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
| `driverManaged` with `scope: cluster` | The DRA Driver runs one static `nvidia-imex` daemon per node, described by the `ComputeDomainDaemonSet` you create, shared by all ComputeDomains with their own channels. It requires `isolation: channel` and the `ComputeDomainDaemonSet` feature gate. See [Static IMEX daemons](#static-imex-daemons). |

The `resources.computeDomains.imex` Helm values select the IMEX daemons along three independent axes:

| Value | Choices |
|---|---|
| `mode` | Who runs the daemons: `driverManaged` (default) or `hostManaged`. |
| `scope` | Whose daemons run on a node: `computeDomain` (default), one set per ComputeDomain, or `cluster`, one set shared by all ComputeDomains. `hostManaged` always uses `cluster`. With `driverManaged`, `cluster` runs [static IMEX daemons](#static-imex-daemons). |
| `isolation` | How workloads in an IMEX domain are isolated: `domain` (default), all share channel 0, or `channel`, each workload `ResourceClaimTemplate` gets its own channel. |

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

## Static IMEX daemons

With `mode: driverManaged` and `scope: cluster`, the IMEX daemons are static and shared by all ComputeDomains:

1. You create the cluster-scoped `ComputeDomainDaemonSet` named `default`. Helm does not manage it, so driver upgrades do not restart the daemons.
2. The `compute-domain-controller` runs one daemon on every node with the `nvidia.com/gpu.clique` label, from one DaemonSet in the driver namespace.
3. Each daemon joins the IMEX domain of its NVLink clique through a `ComputeDomainClique` labeled with the `ComputeDomainDaemonSet` UID. Clique membership, DNS names and IP changes work as in driver-managed mode.
4. ComputeDomains work as in host-managed mode with channel reservations. Their claims get channels that are unique in the NVLink clique, and several ComputeDomains can share a node.
5. When a workload pod claims a channel, the `compute-domain-kubelet-plugin` waits for the static daemon of its node to be ready, then injects the channel of its claim.

Updates of the daemons restart whole cliques, either when you delete their pods or in batches of cliques. See [Static IMEX daemons](../guides/static-imex-daemons.md).

## Channel reservations

With the Alpha `ComputeDomainChannelReservations` feature gate, each workload `ResourceClaimTemplate` of a ComputeDomain gets its own IMEX channel:

- A ComputeDomain sets either `spec.channel`, one template as before, or `spec.channels`, a list of templates.
  Pods using one template get the same channel on every node of an NVLink clique, and pods using different templates are isolated from each other.
- Each kubelet plugin publishes one `channel` device that any number of channel claims on its node can share, through the `allowMultipleAllocations` field of Kubernetes DRA [consumable capacity](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#consumable-capacity).
  The device has no channel ID and no capacity, its own `type`, `imex-channel`, which the `compute-domain-channel.nvidia.com` DeviceClass selects, and names the NVLink clique of the node in its `cliqueID` attribute.
  Pods using the same template can therefore run on the same node.
- Before a workload pod is bound to a node, the controller admits its claim through a DRA binding condition and assigns it a channel, which it writes into the claim's `status.devices[].data`.
  A channel only needs to be unique in an IMEX domain, so each NVLink clique has its own channels, 1 to `imex.maxChannels - 1`.
  Claims of one template share its channel in a clique, and a channel is freed with its last claim.
  The kubelet plugin injects the channel of the claim's status.
- With `imex.scope=computeDomain`, each node runs the IMEX daemon of one ComputeDomain.
  The channel device of a node carries a `compute-domain.nvidia.com/computeDomain` taint with that ComputeDomain, and each workload template tolerates only its own ComputeDomain, so the scheduler skips nodes of other ComputeDomains.
  The controller sets the owner, through the `resource.nvidia.com/computeDomain` node label, when it admits the first claim of a ComputeDomain on a node.
  The kubelet plugin removes it after the last claim is unprepared, and the controller removes it once no claim of the ComputeDomain is left on the node, for example after a pod was deleted before it started.
  The controller rejects an allocation if another ComputeDomain has admitted claims on the node or owns it, and the scheduler then tries another placement.
  This covers claims allocated before the scheduler sees a new owner.
  Decisions are serialized per node, so the first claims of two ComputeDomains never both get a free node.
  Workloads of one ComputeDomain, from any of its templates, share a node.
- With `imex.scope=cluster`, as with `hostManaged`, several ComputeDomains can share a node, isolated by their channels.
- ComputeDomains created before the gate was enabled keep working as without it: the kubelet plugin still publishes the `channel-0` device their claim templates request, one claim per node, and channel 0 is never assigned to others.
- The first claim of a ComputeDomain on a node starts or checks the IMEX daemon, and later claims from any of its templates only get their channel injected.

This requires Kubernetes 1.34 or later with `DRAConsumableCapacity`, and DRA binding conditions (`DRADeviceBindingConditions`, `DRAResourceClaimDeviceStatus`) on the kube-apiserver and kube-scheduler.
With `imex.scope=computeDomain`, also enable [`DRADeviceTaints`](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#device-taints-and-tolerations) on the kube-apiserver and kube-scheduler (default on v1.36 and later). Without it, the API server drops the owner taint, and the controller keeps ComputeDomains apart only by rejecting their claims.
With the feature gate off, ComputeDomains keep working on every Kubernetes version the chart supports, and `spec.channels` is rejected by the controller.
Apply the ComputeDomain CRD of the release before upgrading, as Helm does not upgrade CRDs.
Older controllers and kubelet plugins do not understand `spec.channels`, so only use it once all components run this release.
Enabling the gate does not disrupt existing ComputeDomains or their pending pods, and it can be enabled on the kubelet plugins and the controller in any order:

- The controller keeps their claim templates, so their pods keep running on channel 0, one per node, without admission.
- Only ComputeDomains created with the gate get their own channels. Recreate a ComputeDomain to move it to them.
- New ComputeDomains wait for kubelet plugins with the gate, as their templates request the shared channel device.

Before disabling the gate, drain and recreate the ComputeDomains created with it.

Every pod with a channel claim waits for the controller's admission before it is bound, at the cost of one ResourceClaim status update.
The controller admits roughly 50 to 100 pods per second with its default API client limits (`controller.kubeAPI`).
With `imex.scope=computeDomain`, pods of other ComputeDomains may be tried on a node for about a second after a ComputeDomain takes it, and are then placed elsewhere.
Run a single active controller, either with leader election or with the default update strategy, which replaces the controller pod without a surge pod.

---

## Prerequisites

See [Prerequisites](../prerequisites.md) for hardware and software requirements, including the ComputeDomain-specific requirements for Multi-Node NVLink hardware, `nvidia.com/gpu.clique` label ownership, and `nvidia-imex` service configuration.

---

## Get started

To create a `ComputeDomain` and run a workload that claims an IMEX channel, see the [ComputeDomain workloads guide](../guides/compute-domain-workloads.md).
