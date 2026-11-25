---
name: dra-helm-demo-sample-workloads
description: Guide direct and GPU Operator-managed deployment and upgrades, kind demos, sample manifest authoring, and quickstart validation for the NVIDIA DRA driver. Use when a user asks to install, upgrade, or demo the driver, create a resource.k8s.io ResourceClaim or ResourceClaimTemplate workload, share one claim across containers, prove the same GPU UUID, exercise the repository's kind workflow, or validate DeviceClass, ResourceSlice, claim, and container allocation evidence.
metadata:
  author: "DRA Driver for NVIDIA GPUs team"
---

# Helm, demos, and sample workloads

Match the sample to the cluster and verify GPU allocation from Kubernetes objects to containers. Keep rendering separate from live changes.

## Start by establishing the target

Check the repository and target environment for:

- Whether the user wants instructions, rendered YAML, a released-chart install, an existing-release upgrade, a local-chart install, or a disposable kind demo.
- The exact kube context, cluster, release name, namespace, chart version, and values file.
- Current or planned GPU Operator ownership of the node stack and DRA driver (see Operator guidance below).
- The Kubernetes version and served `resource.k8s.io` API versions. Prefer the highest served version in this order: `v1`, `v1beta2`, `v1beta1`.
- NVIDIA driver, Container Toolkit, CDI, NFD/GFD, node labels, GPU hardware, and relevant feature-gate readiness.
- The driver root: normally `/` for a host-installed driver, `/run/nvidia/driver` for a GPU Operator driver container, and the current documented GKE path for GKE. Verify rather than copying an older demo script.
- Whether the standard NVIDIA device plugin is already exposing the same GPUs.

Use `site/content/docs/prerequisites.md`, `site/content/docs/install.md`, `site/content/docs/upgrade.md`, and the relevant release's chart values as the authority. If the root README or an older demo script disagrees, call out the difference and follow the current documentation and implementation.

Read-only discovery can include:

```bash
kubectl config current-context
kubectl version
kubectl api-versions
kubectl get nodes -o wide
kubectl get nodes --show-labels
helm list --all-namespaces
```

Do not run `helm install` or `upgrade`, `kubectl apply`, label changes, kind creation/deletion, GPU reconfiguration, or service changes merely because the user asks how to do them. Those mutate external state. Execute them only when the user has asked for the change and the target is unambiguous. Treat cluster deletion and broad namespace cleanup as destructive operations requiring exact scope.

## Choose the workflow

| Goal | Primary path |
| --- | --- |
| Inspect or review output | Render the local chart and validate manifests without persisting them |
| Install or manage the driver with GPU Operator | Follow the GPU Operator project's DRA guidance and operator-specific agent skills when available |
| Install a released driver | Use the versioned OCI chart from the current install guide |
| Upgrade an existing directly managed Helm release | Follow the existing-release workflow below and the source-to-target procedure in `site/content/docs/upgrade.md` |
| Test local chart changes on an existing cluster | Install `deployments/helm/dra-driver-nvidia-gpu` with an explicit context, namespace, and values |
| Run a local development demo | Use `demo/clusters/kind/` after confirming Docker, NVIDIA runtime, host GPU access, and cleanup scope |
| Demonstrate allocation only | Select or author the smallest matching manifest under `demo/specs/quickstart/` |

## Route GPU Operator-managed deployments

GPU Operator can install and manage the DRA driver.

For Operator enablement, configuration, compatibility, upgrades, and troubleshooting, follow the [GPU Operator DRA installation guide](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/dra-intro-install.html) and the [NVIDIA GPU Operator project](https://github.com/NVIDIA/gpu-operator). Follow relevant skills in the Operator repository when available. Use this skill for samples and allocation checks.

Confirm ownership of the DRA release and related components before proposing install, upgrade, rollback, or cleanup. Use direct Helm commands on Operator-managed deployments only when Operator documentation explicitly requires them.

## Work with the Helm chart

Inspect these paths before proposing values or diagnosing output:

- `deployments/helm/dra-driver-nvidia-gpu/Chart.yaml`
- `deployments/helm/dra-driver-nvidia-gpu/values.yaml`
- `deployments/helm/dra-driver-nvidia-gpu/templates/`
- `site/content/docs/install.md`
- `site/content/docs/upgrade.md`

The chart requires Kubernetes 1.32 or newer and detects the highest supported DRA API from cluster capabilities. `resourceApiVersion` is an escape hatch for inaccurate capability discovery, not a default setting. The chart also rejects installation into the `default` namespace unless `allowDefaultNamespace=true` is explicitly chosen.

Current values enable GPU and ComputeDomain resource plugins. Enabling GPU resources requires the deliberate acknowledgement below because a standard NVIDIA device plugin can collide with DRA management of the same GPUs:

```bash
--set gpuResourcesEnabledOverride=true
```

Explain this acknowledgement; do not present it as harmless boilerplate.

For a new released install, derive the exact version from the user's target or the current install documentation. If the release already exists, use the upgrade workflow below:

```bash
helm install dra-driver-nvidia-gpu \
  oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
  --version <version> \
  --namespace dra-driver-nvidia-gpu \
  --create-namespace \
  --set gpuResourcesEnabledOverride=true \
  --wait
```

For local chart work, replace the OCI reference with `./deployments/helm/dra-driver-nvidia-gpu`. Add only values justified by the inspected environment, such as `nvidiaDriverRoot` or selectively disabling a resource plugin.

Before a live install, prefer a render-and-review pass:

```bash
helm lint deployments/helm/dra-driver-nvidia-gpu
helm template dra-driver-nvidia-gpu deployments/helm/dra-driver-nvidia-gpu \
  --namespace dra-driver-nvidia-gpu \
  --api-versions resource.k8s.io/v1 \
  --set gpuResourcesEnabledOverride=true
```

Choose the `--api-versions` value to model the target cluster. A server-side dry run contacts the cluster and exercises its API and admission without persisting objects:

```bash
kubectl apply --dry-run=server -f <manifest>
```

## Upgrade an existing release

Confirm release ownership. Upgrade Operator-managed releases through the Operator. For direct Helm releases, follow the steps below.

### Capture the current state

Confirm context, release name, namespace, installed chart and image versions, and target chart version. Keep the existing release identity.

Inspect the Helm release and cluster:

```bash
helm --kube-context <context> status <release> --namespace <namespace>
helm --kube-context <context> history <release> --namespace <namespace>
kubectl --context <context> get pods -n <namespace> -o wide
kubectl --context <context> get deviceclasses
kubectl --context <context> get resourceslices -o wide
kubectl --context <context> get resourceclaims -A -o yaml
```

Identify the latest attempted revision and the last successful revision in Helm history.
For each distinct revision, save its user-supplied values, computed values, and manifest in a protected location:

```bash
helm --kube-context <context> get values <release> -n <namespace> --revision <revision> -o yaml
helm --kube-context <context> get values <release> -n <namespace> --revision <revision> --all -o yaml
helm --kube-context <context> get manifest <release> -n <namespace> --revision <revision>
```

After a failed upgrade, compare both revisions with the live Deployments, DaemonSets, and pods before choosing values such as `nvidiaDriverRoot`.
If no successful revision is available, record that gap and verify the baseline from the live installation.
Use computed values to diagnose differences. Carry forward only the user overrides that still apply.
Record healthy workloads and claims, their allocations and selected nodes, and the current ComputeDomain/IMEX mode when enabled.

### Review the source-to-target transition

Read `site/content/docs/upgrade.md` and both releases' notes for the exact transition. Use the target release's docs and chart sources. Report undocumented transitions or unresolved compatibility before upgrading.

Check Kubernetes and served DRA API compatibility, NVIDIA driver/Container Toolkit requirements, feature gates, enabled plugins, Helm value changes, and checkpoint/API downgrade restrictions. Determine any required intermediate releases or workload maintenance steps. Preserve existing release/resource names and overrides where the transition requires them; do not substitute the new-install names.

When the transition requires CRD updates, inspect the exact target chart's definitions with `helm show crds <chart> --version <target-chart-version>` (omit `--version` for a local chart).
For each definition, compare its `spec` with `kubectl --context <context> get crd <name> -o yaml`, including schema and served or storage version changes. Record any missing definition.
Review these separately from the release manifest. Helm does not upgrade existing CRDs from `crds/`, and Helm rollback does not revert a separately applied CRD.
If changing ComputeDomain IMEX mode or isolation, include the guide's workload-drain and ComputeDomain-removal steps in the plan.

Build a reviewed values file from the overrides verified against the live installation and intentional target-version changes. Retain required settings such as driver root, resource enablement, image overrides, and naming overrides after checking their target-version semantics. If the transition guide calls for `--reuse-values`, verify which values Helm will reuse before choosing that strategy.

Preview the upgrade against the target cluster using the same release, chart version, namespace, and values strategy as the planned upgrade.
Local `helm template --api-versions` can select a newer DRA API than the cluster serves.
To review the manifest while suppressing Kubernetes Secrets:

```bash
set -o pipefail
helm --kube-context <context> upgrade <release> \
  oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
  --version <target-chart-version> \
  --namespace <namespace> \
  --values <reviewed-values.yaml> \
  --dry-run=server --hide-secret --output json | jq -r '.manifest'
```

Compare the preview with the captured manifest and live resources. Review resource names, DRA API versions, RBAC, images, mounts, feature gates, and rollout behavior.
Keep captured output private and redact sensitive values before sharing.
Before execution, explain the expected workload impact, stopping conditions, and whether rollback is supported.

### Execute only the authorized upgrade

An instructions-only request does not authorize a cluster change.
If the upgrade is remediation for a failure, follow the [troubleshooting approval procedure](../dra-operations-troubleshooting-observability/SKILL.md#require-human-approval-before-disruptive-remediation) before the first change, including a drain or CRD update.
Obtain explicit approval for the affected workloads, planned actions, impact, stopping conditions, and recovery or rollback limits. Honor prior approval of the same plan and scope. If scope, impact, or recovery changes materially, stop for approval of the revised plan.

Once authorized for the identified target, perform the release guide's prerequisites and any required CRD updates in the documented order, then upgrade the release.
For a compatible directly managed release using reviewed values:

```bash
helm --kube-context <context> upgrade <release> \
  oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
  --version <target-chart-version> \
  --namespace <namespace> \
  --values <reviewed-values.yaml> \
  --wait \
  --timeout <timeout>
```

Use the discovered release and namespace. Omit `--install` so a missing or mistyped release does not silently become a fresh installation. For a requested local-chart upgrade, replace the OCI reference with the inspected local chart, omit `--version`, and verify its chart/app versions and image availability.

### Verify continuity after the upgrade

Check Helm status and the new revision, then inspect rollout completion for the actual Deployments and DaemonSets discovered in the release. Confirm the expected chart/images are running and ready on the intended nodes, DeviceClasses and ResourceSlices remain available, and previously allocated claims retain their expected allocations and workload reservations. Verify existing workloads remain healthy and retain GPU or IMEX access; a Helm `deployed` status alone is insufficient. Run a new smoke workload only within the user's requested scope.

If the rollout or workload checks fail, preserve current and previous component logs, events, image/version information, and claim evidence before further remediation. Stop additional changes and report the first failing boundary. Propose rollback only after checking the source/target checkpoint, CRD, API, and configuration compatibility; do not automatically roll back, uninstall, delete claims, or remove checkpoints.

At handoff, report source and target versions, previous and new Helm revisions, context/release/namespace, values and CRD changes, observed rollout and workload evidence, rollback limitations, and any checks not performed.

## Use the kind demo deliberately

The repository's local workflow is:

```bash
export KIND_CLUSTER_NAME=kind-dra-1
./demo/clusters/kind/build-dra-driver-gpu.sh
./demo/clusters/kind/create-cluster.sh
./demo/clusters/kind/install-dra-driver-gpu.sh
```

Review each script before running it. The build path invokes image builds and generation targets and can change generated files. Cluster creation pulls or builds a kind node image, mounts host GPU/runtime paths, and creates a retained cluster. Installation labels worker nodes and performs a waiting Helm upgrade/install. The host must already support the NVIDIA container runtime and the mounts described in the kind configuration. The `nvkind` workflow delegates to the kind scripts and may mask NVIDIA driver parameters.

Name the cluster explicitly and preserve it until validation is complete. Offer the matching scoped delete script afterward, but never run it automatically:

```bash
./demo/clusters/kind/delete-cluster.sh
```

## Select or create a sample workload

First inspect the APIs the cluster serves. Use `demo/specs/quickstart/v1/` for `resource.k8s.io/v1` and `demo/specs/quickstart/v1beta1/` only for a `v1beta1` cluster. The repository may not contain an example directory for every API the chart supports; if the cluster serves only `v1beta2`, adapt a minimal manifest to that schema and validate it server-side instead of applying a mismatched example.

Use the smallest example that proves the requested behavior:

- `gpu-test1`: separate claims allocate GPUs to two pods.
- `gpu-test2`: two containers in one pod share one claim and should see the same GPU UUID. This is the preferred basic smoke test.
- `gpu-test3`: two pods share one global claim.
- `gpu-test4`: MIG-profile allocation; requires compatible hardware and configuration.
- `gpu-test5` or `gpu-test-mps`: time-slicing or MPS behavior; requires the corresponding configuration and feature gates.
- `gpu-test6`: specialized A100/CEL behavior, not a generic quickstart.

When authoring a new basic sample:

1. Use a unique Namespace so apply, observation, and cleanup stay scoped.
2. Use the served `resource.k8s.io` API version.
3. Request the `gpu.nvidia.com` DeviceClass through a `ResourceClaimTemplate` or `ResourceClaim`.
4. Map the claim into the Pod's `resourceClaims`, then list that claim under every consuming container's `resources.claims`.
5. Use a simple command such as `nvidia-smi -L`, followed by sleep only when logs need to remain inspectable.
6. Add GPU-taint tolerations only when the target nodes require them.
7. Inspect actual ResourceSlice attributes before writing CEL selectors, and qualify driver attributes with `gpu.nvidia.com/`.
8. Do not add opaque sharing configuration for the basic two-container shared-claim case. Time slicing and MPS are separate, opt-in tests.

Keep sample changes close to the existing versioned quickstarts and follow their naming and labeling conventions. If the requested behavior changes APIs, opaque configuration, or feature gates, treat that as a compatibility-sensitive design task rather than a simple sample edit.

## Validate from installation to container

Check each stage in order.

1. Confirm driver components are ready:

   ```bash
   kubectl get pods -n dra-driver-nvidia-gpu -o wide
   kubectl get deviceclasses
   kubectl get resourceslices -o wide
   ```

   Expect the relevant controller and kubelet plugin pods, DeviceClasses for enabled resources, and per-node ResourceSlices from the NVIDIA driver.

2. Apply the chosen sample only after reviewing its namespace, API version, class, selectors, images, and hardware assumptions:

   ```bash
   kubectl apply -f demo/specs/quickstart/v1/gpu-test2.yaml
   ```

3. Observe allocation and scheduling:

   ```bash
   kubectl get pods -n gpu-test2 -o wide
   kubectl get resourceclaims -n gpu-test2
   kubectl describe pod -n gpu-test2 pod
   kubectl get events -n gpu-test2 --sort-by=.lastTimestamp
   ```

4. Prove device visibility from every consuming container:

   ```bash
   kubectl logs -n gpu-test2 pod --all-containers --prefix
   ```

For `gpu-test2`, success means the pod is Running, its generated claim is allocated, and both containers report the same GPU UUID. Report the observed UUID or allocation evidence, not merely that `kubectl apply` succeeded.

## Diagnose failures by layer

- No DeviceClass or ResourceSlice: inspect controller and kubelet-plugin readiness, served DRA API, RBAC, node discovery, hardware visibility, and driver logs.
- Pod Pending: describe the Pod and ResourceClaim, inspect events, and compare requested selectors/capacity with the advertised ResourceSlices.
- `ContainerCreating` or prepare failures: inspect plugin and kubelet logs, CDI configuration, container runtime integration, and `nvidiaDriverRoot`.
- Missing plugin pod on a GPU node: compare node labels, affinity, taints, and tolerations with chart output.
- kind image pull failures: verify the local image was loaded into the named cluster and that repository, tag, and pull policy match the rendered chart.
- Wrong or missing GPU in the container: check claim allocation first, then CDI and runtime injection; do not jump directly to changing the workload.
- ComputeDomain failures: verify compatible MNNVL hardware, GFD topology labels, IMEX service configuration, and the documented driver/Kubernetes prerequisites.

Gather precise resource names and relevant logs before recommending a reinstall.

## Clean up and hand off

Use the exact manifest or uniquely named namespace created for the demo:

```bash
kubectl delete -f demo/specs/quickstart/v1/gpu-test2.yaml
```

Do not delete the driver release, namespace, or kind cluster unless the user requested that scope. At handoff, state the kube context, release/chart source and version, namespace, applied values, sample and API version, observed DeviceClass/ResourceClaim/ResourceSlice state, container-level evidence, and any resources intentionally left running.
