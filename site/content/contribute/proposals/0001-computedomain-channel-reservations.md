---
title: 0001 — ComputeDomain IMEX channel reservations
linkTitle: Channel reservations
weight: 1
description: Assign an IMEX channel per NVLink clique to each ComputeDomain ResourceClaimTemplate to support multiple templates per ComputeDomain, multiple pods per template on a node, and multiple ComputeDomains per node.
---

| Field          | Value                                                                                   |
|----------------|-----------------------------------------------------------------------------------------|
| Status         | provisional                                                                             |
| Authors        | @shivamerla                                                                             |
| Created        | 2026-10-17                                                                              |
| Related issues | supersedes [kubernetes-sigs/dra-driver-nvidia-gpu#1550](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/pull/1550) |

## Summary

A ComputeDomain lists its workload ResourceClaimTemplates in `spec.channels`.
Kubelet plugins publish one `channel` device that all channel claims of
a node share. Before a pod is bound, the ComputeDomain controller admits its
claim through a DRA binding condition and assigns it a channel, which it
writes into the claim's device status. A channel only needs to be unique in
its IMEX domain, the NVLink clique, so each clique has its own 2047 channels.
The kubelet plugin injects the channel of the claim's status. So:

- Every pod of a template gets the template's channel on every node of a
  clique, and pods of a template can share a node.
- Templates of a ComputeDomain are isolated from each other.
- ComputeDomains sharing an IMEX domain (cluster scope) get distinct channels
  and share nodes freely.
- With per-ComputeDomain IMEX daemons (compute-domain scope), a node runs one
  ComputeDomain at a time, which the scheduler sees through an owner
  attribute and the controller enforces at admission.
- The number of concurrent workloads is bounded per clique, not per cluster,
  and no ComputeDomain status is written for channels.

## Motivation

### Who is asking for this, and why?

Today every ComputeDomain injects channel 0, and the channel device of a node
can be allocated once. Under driver-managed IMEX this allows one workload pod
per ComputeDomain per node. Under host-managed IMEX all ComputeDomains share
channel 0 of the host IMEX domain, so concurrent multi-node workloads in one
NVLink partition are not isolated from each other and cannot share a node.

### Goals

- Multiple workload ResourceClaimTemplates per ComputeDomain, each backed by a
  distinct channel that is the same on every node of a clique.
- Multiple pods of a template on a node, sharing its channel.
- Multiple ComputeDomains per node with distinct channels (cluster scope).
- Never two ComputeDomains' workloads on one node (compute-domain scope),
  steered by the scheduler and enforced before pods are bound.
- More than 2048 concurrent templates in a cluster.
- No channel writes to ComputeDomain status.
- Assignments survive controller restarts. Channels are reused once their
  claims are gone.
- Upgrades with the gate off change nothing.

### Non-goals

- Multiple ComputeDomains per node with per-ComputeDomain IMEX daemons.
- Capping the pods per channel on a node.

## Why this belongs in the NVIDIA DRA driver

The controller already owns ComputeDomain admission and the generated claim
templates. DRA allocates each claim independently and has no notion of a
channel shared by separate claims, so the driver must choose the channel.
Kubernetes provides multi-allocatable devices for sharing a device and
binding conditions for deciding before binding. No upstream change is
required.

## Proposal

### User-facing example

```yaml
apiVersion: resource.nvidia.com/v1beta1
kind: ComputeDomain
metadata:
  name: job
spec:
  channels:
  - resourceClaimTemplate:
      name: training
  - resourceClaimTemplate:
      name: inference
```

The controller creates the templates `training` and `inference`. Each
requests the `compute-domain-channel.nvidia.com` DeviceClass, which selects
the `channel` device, and names itself in its
`ComputeDomainChannelConfig`. When it admits a claim, the controller writes
the channel into the claim's device status:

```yaml
status:
  devices:
  - driver: compute-domain.nvidia.com
    device: channel
    conditions:
    - type: compute-domain.nvidia.com/ChannelAdmitted
      status: "True"
    data:
      channel: 3
      imexDomain: 9561f0f1-93a2-4d52-9e29-4041d4aa1d4c.32766
```

`spec.channel` keeps working and is treated as a list of one.

### Affected components

- [x] `api/` — `spec.channels`, `ComputeDomainChannelConfig.resourceClaimTemplateName`
- [ ] `gpu-kubelet-plugin`
- [x] `compute-domain-kubelet-plugin`
- [x] `compute-domain-controller`
- [ ] `compute-domain-daemon`
- [x] admission webhook (validates the template name)
- [x] Helm chart (`deployments/helm`)
- [ ] CDI spec generation
- [ ] Metrics
- [ ] Kubelet-plugin checkpoint schema
- [x] Documentation
- [x] CI / testing

### Authoritative state owner

The controller (single writer through leader election) owns the
`ChannelAdmitted`/`ChannelRejected` conditions and the channel data in
`ResourceClaim.status.devices`. Its assignments live in memory and are
rebuilt from the claims cache after a restart. Node ownership is not stored
apart from the ComputeDomain node label. Kubelet plugins read the channel
from the claim they prepare.

### Smallest valuable slice

`spec.channels` under compute-domain scope.

## Design

### API changes

- `spec.channels`: list of `{resourceClaimTemplate: {name}}`, 1–128 entries,
  unique names, no `allocationMode`. CEL rules require exactly one of
  `channel` and `channels`, and the spec stays immutable. `channel` becomes
  optional in the schema.
- `ComputeDomainSpec.GetChannels()` returns either form as a list, so all
  callers handle them alike.
- `ComputeDomainChannelConfig.resourceClaimTemplateName`: optional, set by the
  controller, at most 253 characters, validated by the kubelet plugin and
  the webhook. Claims of templates without it keep the legacy channel 0.
- Device status data `{channel, imexDomain}` of the `channel` device,
  set at admission.
- New DeviceClass `compute-domain-channel.nvidia.com`, which selects the
  `channel` device by its own `type`, `imex-channel`. Workload
  templates request it without a CEL selector, and the
  `compute-domain-default-channel.nvidia.com` DeviceClass stays unchanged.

**Device.** With the gate, each kubelet plugin publishes one `channel`
device with `allowMultipleAllocations` and no capacity, so any number of
claims share it. It carries no channel ID, as the device only brings claims
to the kubelet plugin. Of the per-channel devices, only `channel-0` stays
published, unchanged, for the templates of ComputeDomains created before the
gate. The
device has the `ChannelAdmitted` binding condition and the `ChannelRejected`
binding failure condition, and:

- `type: imex-channel`, so that DeviceClasses tell it from `channel-0`.
- `cliqueID`: the NVLink clique of the node, republished when the periodic
  clique refresh finds a new partition.
- A `compute-domain.nvidia.com/computeDomain=<uid>:NoSchedule` taint
  (compute-domain scope): the owner of the node, from the
  `resource.nvidia.com/computeDomain` node label, which the plugin watches.
  Workload templates tolerate only their own ComputeDomain, so the scheduler
  places them on free nodes or nodes of their ComputeDomain. `NoSchedule`
  leaves claims already allocated alone.

The device is published once the plugin is ready and is not withdrawn while
IMEX daemons restart, so allocations do not churn with daemon restarts.

**Assignment.** Claims of one template of a ComputeDomain share a channel in
each IMEX domain, the clique the node's channel device names, or the node
itself without one. The controller assigns it when it admits the first such
claim, from `[1, imex.maxChannels)`, starting after the channel it assigned
last, so that a freed channel is reused last. The assignment stays while any
claim holds it and is freed when the last one is deallocated or deleted. It
is written into the claim's device status in the same update as the
`ChannelAdmitted` condition, so it costs no extra API call. After a restart
the controller restores the assignments from the admitted claims before it
admits new ones. A clique without a free channel rejects the claim with
`NoFreeChannel`, and the scheduler tries another placement. Channel 0 is
never assigned, as ComputeDomains created before the gate use it. Their
templates are kept, and request the `channel-0` device without admission.
Claims of `channel` without a template name, which the controller never
creates, get channel 0 as well. Templates created with the gate are replaced
when their request or config differ from the current one.

**Prepare.** The plugin takes the channel from the claim's device status for
the `channel` device, or from the device name for per-channel devices
allocated before the upgrade. It rejects permanently a missing channel, a
channel the node does not have, and a channel prepared for another
ComputeDomain on the node, except for the shared legacy channel 0 under
host-managed IMEX. A channel assigned for another clique, after a
repartition the controller had not seen, is logged, as it is still unique
on the node. Claims of a ComputeDomain share their channel. Only the first
claim of a ComputeDomain on a node performs the IMEX bootstrap (node label
and readiness wait, or host IMEX readiness check). Later claims, from any of
its templates, only get their channel injected. The checkpoint records each
claim's channel in the existing channel info, so its schema is unchanged.

**Admission.** The scheduler holds each pod until the controller decides on
its allocated channel, and retries elsewhere on rejection. In every scope,
the controller rejects claims that cannot be attributed to an existing
ComputeDomain in their namespace, or whose ComputeDomain is being deleted,
and assigns the channel of admitted ones. Configs are decoded strictly like
in the kubelet plugin.

Under compute-domain scope it also admits a claim only if no other
ComputeDomain uses the node, where a ComputeDomain uses a node while:

- it has admitted channels there (or channels allocated without a binding
  condition, by a plugin without the gate),
- this process admitted one of its claims there that the cache does not show
  yet, or
- it owns the node, shown by the node label.

The controller sets the node label when it admits the first claim of a
ComputeDomain on a node, under the node lock, so the scheduler stops trying
the node for other ComputeDomains once the plugin publishes the owner. The
admission stays the guarantee for claims allocated before that. Decisions
are serialized per node, so two ComputeDomains' first claims cannot both take
a node. Undecided and rejected claims do not hold a node.

Admission runs regardless of the gate. With the gate off it admits all
channels, so kubelet plugins enabling the gate before the controller cannot
hold up pods.

**Scale.** Every channel pod waits for one admission before it is bound,
costing one ResourceClaim status update, plus a node read and update for the
first claim of a ComputeDomain on a node under compute-domain scope.
Admissions use a dedicated queue, deduplicated by claim, with 16 workers and
no overall rate limit. Claims are indexed by node and by ComputeDomain, and
ResourceSlices by node, so a decision only looks at its node. The
controller's API client defaults to 100 QPS (burst 200), which admits roughly
50 to 100 pods per second. Without leader election, the controller Deployment
is updated without a surge pod (maxSurge 0, maxUnavailable 1) so that a
single controller is active at a time.

**Release.** Channels are freed with their last claim. Compute-domain scope:
a node is free once its owner has no admitted claim left on it and the node
label is gone. The kubelet plugin removes the label after the last claim is
unprepared. The controller removes it, after a short delay, once no claim of
the owner holds a channel on the node, which covers claims admitted but never
prepared, for example of pods deleted before they started.

### Why not DeviceTaintRules

An earlier version published one device per channel, pinned templates with a
CEL selector on the channel ID, and tainted nodes in use and reserved
channels with DeviceTaintRules. With one shared device and the channel
assigned at admission, only the owner of a node needs to reach the
scheduler. The plugin taints its own `channel` device in its
ResourceSlice instead, without an API object per node and its lifecycle.

### Why a taint and not an owner attribute

An owner attribute on the channel device works without `DRADeviceTaints`,
but every workload template then needs a CEL selector naming its
ComputeDomain, and the ownership hides in a selector. A taint states it on
the device, and templates only carry a toleration. The driver already
depends on `DRADeviceTaints` for GPU health. Without it the API server drops
the taint, and admission alone keeps ComputeDomains apart, at the cost of
rejected attempts. A claim tolerating all taints skips the steering the same
way, and is rejected at admission as well.

### Why not assign channels in ComputeDomain status

An earlier version assigned each template one channel in the ComputeDomain
status, unique across the cluster, and put it into the template config. That
limits a cluster to 2047 templates, writes ComputeDomain status for every
ComputeDomain, and needs the same channel in every clique although cliques
are separate IMEX domains. Assigning at admission knows the clique of the
node, and the claim status the controller writes anyway carries the channel.

### Feature gate & graduation

Alpha gate `ComputeDomainChannelReservations` (default off) on the controller
and kubelet plugin. With the gate off, behavior is unchanged and
`spec.channels` is rejected by the controller. Beta requires real multi-node
validation in both scopes and an upgrade test.

### Upgrade & downgrade

Apply the CRD of the release before upgrading, as Helm does not upgrade CRDs.
With the gate off, the upgrade changes no behavior: templates are rendered as
before, the kubelet plugin publishes the same devices, the claim configs and
the checkpoint schema are unchanged, and no status is written.

Older controllers and kubelet plugins require `spec.channel`, so only use
`spec.channels` once all components run this release.

Enabling the gate does not disrupt existing ComputeDomains or their pending
pods:

- Their templates are kept, and the plugin still publishes the `channel-0`
  device they request, so their pods run as without the gate, one per node
  on channel 0. Per-channel devices allocated before the upgrade are still
  prepared.
- New ComputeDomains get channels from 1. Recreating a ComputeDomain moves
  it to its own channels.

The gate can be enabled on kubelet plugins and the controller in any order.
Pods of new ComputeDomains wait for kubelet plugins with the gate.
Disabling it leaves templates requesting the `channel` device
unschedulable, so drain and recreate the ComputeDomains created with it.

### Environment floor

Kubernetes 1.34 or later with `DRAConsumableCapacity` (default from 1.36) on
the API server, controller manager, scheduler and kubelet, and DRA binding
conditions (`DRADeviceBindingConditions`, `DRAResourceClaimDeviceStatus`) on
the API server and scheduler. With `imex.scope=computeDomain`, also
`DRADeviceTaints` (default from 1.36) on the API server and scheduler. Nodes
must have at least as many IMEX channels
as `imex.maxChannels`.

### Test plan

Unit tests cover the API (both forms as a list, template name validation),
the CRD rules (exactly one of `channel` and `channels`, unique names, rule
cost), the assigner (sharing per template and clique, exhaustion,
wrap-around, reuse last, restore), admission (channel data, cliques from
ResourceSlices, nodes without a clique, legacy channel 0, per-channel
devices, restarts, node in use through admitted claims, in-process admissions
and the node label, labeling and releasing nodes, undecided and rejected
claims, concurrent first claims of 32 ComputeDomains on one node, status
update failures, invalid configs), template rendering and replacement
(unchanged without the gate, kept for ComputeDomains from before it,
template name and selector with it), the published devices (`channel-0`
kept, the shared device without capacity or ID, clique, owner and binding
conditions, republishing on owner and clique changes), and Prepare (channel from the
claim status, sharing within a ComputeDomain, rejection across
ComputeDomains, shared legacy channel, missing or unknown channels,
per-channel devices from before the upgrade, checkpointed channel). A scale
test runs 200 ComputeDomains contending for 500 nodes through the admission
workers and pins the API calls per claim. Hardware validation: two templates
of one ComputeDomain on one node with distinct channels and memory exchange
across nodes per template, pods of one template sharing a node,
ComputeDomains sharing nodes in cluster scope, ComputeDomains never sharing a
node under contention in compute-domain scope, deletion and reuse, controller
and plugin restarts, and an upgrade with the gate off.

## Risks

- Every channel claim waits for the controller before its pod is bound. If
  the controller is unavailable, pods wait out the scheduler's binding
  timeout and are retried. Running workloads are unaffected.
- A node repartitioned after its claims were admitted keeps their channels,
  which may not be unique in its new clique. Its IMEX daemon restarts
  anyway, and new claims get channels of the new clique.
- Claims retained after pod deletion delay channel and node release.
- Assignments assume a single active controller: run it with leader election
  or with the default update strategy without a surge pod.
- Templates of ComputeDomains created before the gate keep channel 0 only
  while they exist. A deleted one is recreated with the gate, and its new
  pods get another channel than its running pods.

## Alternatives

- **Channels in ComputeDomain status (earlier version):** see above.
- **One device per channel with CEL selectors (earlier version):** the
  scheduler enforced the channel, but each channel could back only one claim
  per node without consumable shares on every channel device, and taints were
  needed to keep other ComputeDomains off reserved channels and nodes in use.
- **ConfigMap registry (#1550):** a second source of truth, extra API calls on
  every Prepare, and extra RBAC.
- **Node-local channel choice:** cannot guarantee the same channel on every
  node of a clique.
- **Pod anti-affinity injected by a mutating webhook:** enforces node
  exclusivity in the scheduler without DRA alpha features, but adds a webhook
  on the pod path and inter-pod anti-affinity cost at scale.

## Drawbacks

Pods of other ComputeDomains may be tried on a node under compute-domain
scope while its new owner propagates to the ResourceSlice, about a second
after the first admission. Under cluster scope, pod binding now waits for the
controller as well.

## Future work

`imex.mode=driverManaged` with `imex.scope=cluster`: a static IMEX DaemonSet
shared by all ComputeDomains, like the host service in host-managed mode,
instead of one DaemonSet per ComputeDomain. It reuses the cluster-scope rules
(per-clique assignments, no node ownership).

## Open questions

- Whether to surface assignment failures as a ComputeDomain condition.
