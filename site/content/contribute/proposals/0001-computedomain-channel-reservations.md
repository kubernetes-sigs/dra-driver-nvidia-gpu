---
title: 0001 — ComputeDomain IMEX channel reservations
linkTitle: Channel reservations
weight: 1
description: Reserve IMEX channels per ComputeDomain to support multiple claim templates per ComputeDomain and multiple ComputeDomains per node.
---

| Field          | Value                                                                                   |
|----------------|-----------------------------------------------------------------------------------------|
| Status         | provisional                                                                             |
| Authors        | @shivamerla                                                                             |
| Created        | 2026-10-17                                                                              |
| Related issues | supersedes [kubernetes-sigs/dra-driver-nvidia-gpu#1550](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/pull/1550) |

## Summary

The ComputeDomain controller reserves IMEX channels per ComputeDomain,
records them in the ComputeDomain status, and pins each generated workload
ResourceClaimTemplate to one reserved channel. A ComputeDomain can request
several ResourceClaimTemplates, each backed by its own channel, so multiple
workloads share one ComputeDomain (and one IMEX daemon) on a node while
staying isolated from each other. The scheduler enforces placement through DRA
device taints and binding conditions, with the controller as the single gate:

- Driver-managed IMEX runs one IMEX daemon per node, for one ComputeDomain.
  Workloads of two ComputeDomains never share a node, so channel IDs are reused
  across ComputeDomains.
- Host-managed IMEX shares one IMEX domain, so channels are reserved
  cluster-wide per ComputeDomain. ComputeDomains share nodes freely as long as
  their channels differ.

## Motivation

### Who is asking for this, and why?

Today every ComputeDomain injects channel 0. Under driver-managed IMEX this
allows one workload claim per ComputeDomain per node. Under host-managed IMEX
all ComputeDomains share channel 0 of the host IMEX domain, so concurrent
multi-node workloads in one NVLink partition are not isolated from each other
and cannot share a node.

### Goals

- Multiple workload ResourceClaimTemplates per ComputeDomain, each backed by a
  distinct channel that is the same on every node (driver- and host-managed).
- Multiple ComputeDomains per node with distinct channels (host-managed).
- Never two ComputeDomains' workloads on one node (driver-managed), regardless
  of the channels they use.
- Enforcement before pods are bound, without races between ComputeDomains'
  first allocations; Prepare-time validation as a backstop.
- Reservations survive controller restarts; channels are reused after a
  ComputeDomain and its claims are gone.

### Non-goals

- Per-clique channel reuse under host-managed IMEX (channels are unique
  cluster-wide; see Open questions).
- Multiple ComputeDomains per node under driver-managed IMEX (one IMEX
  daemon per node).
- Per-pod channel isolation within one template (`imex.isolation=channel`).

## Why this belongs in the NVIDIA DRA driver

The controller already owns ComputeDomain admission and the generated claim
templates. DRA allocates each claim independently and has no notion of a
channel shared by separate claims of one ComputeDomain, so the driver must
choose the channel. Kubernetes provides the enforcement primitives (CEL
selectors, device taints); no upstream change is required.

## Proposal

### User-facing example

```yaml
apiVersion: resource.nvidia.com/v1beta1
kind: ComputeDomain
metadata:
  name: job
spec:
  channel:
    resourceClaimTemplate:
      name: job-channel
      count: 4
```

The controller creates `job-channel-0` through `job-channel-3`, reserves four
channels (driver-managed: `[0,1,2,3]`; host-managed: the lowest unreserved
IDs), and records them in `status.channels`. Template `i` requests the
`compute-domain-channel.nvidia.com` DeviceClass with the selector
`device.attributes['compute-domain.nvidia.com'].id == status.channels[i]`.
With `count: 1` (the default) the template keeps the name `job-channel`.

### Affected components

- [x] `api/` — `spec.channel.resourceClaimTemplate.count`, `status.channels`
- [ ] `gpu-kubelet-plugin`
- [x] `compute-domain-kubelet-plugin`
- [x] `compute-domain-controller`
- [ ] `compute-domain-daemon`
- [ ] admission webhook
- [x] Helm chart (`deployments/helm`)
- [ ] CDI spec generation
- [ ] Metrics
- [ ] Kubelet-plugin checkpoint schema
- [x] Documentation
- [x] CI / testing

### Authoritative state owner

The controller (single writer through leader election) owns
`ComputeDomain.status.channels`, the `ChannelAdmitted`/`ChannelRejected`
conditions in `ResourceClaim.status.devices`, and the `DeviceTaintRules`:

- host-managed: one rule per reserved channel (all nodes);
- driver-managed: one rule per node in use, named after the node, whose taint
  value is the owning ComputeDomain. Creating it is atomic in the API server,
  so the rule is the node ownership record.

Kubelet plugins read `status.channels` from their existing ComputeDomain
informer to validate claims during Prepare.

### Smallest valuable slice

`count` with pinned templates under driver-managed IMEX, which needs no
cross-ComputeDomain coordination. Host-managed uniqueness and taints build on
the same reservation path.

## Design

### API changes

- `spec.channel.resourceClaimTemplate.count`: integer, default 1, 1–128,
  immutable with the rest of the spec. `allocationMode: All` requires
  `count: 1` (CEL rule).
- `status.channels`: reserved channel IDs, index `i` backs template `i`.
- New DeviceClass `compute-domain-channel.nvidia.com` (any channel).
- Generated workload and daemon templates gain a CEL selector and a toleration
  for the taint `compute-domain.nvidia.com/owner=<UID>:NoSchedule`.
- Kubelet plugins publish channels `[0, imex.maxChannels)` (capped by the
  node's channel count) in slices of at most 128 devices.

**Reservation.** Driver-managed: channels `0..count-1`, since each
ComputeDomain has its own IMEX domain. Host-managed: the lowest IDs in
`[1, imex.maxChannels)` not reserved by any other ComputeDomain (from the
informer plus reservations written by this process and not yet observed).
Channel 0 is shared by host-managed ComputeDomains created before
reservations, and is never tainted. The reservation is persisted before
templates and taint rules are created. A reservation pruned by an outdated
CRD is reported instead of being retried.

**Prepare.** The plugin injects the allocated channel, rejects channels not in
`status.channels` (permanent error; retryable while the status is empty), and
rejects channels held by another claim on the node unless consumable shares
allow sharing within the same ComputeDomain. Only the first claim of a
ComputeDomain on a node performs the IMEX bootstrap (node label and readiness
wait, or host IMEX readiness check); later claims, from any of its templates,
only get their channel injected.

**Admission.** Published channel devices carry the binding condition
`compute-domain.nvidia.com/ChannelAdmitted` and the failure condition
`compute-domain.nvidia.com/ChannelRejected` (static device fields; slices are
never republished). The scheduler holds each pod until the controller decides
on its allocated channel, and retries elsewhere on rejection:

- driver-managed: admit if the claim's ComputeDomain owns the node, taking the
  node (creating its rule) if it is free; reject otherwise. The node rule
  taints all of the driver's devices on the node for other ComputeDomains, so
  the scheduler stops trying them there; the owner's workload and daemon
  templates tolerate it.
- host-managed: admit if the channel is in the ComputeDomain's
  `status.channels`.

Claims that cannot be attributed to an existing ComputeDomain in their
namespace (missing or invalid config, unknown ComputeDomain) are rejected, so
that they never hold a channel. Configs are decoded strictly like in the
kubelet plugin, so an invalid config is reported with the same error.

Decisions per node are serialized in the controller, so two ComputeDomains'
first claims cannot both take a node.

Admission runs regardless of the gate. With the gate off it admits all
channels, so kubelet plugins enabling the gate before the controller cannot
hold up pods, and it deletes leftover DeviceTaintRules.

**Scale.** Every channel pod waits for one admission before it is bound.
Admissions use a dedicated queue, deduplicated by claim and node, with 16
workers and no overall rate limit. Node owners are cached, as the controller
is the only writer of node rules. Claims are indexed by node and by
ComputeDomain, so release checks do not scan the cache. A claim costs one
ResourceClaim status update, plus one rule create for the first claim of a
node. The controller watches its node rules and reverts deleted or changed
ones right away, and a sweep rechecks owned nodes once a minute. The
controller's API client defaults to
100 QPS (burst 200), which admits roughly 50 to 100 pods per second. Without
leader election, the controller Deployment is updated without a surge pod
(maxSurge 0, maxUnavailable 1) so that a single controller is active at a
time. It keeps the RollingUpdate type, as switching an existing Deployment to
Recreate fails for Helm upgrades applied server-side.

**Release.** Driver-managed: a node rule is deleted once no allocated claim of
its owner remains on the node and the kubelet plugin has removed the owner's
IMEX node label, which happens on the last Unprepare. A claim deleted before
Unprepare, for example of a force-deleted pod, thus keeps the node owned until
its IMEX daemon is torn down. Releasing on a stale cache is safe, as a claim
not yet in the cache still waits for its binding condition, and admitting it
rechecks the owner. Both modes: on ComputeDomain deletion, after the templates
are removed, the controller waits until no ResourceClaim references the
ComputeDomain, deletes its taint rules, then removes the finalizer (and with
it `status.channels`). The IMEX node label is only removed when the last claim
of the ComputeDomain on a node is unprepared.

### Feature gate & graduation

New alpha gate `ComputeDomainChannelReservations` (default off) on the
controller and kubelet plugin. With the gate off, behavior is unchanged and
`count > 1` is rejected by the controller. Beta requires real multi-node
validation in both modes and an upgrade test.

### Upgrade & downgrade

Apply the CRD of the release before upgrading, as Helm does not upgrade CRDs.
With the gate off, the upgrade changes no behavior: `count` defaults to 1,
templates are found by their unchanged name, the kubelet plugin publishes the
same devices, and the checkpoint schema is unchanged.

Enabling the gate does not disrupt existing ComputeDomains:

- driver-managed: their workload and daemon templates are replaced by pinned,
  tolerating ones requesting the same channels. Claims created from the old
  templates keep working.
- host-managed: they keep sharing channel 0, which their running workloads
  use, while new ComputeDomains get channels from 1. Recreating a
  ComputeDomain moves it to its own channel.

The gate can be enabled on kubelet plugins and the controller in any order.
Disabling it leaves pinned templates of channels above 0 unschedulable, so
drain and recreate the ComputeDomains created with the gate. The controller
removes its DeviceTaintRules. Channel reservations in status are deterministic
under driver-managed IMEX, so an older daemon dropping the field during skew
is repaired on the next reconcile.

### Environment floor

Kubernetes 1.36 or later with DRA device taints (`DRADeviceTaints`,
`DRADeviceTaintRules`) and binding conditions (`DRADeviceBindingConditions`,
`DRAResourceClaimDeviceStatus`) enabled on the API server and scheduler. The
controller picks the `DeviceTaintRule` API version at startup (`v1` on 1.37+,
`v1beta2` on 1.36) and refuses to start with the gate if neither is served.
It accesses DeviceTaintRules and ResourceClaims through version-pinned
clients rather than the version-adapting DRA client, whose fallback is shared
across resource types. Nodes must have at least as many IMEX channels as the
highest reserved ID.

### Test plan

Unit tests cover reservation in both modes (persistence, exhaustion, reuse),
template naming and rendering (selector, tolerations), taint rule lifecycle,
claim admission (node ownership, rejection of other ComputeDomains, reserved
channel checks, rejection of invalid configs), node release, release blocked by claims or a
running IMEX daemon, release with uncached claims, failed status updates,
reverting deleted or changed rules, slice
chunking with binding conditions, allocated-channel injection, reservation
validation, bootstrap skipping, and node label retention. Upgrade tests cover
legacy host-managed ComputeDomains sharing channel 0, replacement of outdated
templates, admission with the gate off, removal of leftover rules, and
detection of an outdated CRD. Scale tests run 200 ComputeDomains contending
for 500 nodes through the admission workers and pin the API writes per claim.
A benchmark syncs 10,000 owned nodes with 100,000 cached claims (about 30 ms).
Hardware validation: two templates of one ComputeDomain on one node with
distinct channels and memory exchange across nodes per template (both modes);
two host-managed ComputeDomains on one node; two driver-managed
ComputeDomains never sharing a node under contention; deletion and reuse;
controller and plugin restarts. Extend `tests/bats/test_cd_imex_chan_inject.bats`.

## Risks

- Every channel claim waits for the controller before its pod is bound. If
  the controller is unavailable, pods wait out the scheduler's binding timeout
  and are retried; running workloads are unaffected.
- Editing or clearing `status.channels` under host-managed IMEX could hand a
  channel to two ComputeDomains; the controller treats status as authoritative.
- Claims retained after pod deletion delay channel release.
- Reservations assume a single active controller: run it with leader election
  or with the default update strategy without a surge pod. The old controller
  may still be shutting down while the new one starts.
- Claim status writes double, as the scheduler already writes each claim.
- Cluster-wide uniqueness limits host-managed reservations to
  `imex.maxChannels` channels across all ComputeDomains.

## Alternatives

- **ConfigMap registry (#1550):** a second source of truth, extra API calls on
  every Prepare, and extra RBAC; status is cached by existing informers.
- **Partitionable-device counters / compatibility groups:** identity-blind;
  they cannot express "only claims of ComputeDomain X may use channel k".
- **Node-local channel choice:** cannot guarantee the same channel on every
  node of a ComputeDomain.
- **Device taints alone:** a node only becomes owned after the first
  allocation, leaving a window in which another ComputeDomain's claim lands on
  it and fails at Prepare without being rescheduled. Binding conditions close
  that window.

## Drawbacks

More devices per node ResourceSlice when the gate is enabled, and a cluster-wide
list of ResourceClaims during host-managed ComputeDomain deletion.

## Future work

`imex.mode=driverManaged` with `imex.isolation=channel`: a static IMEX
DaemonSet shared by all ComputeDomains, like the host service in host-managed
mode, instead of one DaemonSet per ComputeDomain. Multiple ComputeDomains then
share nodes with mutually exclusive channels, reusing the host-managed rules
(cluster-wide reservations, channel taints, reservation admission, no node
ownership). Node ownership would then follow `isolation=domain` rather than
the IMEX mode.

## Open questions

- Per-clique reservations under host-managed IMEX, including handling nodes
  that move between NVLink partitions.
- Whether to surface reservation failures as a ComputeDomain condition.
