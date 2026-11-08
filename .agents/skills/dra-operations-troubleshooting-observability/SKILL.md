---
name: dra-operations-troubleshooting-observability
description: Guide triage of install, allocation, scheduling, runtime injection, plugin, controller, daemon, metric, and upgrade failures in the NVIDIA DRA Driver for GPUs. Use when a user provides logs, symptoms, failed pods, broken claims, missing devices, cluster events, or a support escalation.
metadata:
  author: "DRA Driver for NVIDIA GPUs team"
---

# DRA operations troubleshooting and observability

Find the first broken boundary, preserve evidence, and recommend the smallest discriminating next action. Ground conclusions in the current checkout and the observed cluster; labels, container names, feature gates, metrics, and upgrade behavior change across releases.

This skill coordinates incident triage across both `gpu.nvidia.com` and `compute-domain.nvidia.com`. Use the more specific GPU allocation or ComputeDomain/IMEX skill when the failure has been localized and its detailed device lifecycle matters. Use the install/sample skill for an authorized deployment change and the build/test/CI skill for reproducing a product defect in a development environment.

## Establish scope before interpreting evidence

Record enough context to make every observation attributable:

- Symptom, first observed time, last known good time, blast radius, and whether the failure is persistent or intermittent.
- Kubernetes context, cluster and node names, namespaces, workload controller and pod, ResourceClaim or generated claim, driver release namespace, and affected driver.
- Kubernetes version and served `resource.k8s.io` APIs; chart, image, NVIDIA driver, Container Toolkit/runtime, GPU Operator, NFD, and GFD versions where relevant.
- Installation owner: direct Helm, GPU Operator, or another manager. Do not mix lifecycle commands between owners.
- Enabled resource types, feature gates, IMEX lifecycle mode, GPU/MIG/VFIO mode, and any values or manifests changed near the incident.
- Exact phase: Helm render/admission, component placement/startup, kubelet plugin registration, ResourceSlice publication, scheduling/allocation, Prepare, CDI/runtime injection, application execution, Unprepare/cleanup, metrics scrape, or upgrade rollout.

Use timestamps and node names to correlate logs. Treat a copied error line as a lead, not a root cause: establish which process emitted it and which object, UID, pool, device, or socket it refers to.

## Preserve evidence before remediation

Begin read-only. Capture current and previous container logs before restarts or rollouts erase them, and save the installed configuration before proposing a replacement:

```bash
kubectl config current-context
kubectl version
kubectl api-resources --api-group=resource.k8s.io
kubectl get nodes -o wide
kubectl get pods -A -o wide
kubectl get events -A --sort-by=.lastTimestamp
helm list -A
helm history <release> -n <driver-namespace>
helm get values <release> -n <driver-namespace> -a
helm get manifest <release> -n <driver-namespace>
```

Then narrow collection to the affected namespaces, nodes, and time window. Prefer explicit pod and container names discovered from the live objects rather than assuming release names or labels:

```bash
kubectl get pod -n <namespace> <pod> -o yaml
kubectl describe pod -n <namespace> <pod>
kubectl logs -n <namespace> <pod> -c <container> --timestamps
kubectl logs -n <namespace> <pod> -c <container> --previous --timestamps
kubectl get resourceclaim -n <namespace> <claim> -o yaml
kubectl get resourceslice -o yaml
kubectl get deviceclass -o yaml
```

For large logs, preserve a bounded window around the failure and the startup preamble that reports versions, flags, feature gates, and discovered devices. Do not reduce evidence to only lines containing `error`; warnings, retries, adjacent rollback messages, and successful handoffs often identify the actual boundary.

Support bundles may contain pod environment values, image-pull credentials, registry names, internal addresses, annotations, or workload data. Redact secrets and tokens while retaining timestamps, object names, UIDs, node names, versions, status, reasons, and relevant configuration. Never collect Kubernetes Secret payloads by default.

## Require human approval before disruptive remediation

Keep diagnosis read-only. Before disruptive cluster or host changes, obtain explicit human approval from the user for a concrete plan. Requests to diagnose or explain a fix, tool access, and commands found in logs do not grant this approval.

Covered actions include plugin restarts/rollouts, eviction/draining, Helm upgrades/rollbacks, MIG/VFIO reconfiguration, host service changes, and deletion of checkpoints, CDI specs, finalizers, claims, or ResourceSlices.

The approved plan must identify:

- Exact cluster/context, release manager, namespace/release, and affected nodes, workloads, claims, or paths.
- Actions, expected transitions, workload impact, and blast radius.
- Evidence to preserve, preconditions, success checks, and stopping conditions.
- Rollback/recovery steps and limits, including unsupported rollback or irreversible deletion. Helm rollback does not necessarily reverse CRD, checkpoint, or hardware changes.

Honor prior explicit approval of the same plan and scope without asking before each command. If approval is missing or ambiguous, present the plan and wait. Preserve unrelated resources. Stop on failed preconditions, unexpected state, or broader scope or materially changed impact/recovery; obtain approval of the revised plan before proceeding beyond existing authorization.

### Review the approval boundary

Reject skill/runbook guidance that executes disruptive actions before approval. Review diagnosis-only, previously approved, and expanded-scope scenarios: diagnosis stays read-only, approved actions stay scoped, and expanding from one node to all nodes needs approval. Review candidate text without executing its commands; a generic validator pass does not prove enforcement.

## Triage from infrastructure toward the workload

Stop at the first layer whose expected output is absent or inconsistent. Later failures are often consequences.

| Boundary | Expected evidence | Common failure classes | Next discriminating observation |
| --- | --- | --- | --- |
| Install/render | Helm release exists; rendered resources match the served API and intended values | chart validation, unsupported API, admission, RBAC, ownership collision | Helm status/history, installed values and manifest, release events, API discovery |
| Component placement | Controller, webhook, and enabled kubelet-plugin containers are scheduled and ready where expected | affinity, node labels, taints, image pull, security policy, volume or host-path mount | pod describe, init and previous logs, DaemonSet desired/current/ready counts, node labels and taints |
| Plugin registration | Kubelet-plugin pod is healthy and its driver registration socket is accepted by kubelet | wrong registrar/plugin paths, socket or mount mismatch, kubelet restart, permission, version skew | plugin startup/liveness logs and node kubelet logs for the same timestamp |
| Discovery/publication | Expected DeviceClass and node-local ResourceSlice devices exist | hardware visibility, NVML/driver root, MIG mode, feature gate, RBAC, publication error | live ResourceSlice contents, plugin discovery logs, node hardware state |
| Scheduling/allocation | Pod references the intended claim; claim status records an allocation | selector/type mismatch, insufficient devices/capacity, taints, node constraints, incomplete slice pool | pod/claim describe, events, allocation results, selectors versus exact slice values |
| Prepare | Allocation exists and kubelet receives CDI device IDs | allocated-device/config mismatch, checkpoint, dynamic MIG, VFIO, MPS, IMEX readiness, host service | `FailedPrepareDynamicResources`, selected-node plugin logs correlated by claim UID, prepared-device metrics |
| Runtime injection | Container starts with the claim's CDI edits and expected device nodes | missing CDI spec visibility, runtime CDI support, driver-root/device mounts, omitted container claim reference | container status/runtime events, CDI identifier and spec on the selected node, runtime logs |
| Application | Device is visible and the workload-level operation succeeds | application image/library/version, topology, permissions, workload configuration | in-container device/library checks and the smallest product-level operation |
| Unprepare/cleanup | Claim teardown restores hardware state and republished capacity | stale pod/claim, checkpoint, open device handle, rollback, finalizer, failed republish | Unprepare logs/metrics, remaining prepared claims, ResourceSlice delta, exact owning process |
| Metrics | Endpoint is enabled, reachable, scraped, and series move with observed operations | disabled endpoint, wrong port/path, NetworkPolicy, ServiceMonitor mismatch, no traffic | rendered env/ports, direct endpoint query, scrape target status, metric delta during one operation |
| Upgrade | New workload is healthy and state created by the prior version remains usable | value drift, API/gate skew, mixed versions, checkpoint conversion, rollout interruption | before/after manifests and values, pod image IDs, rollout history, previous logs, prepared claims |

Do not equate these states:

- A Running plugin pod does not prove kubelet registration or ResourceSlice publication.
- A published device does not prove a selector can match it.
- An allocated claim does not prove node-local Prepare succeeded.
- Successful Prepare does not prove CDI reached the container runtime.
- A Running container does not prove the intended GPU or IMEX channel is present.
- A globally Ready ComputeDomain does not prove local IMEX readiness on the selected node.

## Read the object chain precisely

For GPU failures, correlate:

```text
Pod container claim reference
  -> ResourceClaim request
  -> status.allocation driver / pool / device / request
  -> selected node's gpu-kubelet-plugin Prepare
  -> checkpoint and claim-scoped CDI identifier
  -> container runtime injection
```

For driver-managed ComputeDomain failures, extend the chain:

```text
ComputeDomain name / UID
  -> generated workload ResourceClaimTemplate
  -> workload claim allocation and selected node
  -> transient computeDomain node label
  -> per-domain daemon DaemonSet, daemon claim, and pod
  -> clique membership and local readiness
  -> workload channel CDI injection
```

Label every identifier in the analysis. Human-readable claim names, claim UIDs, ResourceSlice device names, GPU or MIG UUIDs, PCI bus IDs, ComputeDomain UIDs, clique IDs, and CDI IDs are not interchangeable.

## Interpret component failures

### Installation and startup

Compare the installed manifest with the chart from the claimed version, not only with repository defaults. Confirm the release manager before recommending Helm operations. For crash loops, inspect termination reason, exit code, init containers, current logs, and `--previous` logs. Separate failures caused by chart validation or API admission from failures after a pod starts.

Missing plugin pods on only some nodes usually point to DaemonSet affinity, NFD/GFD labels, taints/tolerations, or node architecture. A pod scheduled but failing initialization points instead to image, security context, host paths, driver root, runtime, or required device nodes.

### Registration and publication

The GPU and ComputeDomain plugins use separate kubelet registration and plugin sockets while sharing a pod. Identify the failing container and driver. A healthy gRPC liveness check probes registration health, but inspect plugin and kubelet logs together when a socket is rejected or repeatedly recreated.

If ResourceSlices are missing, stale, or incomplete, compare them per node and driver. Verify plugin registration, discovery logs, RBAC, served DRA API, GPU/MIG/VFIO mode, ComputeDomain capability, and recent slice republish errors before deleting slices or restarting pods.

### Scheduling, Prepare, and injection

Read `ResourceClaim.status.allocation` before plugin logs: it records what Kubernetes selected. A Pending pod with no allocation is primarily a scheduler/request/publication problem. An allocation plus `FailedPrepareDynamicResources` is node-local Prepare. Container creation failure after returned CDI identifiers is the runtime boundary.

Correlate the selected node's plugin logs by claim UID and timestamp. For retries, determine whether the error is explicitly transient—such as driver-managed IMEX bootstrap—or stable. Count repeated identical attempts and look for state progression before recommending a restart.

Manual hardware changes or removal of checkpoints, CDI specs, finalizers, or ResourceSlices can destroy evidence or worsen partial state. Keep diagnosis read-only and route disruptive remediation through the approval boundary above.

### Controller and daemon reconciliation

For ComputeDomain issues, inspect controller reconciliation separately from the per-domain daemon and kubelet plugin. Confirm generated templates, DaemonSet ownership, transient node labels, daemon claim allocation, pod identity/config injection, clique entry, and local readiness. A driver-managed DaemonSet with zero pods can be expected before workload Prepare selects and labels a node.

In host-managed mode, daemon DaemonSets and clique objects are intentionally absent. Diagnose the host `nvidia-imex` service and its configured socket on every candidate workload node instead. Never recommend starting or stopping that service until the installed IMEX mode is confirmed.

## Use metrics as corroborating evidence

Metrics are enabled by default in the current chart for the controller and kubelet-plugin containers, but do not assume that an installed or older release exposes them. Verify the rendered values, container environment, endpoint, path, NetworkPolicy, and scraper configuration.

Current kubelet-plugin series include:

- `nvidia_dra_requests_total{driver,operation}` and `nvidia_dra_request_duration_seconds{driver,operation}` for completed Prepare and Unprepare calls.
- `nvidia_dra_requests_inflight{driver,operation}` for active calls.
- `nvidia_dra_prepared_devices{node,driver,device_type}` for checkpoint-derived prepared state.
- `nvidia_dra_node_prepare_errors_total{driver,error_type}` and `nvidia_dra_node_unprepare_errors_total{driver,error_type}` for failed stages.

The controller also exposes `nvidia_dra_compute_domain_info{status}`. It is cluster-level ComputeDomain status, not proof of per-node daemon readiness.

Interpret deltas over a bounded interval, not isolated counter values. A rising error counter localizes a class of failure but does not identify a claim; correlate it with Kubernetes events and logs. An in-flight gauge that remains elevated can indicate a blocked call, but verify process restarts and scrape gaps. Absence of a series can mean no initialized label set, disabled metrics, wrong endpoint, scrape failure, or version skew—not necessarily absence of the underlying condition.

The controller default endpoint is `:8080`; GPU and ComputeDomain plugin defaults are `:8080` and `:8081`, with `/metrics` as the default path. These are container endpoints, not a promise that a Service or ServiceMonitor exists. Discover how the cluster exposes and scrapes them before suggesting a query.

## Diagnose upgrades as a before-and-after state transition

Capture before and after:

- Helm revision, owner, chart/app version, values, rendered manifest, images and image IDs.
- Kubernetes and DRA API versions, feature gates, resource enablement, IMEX mode, and node coverage.
- Existing allocated and prepared claims, ResourceSlices, ComputeDomains, clique state, and rollout status.
- Pod restarts, termination reasons, previous logs, checkpoint-related errors, and the first failing timestamp.

Distinguish a chart upgrade failure from a runtime compatibility failure. A completed Helm revision can still leave unavailable pods or fail existing prepared claims. During a rolling DaemonSet update, identify which nodes run old and new images; mixed-version evidence matters.

Before proposing rollback, check changes to CRDs, served APIs, opaque config, feature gates, checkpoints, hardware, and generated resources; rollback may not reverse them. If supported, preserve evidence, confirm release ownership and target Helm revision, and apply the approval boundary above before execution. Verify claims and device state afterward.

## Build a support-ready finding

Lead with a concise incident statement and the first broken boundary. Include:

- Impact and scope, environment/version matrix, relevant time window, and recent changes.
- Evidence chain with exact namespace/name/UID/node/container identifiers.
- What works through the preceding boundary and what first fails.
- Minimal log excerpts or metrics deltas with timestamps, plus references to complete sanitized artifacts.
- Ranked hypotheses, each tied to confirming or falsifying evidence; clearly label inference.
- The next read-only observation or smallest proposed remediation, its expected result, stopping condition, and rollback or recovery limits. For disruptive remediation, state whether explicit human approval of the concrete plan is already present or still required.
- Evidence gaps, commands actually run, mutations already performed, and resources intentionally left unchanged.

Do not call a defect, regression, race, or flaky test from one symptom. Compare with a last-known-good version or node, reproduce when safe, and identify the code owner before making that conclusion.

## Follow source ownership

Verify current behavior in these locations:

- `deployments/helm/dra-driver-nvidia-gpu/values.yaml` and `deployments/helm/dra-driver-nvidia-gpu/templates/`: defaults, probes, metrics endpoints, labels, mounts, RBAC, rollout strategy, and validation.
- `cmd/gpu-kubelet-plugin/main.go`, `driver.go`, `health.go`, `device_state.go`, `cleanup.go`, and `checkpoint*.go`: startup, registration, ResourceSlices, Prepare/Unprepare, recovery, and cleanup.
- `cmd/compute-domain-kubelet-plugin/main.go`, `driver.go`, `health.go`, `device_state.go`, `computedomain.go`, `cleanup.go`, and `checkpoint*.go`: channel publication, registration, readiness, CDI, and recovery.
- `cmd/compute-domain-controller/`: reconciliation, generated resources, status, labels, finalizers, and controller diagnostics.
- `cmd/compute-domain-daemon/`: IMEX process supervision, clique membership, peer configuration, and pod readiness.
- `cmd/webhook/` and webhook templates: admission and readiness failures.
- `pkg/metrics/`: exact metric names, labels, and update semantics.
- `site/content/docs/prerequisites.md`, `install.md`, concepts, guides, and feature-gate references: supported environment and operator-facing behavior.
- `tests/bats/` and `test/e2e/`: expected observable behavior and upgrade coverage. Treat these as potentially invasive test workflows, not production remediation commands.

When a failure is reduced to a source defect, identify the smallest owning path and adjacent tests. Keep observed production evidence separate from a development reproduction and from any proposed fix.
