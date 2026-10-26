---
title: Static IMEX daemons
linkTitle: Static IMEX daemons
weight: 35
description: Run one IMEX daemon per node, shared by all ComputeDomains, and roll it out by NVLink clique.
---

By default, every `ComputeDomain` gets its own IMEX daemons, so a node runs the
workloads of one `ComputeDomain` at a time. With static IMEX daemons, one IMEX
daemon per node is shared by all ComputeDomains, and each `ComputeDomain` gets
its own IMEX channels. Workloads of several ComputeDomains can then share a
node, and starting a workload no longer starts IMEX daemons.

The daemons are described by a cluster-scoped `ComputeDomainDaemonSet` named
`default`. You create and update it, not Helm, so that driver upgrades do not
restart IMEX daemons under running workloads.

## Prerequisites

- The prerequisites of [Channel reservations](../concepts/compute-domains.md#channel-reservations).
- Helm values:

  ```yaml
  resources:
    computeDomains:
      imex:
        mode: driverManaged
        scope: cluster
        isolation: channel
  featureGates:
    ComputeDomainDaemonSet: true
    ComputeDomainChannelReservations: true
  ```

- To switch an existing installation, see
  [Migrate from per-ComputeDomain daemons](#migrate-from-per-computedomain-daemons).

## Create the daemons

```bash
kubectl apply -f demo/specs/imex/computedomaindaemonset.yaml
```

The controller creates a DaemonSet in the driver namespace. Its daemons run on
every node with the `nvidia.com/gpu.clique` label, which the kubelet plugin
sets in this mode, and that matches `spec.nodeSelector`. Each daemon joins the
IMEX domain of its NVLink clique through a `ComputeDomainClique` named
`<ComputeDomainDaemonSet UID>.<clique ID>`.

The DaemonSet deletes the daemon of a node as soon as the node stops matching,
whatever the node affinity says about execution. The kubelet plugin therefore
keeps the `nvidia.com/gpu.clique` label across its restarts and driver upgrades.
Do not remove it, or change labels that `spec.nodeSelector` matches, on nodes
with running workloads.

Workload pods wait in `ContainerCreating` until the `ComputeDomainDaemonSet`
exists and the daemon of their node is ready.

```bash
kubectl get computedomaindaemonset default
```

```
NAME      READY   DESIRED   UPDATED   CLIQUES   STRATEGY        AGE
default   1800    1800      1800      100       RollingUpdate   3d
```

`status.cliques` reports the nodes, ready daemons and updated daemons of each
clique.

## Update the daemons

Restarting an IMEX daemon disrupts the workloads of its whole clique. Changes
to the spec, such as `image` or `imexConfig`, update the DaemonSet, but how the
daemons pick them up depends on `spec.updateStrategy.type`:

| Type | Behavior |
|---|---|
| `OnDelete` (default) | Daemons keep running their revision. The `RolloutInProgress` condition counts outdated daemons. Delete the daemon pods of a clique, for example during maintenance, to update it. |
| `RollingUpdate` | The controller restarts whole cliques together, `maxUnavailableCliques` at a time (a number or a percentage, default 1). The next batch starts once each ready node of the updated cliques runs a daemon of the new revision that has been ready for `minReadySeconds` (default 30). Set `paused: true` to stop new batches. |

A rolling update restarts all daemons of a clique together, so each clique is
disrupted once per rollout, not once per node. Nodes that are not ready do not
hold up a rollout, and cliques whose daemons are already unavailable go first.

Leave `spec.image` empty to keep the driver image at creation. Set it to
upgrade the daemons:

```bash
kubectl patch computedomaindaemonset default --type merge \
  -p '{"spec":{"image":"registry.k8s.io/dra-driver-nvidia/dra-driver-nvidia-gpu:v0.6.1"}}'
```

Changing the spec never touches daemons on nodes that keep running one, but
the DaemonSet removes daemons from nodes the spec no longer selects right
away, whatever the update strategy. A `nodeSelector` change that would do
so on nodes with workloads using IMEX channels is held back, with an
`UpdateBlocked` condition naming those nodes, until their workloads are gone.

## Upgrade the driver

Driver upgrades do not change the daemons. When the new driver renders a
different daemon template, the `TemplateOutdated` condition says so, and the
template is applied with the next spec change, typically setting `spec.image`
to the new driver image. Edits to the DaemonSet itself are reverted.

## NVLink partition changes

When the GPUs of a node are moved to another NVLink partition, the kubelet
plugin updates the node's `nvidia.com/gpu.clique` label within a minute. The
controller then restarts the static IMEX daemon of that node right away,
whatever the update strategy, as the change has already disrupted both IMEX
domains. The daemon leaves the clique of its old partition and its new pod
joins the clique of the new one. Workload claims prepared on the node keep
their channel, but their IMEX domain changed with the partition.

## Migrate from per-ComputeDomain daemons

Drain ComputeDomain workloads and delete existing ComputeDomains before you
switch to `scope: cluster`, as for any change of `imex.mode`, `imex.scope`
or `imex.isolation`. Existing ComputeDomains are not migrated, as their
IMEX daemons cannot run beside the static ones. If some are left, the driver
avoids conflicts with their IMEX daemons:

- Static daemons skip nodes that still run a per-ComputeDomain IMEX daemon,
  marked by the `resource.nvidia.com/computeDomain` node label. The kubelet
  plugin removes the label when the last workload of that ComputeDomain on
  the node ends, and the static daemon starts there.
- Deleting a ComputeDomain created before the switch also deletes its
  per-ComputeDomain DaemonSet.

Switching back to `scope: computeDomain` does not delete the static daemons.
The controller reports them with a `Degraded` condition. Delete the
`ComputeDomainDaemonSet` first, as both kinds of daemons cannot run on a
node.

## Conditions

| Condition | Meaning |
|---|---|
| `Ready` | All scheduled daemons are ready. |
| `RolloutInProgress` | Daemons run an outdated revision. |
| `TemplateOutdated` | The driver renders a newer daemon template, applied with the next spec change. |
| `UpdateBlocked` | A spec change that would remove daemons from nodes with workloads is held back. |
| `DeletionBlocked` | Deletion waits for workloads using IMEX channels. |
| `Degraded` | The spec is invalid (`InvalidSpec`), reconciling fails (`ReconcileError`), or the controller does not run static daemons (`StaticDaemonsDisabled`). |

## Delete the daemons

Deleting the `ComputeDomainDaemonSet` waits, with a `DeletionBlocked`
condition, until no `ResourceClaim` is allocated an IMEX channel. The
controller then deletes the DaemonSet, its claim template and its cliques.

Uninstalling the driver leaves the daemons running, so that workloads keep
their IMEX domains. Delete the `ComputeDomainDaemonSet` before uninstalling to
remove them.
