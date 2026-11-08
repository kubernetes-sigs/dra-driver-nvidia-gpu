---
name: dra-helm-demo-sample-workloads
description: Guide direct and GPU Operator-managed deployment and upgrades, kind demos, sample manifest authoring, and quickstart validation for the NVIDIA DRA driver. Use when a user asks to install, upgrade, or demo the driver, create a resource.k8s.io ResourceClaim or ResourceClaimTemplate workload, share one claim across containers, prove the same GPU UUID, exercise the repository's kind workflow, or validate DeviceClass, ResourceSlice, claim, and container allocation evidence.
metadata:
  author: "DRA Driver for NVIDIA GPUs team"
---

# Helm, demos, and sample workloads

Help users deploy the driver deliberately, choose a sample that matches the cluster, and prove GPU allocation from the Kubernetes objects down to the container. Keep rendered-manifest work separate from changes to a live cluster.

## Start by establishing the target

Determine these facts from the repository and the target environment before choosing commands:

- Whether the user wants instructions, rendered YAML, a released-chart install, an existing-release upgrade, a local-chart install, or a disposable kind demo.
- The exact kube context, cluster, release name, namespace, chart version, and values file.
- Whether GPU Operator manages or will manage the node stack and DRA driver. Treat this as a separate installation path and follow the GPU Operator project's DRA integration guidance instead of layering a standalone install over it.
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

The NVIDIA GPU Operator can install and manage the DRA driver as part of the NVIDIA software stack on a cluster. Treat the Operator-managed route as a first-class alternative to installing this repository's chart directly.

For Operator-specific enablement, configuration, component compatibility, upgrades, and lifecycle troubleshooting, defer to the [GPU Operator DRA installation guide](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/dra-intro-install.html) and the [NVIDIA GPU Operator project](https://github.com/NVIDIA/gpu-operator). If the GPU Operator project provides repository-local agent skills, load and follow the relevant skills because that project owns the Operator integration details. Continue using this skill for choosing sample workloads and validating DRA allocation after installation.

Do not mix direct Helm lifecycle commands from this repository with an Operator-managed deployment unless the GPU Operator documentation explicitly calls for that action. Establish which system owns the DRA release and related NVIDIA components before proposing installation, upgrade, rollback, or cleanup commands.

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

Use this workflow when a DRA release already exists. Establish whether direct Helm or GPU Operator owns it before choosing commands. For an Operator-managed release, follow the Operator upgrade guidance above; do not upgrade its DRA chart independently.

### Capture the current state

Confirm the exact kube context, release name, namespace, installed chart and image versions, and intended target chart version. Preserve the existing release identity; an older installation may use different names from the new-install example.

Read-only discovery for a directly managed Helm release can include:

```bash
helm --kube-context <context> status <release> --namespace <namespace>
helm --kube-context <context> history <release> --namespace <namespace>
helm --kube-context <context> get values <release> --namespace <namespace> -o yaml
helm --kube-context <context> get values <release> --namespace <namespace> --all -o yaml
helm --kube-context <context> get manifest <release> --namespace <namespace>
kubectl --context <context> get pods -n <namespace> -o wide
kubectl --context <context> get deviceclasses
kubectl --context <context> get resourceslices -o wide
kubectl --context <context> get resourceclaims -A -o yaml
```

Save the release revision, user-supplied values, computed values, and installed manifest before changing anything. Record which workloads and claims are healthy, their allocations and selected nodes, and the current ComputeDomain/IMEX mode when enabled. Use computed values as diagnostic evidence rather than blindly carrying every old default into a new chart.

### Review the source-to-target transition

Read `site/content/docs/upgrade.md` and the source and target release notes for the exact transition. Use documentation and chart sources matching the target release rather than assuming the development branch describes an older released artifact. If the transition is undocumented or compatibility is unresolved, report the gap before executing an upgrade.

Check Kubernetes and served DRA API compatibility, NVIDIA driver/Container Toolkit requirements, feature gates, enabled plugins, Helm value changes, and checkpoint/API downgrade restrictions. Determine any required intermediate releases or workload maintenance steps. Preserve existing release/resource names and overrides where the transition requires them; do not substitute the new-install names.

Apply CRD updates only when required by the applicable upgrade procedure, using that release's CRDs and the documented ordering. Helm does not update existing CRDs from the chart's `crds/` directory on upgrade. Do not assume every transition needs CRD changes or that a Helm rollback reverses them. If changing ComputeDomain IMEX mode or isolation, follow the guide's workload-drain and ComputeDomain-removal requirements rather than combining that change with an ordinary rolling upgrade.

Build a reviewed values file from the existing user-supplied configuration and intentional target-version changes. Retain required settings such as driver root, resource enablement, image overrides, and naming overrides after checking their target-version semantics. If the transition guide calls for `--reuse-values`, use that strategy deliberately instead of the file-based example below, accounting for old defaults it may retain.

Render the exact target chart with the reviewed values before a live change:

```bash
helm template <release> \
  oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
  --version <target-chart-version> \
  --namespace <namespace> \
  --values <reviewed-values.yaml> \
  --is-upgrade \
  --kube-version <target-cluster-kubernetes-version> \
  --api-versions <served-resource.k8s.io-api-version>
```

Choose capabilities from the actual target cluster and compare the result with the installed manifest. Review resource names, CRDs, RBAC, images, mounts, feature gates, and rollout behavior. Local rendering does not prove admission or runtime compatibility. Before execution, explain the expected workload impact, stopping conditions, and whether rollback is supported; preserve the previous revision even when a transition is forward-only.

### Execute only the authorized upgrade

An instructions-only request does not authorize a cluster change. Once the user has requested the upgrade and its target is unambiguous, follow the applicable guide's prerequisite and CRD steps, then upgrade the existing release. For a compatible directly managed release using reviewed values:

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

Use a staged validation ladder so failures are localized.

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
