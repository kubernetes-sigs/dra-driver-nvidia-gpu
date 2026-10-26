---
title: 0002 — Static IMEX daemons
linkTitle: 0002 Static IMEX daemons
weight: 3
description: One driver-managed IMEX daemon per node, shared by all ComputeDomains with their own channels, and rolled out by NVLink clique.
---

| Field          | Value                      |
|----------------|----------------------------|
| Status         | implementable              |
| Created        | 2026-10-25                 |
| Depends on     | 0001 channel reservations  |

## Summary

Add a cluster-scoped `ComputeDomainDaemonSet` that runs one IMEX daemon on
every node of every NVLink clique, shared by all ComputeDomains, each with its
own IMEX channels. It is selected by `imex.mode: driverManaged` with the new
`imex.scope: cluster`, which requires `imex.isolation: channel`, behind the
alpha `ComputeDomainDaemonSet` gate.
The admin owns the object, not Helm, and updates restart whole cliques, either
on pod deletion or in batches of cliques.

## Motivation

Per-ComputeDomain IMEX daemons tie IMEX to workload churn: every workload
start creates a DaemonSet, cliques and node labels, every node runs one
ComputeDomain at a time, and a driver upgrade replaces daemons. Host-managed
IMEX avoids this, but moves IMEX out of Kubernetes. Static daemons keep IMEX
driver-managed with a lifecycle that follows the fabric, not the workloads.

### Goals

- One IMEX daemon per clique node, independent of ComputeDomains.
- Several ComputeDomains per node, isolated by channels unique in each
  NVLink clique.
- Daemons that never restart because of a Helm upgrade.
- Updates that disrupt each clique once, in batches of cliques.
- Thousands of nodes.

### Non-goals

- Several `ComputeDomainDaemonSet` objects, for example per node pool.
- Migrating running per-ComputeDomain workloads in place.

## Design

### API

`ComputeDomainDaemonSet` (`resource.nvidia.com/v1beta1`, cluster-scoped,
named `default` by CEL rule):

- `spec.image`, `imagePullSecrets`, `nodeSelector`, `priorityClassName`,
  `resources`, `imexConfig`, `logVerbosity`.
- `spec.maxNodesPerClique`, default 18, immutable, as it sizes the static
  IMEX nodes config.
- `spec.updateStrategy`: `OnDelete` (default) or `RollingUpdate` with
  `maxUnavailableCliques` (number or percentage, default 1),
  `minReadySeconds` (default 30) and `paused`.
- Status: daemons desired, ready and updated in total and per clique, with
  `Ready`, `RolloutInProgress` and `DeletionBlocked` conditions.

### Controller

- One DaemonSet in the driver namespace, from
  `templates/compute-domain-static-daemon.tmpl.yaml`, with node affinity on
  `nvidia.com/gpu.clique`, which the kubelet plugin always sets in this mode,
  and its daemon `ResourceClaimTemplate`, whose daemon config carries the
  `ComputeDomainDaemonSet` UID. Both are labeled with that UID.
- The DaemonSet uses `OnDelete`. The image is `spec.image`, else the one the
  DaemonSet runs, else the driver image, and feature gates are fixed in the
  template, so driver upgrades do not change the pod template.
- A hash of the pod template, annotated on the DaemonSet and its pods, marks
  daemons as updated.
- `RollingUpdate` groups daemons by the clique of their node and restarts
  whole cliques. A clique counts as updating until each ready node runs an
  updated daemon that has been ready for `minReadySeconds`, or while recently
  restarted cliques do not show their new daemons in the cache yet.
  Unavailable outdated cliques go first. Unready nodes are left out.
- Clique entries of daemons whose pod is gone, or whose pod joined another
  partition, are removed after a live check.
- NVLink partition changes: a daemon labels its pod with the partition it
  joined. When a node's partition label changes, which the kubelet plugin
  checks every minute in this mode, the controller restarts the daemon of
  that node right away, outside the update strategy, as both IMEX domains
  are disrupted already. Its new daemon claim carries the new partition ID.
  The partition is read from the node and pod labels through one helper on
  each side, so moving from clique IDs to NVLink partition IDs changes only
  those.
- Deletion waits, with `DeletionBlocked`, until no `ResourceClaim` is
  allocated an IMEX channel.
- All events map to one queue key with a short delay. A sync of 1,800 daemons
  in 100 cliques takes about 1 ms from the informer caches.

ComputeDomains take the shared-domain path of host-managed IMEX: channels
assigned per clique at admission, and no node ownership, node labels or
per-ComputeDomain daemons.

### Daemon

The daemon gets `COMPUTE_DOMAIN_DAEMONSET_UID` instead of
`COMPUTE_DOMAIN_UUID` and registers in the clique `<uid>.<cliqueID>` labeled
`resource.nvidia.com/computeDomainDaemonSet`. DNS names, the nodes config,
`/etc/hosts` updates and SIGUSR1 on IP changes are unchanged.

### Kubelet plugin

- Channel claims wait for the `ComputeDomainDaemonSet` and the node's daemon
  to be ready in its clique, then inject the channel of the claim.
- Daemon claims of any other `ComputeDomainDaemonSet` UID fail permanently.
- The daemon config directory of the `ComputeDomainDaemonSet` survives the
  cleanup of directories without a ComputeDomain.

## Alternatives

- **A static ComputeDomain per clique** routed through the per-ComputeDomain
  path. Keeps one ComputeDomain per node and mixes infrastructure with user
  objects.
- **A Helm-rendered DaemonSet.** Every Helm upgrade would restart IMEX under
  running workloads.
- **The DaemonSet `RollingUpdate` strategy.** Restarts node by node, which
  disrupts each clique once per node.
- **A new `imex.mode` value.** Every branch on the mode would have to handle
  it, and it would mix who runs the daemons with whose daemons they are.
- **`isolation: channel` alone.** Channel isolation also applies to
  per-ComputeDomain daemons, where each template gets its own channel, so it
  cannot also select shared daemons. `scope` keeps the three axes
  independent: `mode` is who runs the daemons, `scope` whose daemons they
  are, and `isolation` how workloads in an IMEX domain are separated.

## Risks

- **Single active controller:** rollouts assume one active controller, as
  reservations do. Enable leader election.
- **Clique label:** a node without the `nvidia.com/gpu.clique` label runs no
  daemon. Its workloads get no channel injected, as on non-fabric nodes.
- **DaemonSet node selection:** the DaemonSet controller removes daemons
  from nodes a new pod template no longer selects, whatever the update
  strategy. Such spec changes are held back, with `UpdateBlocked`, while
  those nodes have workloads using IMEX channels.
- **Driver upgrades:** a new driver may render a different pod template.
  The template is only re-rendered on a spec change, or to revert edits of
  the DaemonSet, detected through its generation, and `TemplateOutdated`
  reports the difference. The image pinned at creation is kept in a
  DaemonSet annotation, so edits to the DaemonSet cannot change it.
- **Invalid specs:** CEL rules bound `image` and `imexConfig`, and the
  controller validates node selectors, names and IMEX settings before
  applying them. Spec values are set on the rendered object, not in the
  YAML template. An invalid spec leaves the daemons alone, with
  `Degraded=InvalidSpec`.
- **Migrating from per-ComputeDomain daemons:** not supported with running
  workloads, as both kinds of daemons cannot run on a node. Leftovers are
  handled anyway.
  Static daemons skip nodes with the `resource.nvidia.com/computeDomain`
  label, which the kubelet plugin also removes in this mode, and deleting a
  ComputeDomain created before the switch deletes its per-ComputeDomain
  DaemonSet. Switching back leaves the static daemons running, reported with
  `Degraded=StaticDaemonsDisabled`.
- **Uninstall:** Helm does not delete the `ComputeDomainDaemonSet`, so the
  daemons keep running. Delete it first to remove them.

## Test plan

Unit tests cover the gate and mode validation, clique labels of the daemon,
static channel and daemon claims in the kubelet plugin, the DaemonSet,
template and status, image pinning, deletion blocking and orphan cleanup,
stale clique entries, and rolling updates: batch sizes, `minReadySeconds`,
lagging caches, failed nodes, unavailable cliques and pausing. Before
graduation, bats tests on GB200 cover creating the daemons, two
ComputeDomains on one node, and a rolling update with running workloads.

## Graduation

Beta needs the bats coverage above and production feedback on rollouts.
