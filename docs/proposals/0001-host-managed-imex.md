# 0001 — Host-managed IMEX (operator-owned `nvidia-imex`)

| Field          | Value         |
|----------------|---------------|
| Status         | provisional   |
| Authors        | @dims         |
| Created        | 2026-06-01    |
| Related issues | self-managed host IMEX daemon discussions (link to be added) |

## Summary

This proposal introduces an alpha, install-wide `HostManagedIMEX` feature gate
for clusters in which the cluster operator already owns the host `nvidia-imex`
daemon lifecycle. When the gate is enabled, the driver retains the existing
`ComputeDomain` user API and the existing DRA channel-injection path, but no
longer creates the per-`ComputeDomain` in-cluster IMEX DaemonSets. The driver
advertises and injects IMEX channel `0`, while the operator remains responsible
for starting, configuring, and monitoring `nvidia-imex`. The scope is
deliberately narrow: this is an operational mode for operator-owned IMEX, not a
multi-tenant channel-isolation design.

## Motivation

### Who is asking for this, and why?

This is requested by operators of large Multi-Node NVLink (MNNVL) systems —
GB200 NVL72 and comparable platforms — who already run `nvidia-imex` as a host
service (through systemd, BCM, NMX, or an equivalent fabric manager), and who
therefore do not want the DRA driver to launch a second, per-`ComputeDomain`
IMEX daemon alongside it. The concrete request from these consumers is a
"self-managed host daemon as a system service" mode, selectable via a Helm
parameter, in which the user is responsible for the daemon lifecycle and the
driver only advertises and allocates a channel device. At present the only
supported model is driver-managed — the controller creates a
`compute-domain-daemon` DaemonSet for each `ComputeDomain` — which conflicts
with an operator-run host daemon.

### Goals

- Provide a single install-wide feature gate that, when enabled, prevents the
  controller from creating IMEX DaemonSets, daemon `ResourceClaimTemplate`s, the
  daemon `DeviceClass` and RBAC, and `ComputeDomain` node labels.
- Continue to deliver `/dev/nvidia-caps-imex-channels/channel0` to workloads
  through the existing DRA channel claim, with no change to how users author a
  `ComputeDomain` or consume its generated `ResourceClaimTemplate`.
- Constrain the alpha to a single, well-defined shape: one schedulable channel
  device per node (`channel-0`), one prepared channel claim per node, and
  `allocationMode: Single` only.
- Ensure the driver never starts, restarts, or attests to the health of host
  `nvidia-imex`. That responsibility belongs to the operator, and the driver's
  behavior must remain observably independent of imex state.
- Compose cleanly with the existing GPU DRA path (`resources.gpus.enabled`).

Each goal is measurable: the objects the controller creates, the injected device
node, the rejected allocation modes, and the driver's inaction on imex failure
can all be verified directly.

### Non-goals

- Multiple simultaneous, *isolated* `ComputeDomain`s on one host IMEX fabric.
- Assigning unique IMEX channel IDs per `ComputeDomain`.
- `allocationMode: All` (multi-channel injection).
- A scheduler-visible slot model (`slot-0..slot-N`), a channel allocator CRD, or
  a checkpoint schema migration.
- Mandatory admission-webhook enforcement.
- In-place migration between driver-managed and host-managed modes while
  `ComputeDomain` workloads are running.

## Why this belongs in the NVIDIA DRA driver

The behavior under discussion is entirely the driver's concern: which Kubernetes
objects the `compute-domain-controller` creates for a `ComputeDomain`, and which
devices the `compute-domain-kubelet-plugin` publishes and injects. The host
`nvidia-imex` daemon itself is out of scope and remains owned by the operator
(or by the GPU Operator, or by BCM/NMX) below the DRA boundary. The change does
not belong in upstream Kubernetes or DRA core (which are device-agnostic), in
the device plugin, or in the GPU Operator; it is a mode of the existing
ComputeDomain/IMEX coordination layer and therefore belongs in this driver. No
upstream Kubernetes change is required.

## Proposal

### User-facing example

Operators enable a single Helm flag:

```bash
--set featureGates.HostManagedIMEX=true
```

Users author a `ComputeDomain` exactly as they do today, with `numNodes: 0` and
`allocationMode: Single`:

```yaml
apiVersion: resource.nvidia.com/v1beta1
kind: ComputeDomain
metadata:
  name: train-a
spec:
  numNodes: 0
  channel:
    resourceClaimTemplate:
      name: train-a-imex-channel
    allocationMode: Single
```

Workload pods reference the generated `ResourceClaimTemplate` and receive
`/dev/nvidia-caps-imex-channels/channel0` inside the container. A
`status.status` of `Ready` indicates only that the controller has admitted the
`ComputeDomain` and that the workload `ResourceClaimTemplate` exists; it does
**not** assert that host IMEX is healthy.

### Affected components

- [ ] `api/` — CRDs, CRD fields, or ResourceClaim shape *(reused unchanged)*
- [ ] `gpu-kubelet-plugin`
- [x] `compute-domain-kubelet-plugin`
- [x] `compute-domain-controller`
- [ ] `compute-domain-daemon` *(not built or scheduled under the gate)*
- [ ] admission webhook *(intentionally not required; see Alternatives)*
- [x] Helm chart (`deployments/helm`)
- [ ] CDI spec generation *(existing channel-0 edits reused)*
- [ ] Metrics
- [ ] Kubelet-plugin checkpoint schema *(no change)*
- [x] Documentation
- [x] CI / testing

### Authoritative state owner

This proposal introduces no new persistent state. The host `nvidia-imex` daemon
and its `/etc/nvidia-imex/nodes_config.cfg` are authoritative for IMEX fabric
state and are owned by the operator, outside Kubernetes. Within the driver, the
existing owners are unchanged: the kubelet plugin owns the per-node
`ResourceSlice` (which publishes `channel-0`) and the local checkpoint (the
prepared channel claim), and the controller owns the `ComputeDomain` finalizer
and the workload `ResourceClaimTemplate`. The gate only removes write paths
(daemon objects and node labels); it adds none.

### Smallest valuable slice

The smallest shippable increment is the gate together with the following:
suppressing daemon-object creation in the controller; publishing only
`channel-0` in the kubelet plugin; accepting only an empty or `Single`
allocation mode; and rejecting daemon-config prepare. This alone provides the
requested mode. Multi-node scheduling, migration runbooks, and
documentation and examples can follow incrementally.

## Design

### API changes

There are no changes to CRDs or to the `ComputeDomainChannelConfig` opaque
config. The only new user-facing value is the Helm setting
`featureGates.HostManagedIMEX` (default `false`). When the gate is enabled, the
driver would force two compatible gates off *before* the existing dependency
validation runs:

- `IMEXDaemonsWithDNSNames=false`
- `ComputeDomainCliques=false`

This keeps the operator's configuration to a single host-managed flag and avoids
a three-gate combination. The resulting overrides would be logged at controller
and kubelet-plugin startup.

One detail concerns the kubelet plugin. The `ComputeDomain` CRD enum restricts
`allocationMode` to `All` or `Single` for user-created objects, but the opaque
config the kubelet observes is not covered by that enum. Under the gate,
`Prepare` would therefore add an explicit allowlist check (empty or `Single`)
and reject any other value as a permanent error, rather than relying on the
existing `if == "All"` branch alone.

### Feature gate & graduation

- Introduces `HostManagedIMEX` at **Alpha** stability, default `false`, applied
  install-wide.
- The gate is operationally mutually exclusive with driver-managed daemon
  behavior within a release: mixing driver-managed and host-managed
  `ComputeDomain`s is not supported. It composes with the GPU DRA path
  (`resources.gpus.enabled`).
- Graduation to Beta would be evaluated against the project's three criteria:
  - *Feature completeness* — channel-0 advertisement and injection, and daemon
    suppression, behave as designed.
  - *Interoperability* — validated alongside GPU DRA and the GPU Operator
    (containerized driver), and against host IMEX run by systemd, BCM, or NMX.
  - *Stability and soak* — exercised on production-like GB200 fabric long enough
    to surface fabric and restart edge cases.

### Upgrade & downgrade

Because there is no kubelet-plugin checkpoint schema change, in-flight prepared
claims are unaffected by a rolling restart of the driver. Switching a cluster
between driver-managed and host-managed mode is a disruptive, cluster-wide
operation: drain `ComputeDomain` workloads, delete all `ComputeDomain` objects,
change the gate value via Helm, and recreate the `ComputeDomain`s. The driver
would not automatically adopt or remove daemon objects left over from a previous
mode; the migration runbook covers that cleanup.

### Environment floor

- GPU generations: MNNVL fabric systems on which IMEX channels exist (GB200
  NVL72 and comparable B200/GB-class fabrics).
- NVIDIA driver: a version recent enough to register the
  `nvidia-caps-imex-channels` device major and provide channel 0. The existing
  ComputeDomain floor (driver ≥ 570.158.01) applies to the channel device even
  though `IMEXDaemonsWithDNSNames` is forced off.
- Host `nvidia-imex.service` configured and **running** (not masked), with a
  consistent `nodes_config.cfg` across the fabric — the inverse of the
  driver-managed requirement.
- Kubernetes ≥ 1.34 (`resource.k8s.io/v1` DRA), with CDI enabled in the runtime.

### Test plan

The validation plan spans more than unit tests:

- **Unit** (`pkg/featuregates`, `cmd/compute-domain-controller`,
  `cmd/compute-domain-kubelet-plugin`): gate registration and the force-off
  override; channel prepare rejecting `All` and unknown opaque modes via the new
  allowlist; daemon-config prepare returning a permanent error; an empty clique
  ID returning a permanent error; the `ResourceSlice` omitting the daemon
  device; the controller add path creating only the workload RCT and reporting
  `Ready`; the delete path removing the RCT and finalizers and forgetting
  metrics; and unprepare skipping node-label removal.
- **Integration (no GPU):** rendering the Helm chart with the gate on and off,
  and asserting that the channel `DeviceClass` renders, that the daemon
  `DeviceClass` and daemon RBAC do not, and that `FEATURE_GATES` is plumbed to
  the controller and kubelet plugin.
- **BATS (`tests/bats`):** a host-managed overlay of the channel-injection spec
  (asserting channel-0 injection with no in-cluster daemon and no daemon
  objects), and a host-managed variant of the MNNVL workload
  (`test_cd_mnnvl_workload.bats`'s nvbandwidth) that asserts the
  `SUM multinode_device_to_device_memcpy_read_ce` line with operator-run host
  imex.
- **Mock NVML CI (`hack/ci/mock-nvml`):** exercising the gate behaviors, the
  `ResourceSlice` shape, and the prepare error paths (including the empty-clique
  case) on CPU-only runners, since the mock provides the
  `nvidia-caps-imex-channels` major.
- **Lambda / real-GPU e2e:** on a `*gb200*` selection, validating channel-0
  injection end-to-end and a cross-node IMEX workload, and confirming that
  stopping host imex triggers no driver action and surfaces a CUDA error in the
  workload.

## Risks

- **Host IMEX health is not visible to Kubernetes.** A pod may start with
  channel-0 injected while host imex is down or misconfigured, in which case
  cross-node CUDA operations fail at runtime. The driver deliberately takes no
  corrective action, so operators must monitor imex out of band. This is an
  intentional part of the contract, but it relocates a failure mode to the
  operator and must be documented clearly in the user guide and release notes.
- **No isolation.** Every host-managed `ComputeDomain` uses channel `0`, so two
  isolated jobs on the same fabric are not actually isolated. The operational
  rule is that at most one active, isolated `ComputeDomain` should run per host
  IMEX domain.
- **Leftover objects after a mode change.** Skipping the documented migration
  (deleting all `ComputeDomain`s first) can leave stale driver-managed objects
  behind.
- **Privilege surface.** This is unchanged from the existing channel path; the
  kubelet plugin already injects the channel character device via CDI.
- **Fail-fast behavior.** The kubelet plugin must continue to fail cleanly when
  the channel major is absent at startup; the gate must not weaken that.

## Alternatives

- **A full channel allocator** (an `IMEXChannelAllocation` CRD with a reaper,
  abstract `slot-*` devices, clique-wide optimistic concurrency, a checkpoint
  V3, and live admission lookups) would enable genuine multi-tenant isolation,
  but it introduces a large API, status, RBAC, and lifecycle surface. It is
  deferred to a separate proposal, which this alpha intentionally avoids.
- **A mandatory admission webhook** (rejecting unsupported claim shapes at
  admission) was considered and set aside for the alpha: it would require
  cert-manager, a chart `fail` guard, a config-schema change (a
  `(namespace, name, domainID)` triple), and live-`Get` RBAC. Host-managed
  safety can be enforced entirely within the controller and kubelet plugin. An
  optional, non-mandatory webhook that mirrors the plugin allowlist (to provide
  clearer errors at `kubectl apply` time) is a possible Beta follow-up.
- **The status quo (driver-managed only)** does not serve operators who run host
  `nvidia-imex`, because the driver's daemons collide with the host daemon.
- **Reuse of existing primitives.** The existing `channel-0` device, the
  `compute-domain-default-channel.nvidia.com` `DeviceClass`, workload RCT
  rendering, checkpoint V2, and CDI prepare/unprepare are reused unchanged; the
  gate suppresses the daemon-side paths rather than introducing new machinery.

## Drawbacks

- The `status` semantics are weak: a `Ready` status says nothing about IMEX
  health, which may surprise users accustomed to the driver-managed readiness
  signal.
- Operator burden increases: `nodes_config.cfg` consistency, channel-0
  availability, and imex restarts all become the operator's responsibility.
- The gate is install-wide only — there is no per-namespace or
  per-`ComputeDomain` mode selection, so a single cluster cannot mix
  driver-managed and host-managed jobs.

## Open questions

- Should a future revision allow per-namespace or per-`ComputeDomain` mode
  selection rather than install-wide configuration?
- Is the optional, non-mandatory admission webhook worth adding for feedback at
  `kubectl apply` time, or is Prepare-time rejection sufficient?
- What is the appropriate graduation bridge to a genuine multi-tenant channel
  allocator? Can host-managed mode and an allocator coexist, or would the
  allocator subsume this gate?
