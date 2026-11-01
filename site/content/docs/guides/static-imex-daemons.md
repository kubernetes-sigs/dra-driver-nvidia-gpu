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
        daemonLifecycle: static
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
disrupted once per rollout, not once per node. Nodes that are not ready or
cordoned do not hold up a rollout, and cliques whose daemons are already
unavailable go first.

Large clusters group racks into blocks, or Scalable Units, that share
network switches and usually carry a node label for it. Set
`rollingUpdate.topologyKey` to that label key to update one block at a time.
The controller sorts the values of the label and restarts the cliques of the
first value, `maxUnavailableCliques` at a time. It moves to the next value
once all cliques of the current one run the new revision and are available.
Cliques on nodes without the label go last. `status.updatingTopology` and the
`RolloutInProgress` condition show the block being updated, and
`status.cliques[].topology` the block of each clique. Set `paused: true` to
stop between batches, for example to check the first block.

```bash
kubectl patch computedomaindaemonset default --type merge -p \
  '{"spec":{"updateStrategy":{"rollingUpdate":{"topologyKey":"example.com/block"}}}}'
```

Leave `spec.image` empty to keep the driver image at creation. Set it to
upgrade the daemons:

```bash
kubectl patch computedomaindaemonset default --type merge \
  -p '{"spec":{"image":"registry.k8s.io/dra-driver-nvidia/dra-driver-nvidia-gpu:v0.6.1"}}'
```

### Stalled rolling updates

A clique whose daemons are not available `progressDeadlineSeconds` after
their pods last changed is stalled. The default of 1200 matches the time the
startup probe gives a daemon. A stalled clique keeps its place in the batch,
so that a broken revision reaches at most `maxUnavailableCliques` cliques,
and the rollout waits. The `RolloutInProgress` condition then has
reason `ProgressDeadlineExceeded` and names the stalled cliques, and
`status.cliques[].stalledReason` tells which nodes block each one and why:

```bash
kubectl get computedomaindaemonset default \
  -o jsonpath='{range .status.cliques[?(@.stalledReason)]}{.id}{": "}{.stalledReason}{"\n"}{end}'
```

| Reason | Meaning | To continue |
|---|---|---|
| `ImagePullBackOff`, `ErrImagePull`, `InvalidImageName` | The new image cannot be pulled. | Fix `spec.image`, or set it back to roll back. Stalled cliques run an outdated revision again and are restarted first. |
| `CrashLoopBackOff`, `exited with code` | The new daemon fails to start, for example on an invalid `imexConfig` value. | Fix or roll back the spec as above, and check the daemon logs. |
| `ContainerCreating` | The kubelet cannot start the pod, often as its IMEX channel claim fails to prepare. | Check the pod events and the ComputeDomain kubelet plugin logs on the node. |
| `not scheduled` | The pod does not fit on its node. | Free the resources the message names. |
| `terminating since` | The old pod does not go away, so the DaemonSet does not start the new one. | Check the kubelet on the node, or force delete the pod. |
| `no IMEX daemon pod` | The DaemonSet runs no pod on a node of the clique. | Check the DaemonSet events. |
| `not ready` | The pod runs, but its IMEX daemon does not report ready. | Check the daemon logs, and the NVLink fabric and Fabric Manager of the node. |

To move past a node with a hardware or node-level fault, cordon it. Cordoned
and not ready nodes do not hold up a rollout, so the clique becomes available
without it. Uncordon the node once it is repaired, and its daemon picks up the
current revision when it next restarts. Set `paused: true` to stop new batches
while you investigate.

Errors the controller hits itself, such as failing to delete pods, set the
`Degraded` condition with reason `ReconcileError` and are retried.

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

Apply the `ComputeDomainDaemonSet` CRD of the release before upgrading, as
Helm does not upgrade CRDs. With an older CRD, the API server drops fields it
does not know, such as `topologyKey` or `progressDeadlineSeconds`, and the
controller falls back to their defaults.

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
switch to `daemonLifecycle: static`, as for any change of
`imex.daemonLifecycle`. Existing ComputeDomains are not migrated, as their
IMEX daemons cannot run beside the static ones. If some are left, the driver
avoids conflicts with their IMEX daemons:

- Static daemons skip nodes that still run a per-ComputeDomain IMEX daemon,
  marked by the `resource.nvidia.com/computeDomain` node label. The kubelet
  plugin removes the label when the last workload of that ComputeDomain on
  the node ends, and the static daemon starts there.
- Deleting a ComputeDomain created before the switch also deletes its
  per-ComputeDomain DaemonSet.

Switching back to `daemonLifecycle: perComputeDomain` does not delete the static daemons.
The controller reports them with a `Degraded` condition. Delete the
`ComputeDomainDaemonSet` first, as both kinds of daemons cannot run on a
node.

## Conditions

| Condition | Meaning |
|---|---|
| `Ready` | All scheduled daemons are ready. |
| `RolloutInProgress` | Daemons run an outdated revision. Reason `ProgressDeadlineExceeded` names stalled cliques, see [Stalled rolling updates](#stalled-rolling-updates). |
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
