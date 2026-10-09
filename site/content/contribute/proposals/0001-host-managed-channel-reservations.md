---
title: 0001 — Host-managed ComputeDomain channel reservations
linkTitle: Host-managed channel reservations
weight: 1
description: Reserve one consistent IMEX channel per ComputeDomain in a shared host domain.
---

| Field | Value |
|---|---|
| Status | provisional |
| Authors | To be supplied by the contributing maintainer |
| Created | 2026-10-09 |
| Related issues | None supplied |

## Summary

In host-managed IMEX mode, reserve a distinct channel for each ComputeDomain
and request that same channel on every worker node. Persist reservations across
controller restarts, and return a channel to the pool when the ComputeDomain
and its workload claims are removed. Driver-managed mode keeps its current
channel publication and injection behavior.

## Motivation

### Who is asking for this, and why?

Administrators running concurrent multi-node workloads in one existing host
IMEX domain need different ComputeDomains to use distinct channels. The current
host-managed implementation selects channel zero for every ComputeDomain.

### Goals

- Distinct ComputeDomains receive distinct channel IDs.
- Workers of one ComputeDomain request the same ID across nodes.
- Retries and controller restarts preserve assignments.
- Finalization releases reservations only after workload claims disappear.

### Non-goals

Per-pod reservations, multiple independent host domains in one installation,
same-node sharing between independent claims, and host daemon lifecycle changes.

## Why this belongs in the NVIDIA DRA driver

This controller already owns ComputeDomain admission and generated workload
claim templates. Kubernetes DRA selects node-local devices but does not assign
a common channel to separate claims from one NVIDIA ComputeDomain. The GPU
Operator owns prerequisite services, not these ComputeDomain reservations;
legacy device plugins and external CLIs do not own the generated claims.
No upstream Kubernetes change or additional dependency is required.

## Proposal

### User-facing example

Existing ComputeDomain manifests remain valid:

```yaml
apiVersion: resource.nvidia.com/v1beta1
kind: ComputeDomain
metadata:
  name: job-a
spec:
  numNodes: 0
  channel:
    resourceClaimTemplate:
      name: job-a-channel
```

With host-managed mode enabled, `job-a` reserves channel zero. A second domain
reserves channel one. The generated templates use
`compute-domain-channel.nvidia.com` and a CEL selector for the reserved ID.
After `job-a` and its claims are removed, the next domain can reserve zero.

### Affected components

- ComputeDomain controller: persistent allocation, pinned templates, finalization.
- ComputeDomain kubelet plugin: publication of all channels and reservation validation.
- Helm chart: all-channel DeviceClass and registry/discovery RBAC.
- Documentation and unit tests.
- API types, generated code, checkpoint schemas, GPU plugin, daemon, webhook,
  metrics, and external dependencies are unchanged.

### Authoritative state owner

Only the controller writes `compute-domain-imex-channel-reservations`, a
ConfigMap in the driver namespace mapping ComputeDomain UIDs to channel IDs.
Creates and updates use API conflict retries. Plugins read this object during
Prepare. It has no owner reference to an individual domain and must not be
deleted or modified while domains exist.

### Smallest valuable slice

One shared host IMEX domain per installation, with exclusive claims on each
node and channel reservations lasting for the ComputeDomain lifetime.

## Design

### API changes

No new CRD fields or opaque config fields. A new DeviceClass selects any IMEX
channel. Generated templates add a request-level channel ID selector.
The registry is controller implementation state, not a user configuration knob.
Node pools advertise all channels across slices of at most 128 devices.

The controller intersects channels across currently published node pools,
using only complete latest-generation pools. New reservations wait for complete
publication and fail retryably on exhaustion. Existing reservations are stable
even if node advertisements temporarily disappear. A later node lacking the
reserved channel cannot satisfy that domain's claim.

During deletion, remove the template first, then wait until no claim spec or
allocated config refers to the domain UID. Remove the registry entry before
removing the ComputeDomain finalizer. API errors retain the reservation.
Node-local checkpoints continue to reject conflicting preparations, including
when claim removal precedes node cleanup. A newly scheduled claimant waits for
the prior preparation to be unprepared rather than injecting a busy device.
Force deletion is not proof that a disconnected node has stopped its workload;
administrators must fence such nodes before replacing their workloads.

### Feature gate & graduation

This extends the existing alpha `HostManagedIMEXDaemon` behavior and requires
`imex.mode=hostManaged`. No new gate. `imex.isolation=domain` groups workers
by ComputeDomain; the reserved `channel` setting for per-workload isolation
remains unsupported. Real multi-node isolation and upgrade testing remain
necessary before graduation.

### Upgrade & downgrade

Drain workloads and remove claims and ComputeDomains before upgrading from
channel-zero-only host mode. Upgrade the chart and all relevant binaries,
then recreate domains. Existing unpinned templates fail reconciliation rather
than being silently rewritten. Follow the same drain procedure on downgrade.
Controller restarts retain registry state; node checkpoints retain their
current schema. Do not run multiple driver installations against the same
host IMEX domain with independent registries.

### Environment floor

The existing supported host-managed IMEX environment and Kubernetes DRA API
remain prerequisites. All participating nodes must belong to the configured
shared host IMEX domain, expose the reserved channel, and have a ready host
daemon. This change does not alter hardware or driver version prerequisites.

### Test plan

Controller unit tests cover distinct IDs, restart persistence, conflict retries,
exhaustion, latest-generation pool completeness, and release blocked by claims.
Template tests cover zero and nonzero pinned selectors and driver-managed mode.
Plugin tests cover bounded deterministic publication and rejection of a
channel different from its reservation. Run unit tests with the race detector
and Helm policy/lint checks.

On real MNNVL hardware, create two domains with workers on multiple nodes,
verify their injected channel paths are consistent within each domain and
different between domains, and run memory exchange. Delete one domain and its
claims, verify reuse, and exercise plugin/controller restart and force deletion.
Extend `tests/bats/test_cd_imex_chan_inject.bats` for this scenario before
graduation; no hardware execution is implied by unit test success. Mock NVML
can validate publication but cannot prove actual host IMEX isolation.

## Risks

Deleting the registry loses authoritative ownership. Missing/corrupt registry
reads block new preparation; operators must preserve it. Claims deliberately
retained after pod deletion also retain reservations during domain deletion.
Partial rolling upgrades cannot safely introduce nonzero channels.

## Alternatives

Node-local selection cannot guarantee a common channel across nodes.
CEL selectors express a selected ID but cannot reserve it across domains.
`matchAttribute` constrains devices within an allocation, not independent
per-worker claims. A ResourceClaim-based global channel pool would require a
larger allocation and orchestration design. Existing NVML/device libraries
provide device discovery, not persistent ComputeDomain channel ownership.

## Drawbacks

One ConfigMap serializes reservations and adds API traffic. Conservatively
intersecting node pools can reduce capacity when one node advertises fewer
channels. Idle ComputeDomains retain reservations until deleted.

## Open questions

Maintainer review of the reservation contract and real-hardware validation
remain outstanding. Same-node claim sharing and multiple host domains are
separate future designs.
