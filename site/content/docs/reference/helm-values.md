---
title: Helm chart values
linkTitle: Helm values
weight: 30
description: Configuration parameters for the dra-driver-nvidia-gpu Helm chart.
---

The `dra-driver-nvidia-gpu` Helm chart deploys the controller, kubelet plugin, optional admission webhook, and cluster-scoped DeviceClass resources. 
Set values in a custom `values.yaml` file or pass them with `--set` at install or upgrade time.

This page documents Helm values for the DRA Driver for NVIDIA GPUs.
For install and upgrade steps, see [Install](../install.md) and [Upgrade](../upgrade.md). 
The authoritative source for defaults is [`values.yaml`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/{{< param driver_release_tag >}}/deployments/helm/dra-driver-nvidia-gpu/values.yaml) at that release.

To list the full set of values for the current release:

```bash
helm show values oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu --version {{< param driver_version >}}
```

## Driver and host paths

| Value | Default | Description |
|---|---|---|
| `nvidiaDriverRoot` | `/` | Root directory of the NVIDIA driver installation on the host. Mounted into kubelet plugin containers so the driver can access NVML, device nodes, and libraries. Use `/run/nvidia/driver` when the NVIDIA GPU Operator manages drivers; use `/home/kubernetes/bin/nvidia` on GKE. |
| `nvidiaCDIHookPath` | `""` | Optional path to the `nvidia-cdi-hook` executable. When empty, the chart uses the default path inferred from the installed nvidia-container-toolkit version. |
| `altProcDevices` | `""` | Optional host path to a file that replaces `/proc/devices` inside kubelet plugin containers. Used with mock NVML in development to inject fake IMEX channel entries without a real kernel driver. |

## Resource plugins

| Value | Default | Description |
|---|---|---|
| `gpuResourcesEnabledOverride` | `false` | Must be `true` to enable GPU allocation when `resources.gpus.enabled=true`. Required because the driver cannot run alongside the standard GPU device plugin until [KEP 5004](https://github.com/kubernetes/enhancements/issues/5004) reaches GA. |
| `resources.gpus.enabled` | `true` | Deploy the GPU kubelet plugin and register `gpu.nvidia.com`, `mig.nvidia.com`, and `vfio.gpu.nvidia.com` DeviceClasses. Requires `gpuResourcesEnabledOverride=true`. |
| `resources.computeDomains.enabled` | `true` | Deploy the ComputeDomain controller and kubelet plugin; register ComputeDomain DeviceClasses. When `false`, the controller Deployment and compute-domain kubelet plugin container are not deployed. |

Disable a plugin you do not need with no impact on the other:

```bash
--set resources.computeDomains.enabled=false
# or
--set resources.gpus.enabled=false
```

When GPU allocation is enabled, the chart also creates DeviceClass resources for full GPUs, MIG slices, and VFIO passthrough. Individual device types still require the appropriate hardware and [feature gates](feature-gates/).

## ComputeDomain IMEX

| Value | Default | Description |
|---|---|---|
| `resources.computeDomains.imex.mode` | `driverManaged` | Selects who manages the `nvidia-imex` daemon lifecycle: `driverManaged` creates a per-ComputeDomain daemon DaemonSet, while `hostManaged` uses a host service that you manage and requires the [`HostManagedIMEXDaemon`](feature-gates.md) feature gate. |
| `resources.computeDomains.imex.isolation` | `domain` | Selects IMEX isolation, where `domain` shares channel 0 among workloads in the same IMEX domain and `channel` is reserved but unsupported. |
| `resources.computeDomains.imex.hostSocketPath` | `/etc/nvidia-imex/imex_ctrl.sock` | Sets the path under `nvidiaDriverRoot` to the host IMEX command socket that the ComputeDomain kubelet plugin queries for a `READY` response during claim preparation in `hostManaged` mode. |

Drain ComputeDomain workload pods and delete existing `ComputeDomain` resources before you change `resources.computeDomains.imex.mode` or `resources.computeDomains.imex.isolation`.


## Kubernetes API version

| Value | Default | Description |
|---|---|---|
| `resourceApiVersion` | `""` | API version for `resource.k8s.io` resources (`DeviceClass`, `ResourceClaim`, `ResourceClaimTemplate`). When empty, the chart auto-detects the highest supported version (`v1` > `v1beta2` > `v1beta1`). Set explicitly if your cluster reports incorrect API capabilities—for example, `resourceApiVersion: "resource.k8s.io/v1beta1"`. |

## Naming and namespace

| Value | Default | Description |
|---|---|---|
| `nameOverride` | `""` | Override the chart name used in Kubernetes resource names. Set to `nvidia-dra-driver-gpu` on the first upgrade from pre-v0.4.0 releases to preserve existing resource names. See [Upgrade](../upgrade.md). |
| `fullnameOverride` | `""` | Override the full resource name prefix used across all chart objects. |
| `namespaceOverride` | `""` | Override the release namespace in rendered manifests. Prefer `helm install --namespace` instead. |
| `selectorLabelsOverride` | `{}` | Replace the default pod selector labels when non-empty. For advanced customization only. |
| `allowDefaultNamespace` | `false` | Allow installation into the `default` namespace. The chart fails by default when the release namespace is `default`. |

## Image

| Value | Default | Description |
|---|---|---|
| `image.repository` | `registry.k8s.io/dra-driver-nvidia/dra-driver-nvidia-gpu` | Container image repository for all driver components. |
| `image.tag` | `""` | Image tag. When empty, defaults to the chart `appVersion` with a `v` prefix (for example, `v{{< param driver_version >}}`). |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy for all driver containers and per-claim MPS daemon containers. |
| `imagePullSecrets` | `[]` | Secrets for pulling images for driver pods and per-claim MPS daemon pods from private registries. |

## Feature gates

| Value | Default | Description |
|---|---|---|
| `featureGates` | `{}` | Key-value map of feature gate names to `true` or `false`. Passed to all driver components. Includes both driver-specific gates and upstream Kubernetes logging gates. |

See [Feature gates](feature-gates/) for available gates, defaults, and mutual-exclusion rules.

## Logging

| Value | Default | Description |
|---|---|---|
| `logVerbosity` | `"4"` | Global log verbosity (0–7) for the controller, kubelet plugin, and webhook. Error, Warning, and Info (level 0) messages are always logged. Per-component overrides can be set via environment variables; see the [troubleshooting wiki](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/wiki/Troubleshooting#controlling-log-verbosity). |

## Admission webhook

Disabled by default.

| Value | Default | Description |
|---|---|---|
| `webhook.enabled` | `false` | Deploy the validating admission webhook. Validates opaque configuration in `ResourceClaim` and `ResourceClaimTemplate` specs before they reach the API server. |
| `webhook.replicas` | `1` | Number of webhook pod replicas. |
| `webhook.servicePort` | `443` | Service port the API server uses to reach the webhook. |
| `webhook.containerPort` | `443` | Container port the webhook process listens on. |
| `webhook.failurePolicy` | `Fail` | API server behavior when the webhook is unreachable: `Fail` (reject) or `Ignore` (allow). |
| `webhook.tls.mode` | `cert-manager` | Certificate source: `cert-manager` (automatic) or `secret` (user-provided). |
| `webhook.tls.certManager.issuerType` | `selfsigned` | cert-manager issuer type: `selfsigned`, `clusterissuer`, or `issuer`. |
| `webhook.tls.certManager.issuerName` | `""` | Issuer name when `issuerType` is `clusterissuer` or `issuer`. |
| `webhook.tls.secret.name` | `""` | TLS secret name when `tls.mode` is `secret`. Must contain `tls.crt` and `tls.key`. |
| `webhook.tls.secret.caBundle` | `""` | Base64-encoded CA bundle for validating the webhook certificate when using `secret` mode. |

Prerequisite for `cert-manager` mode: [cert-manager](https://cert-manager.io/) must be installed in the cluster. See [Install: Admission webhook](../install.md#optional-admission-webhook) for an example.

Additional `webhook.*` values control scheduling (`nodeSelector`, `tolerations`, `affinity`), resource limits, service ports, and the webhook ServiceAccount.

## ComputeDomain controller

Deployed when `resources.computeDomains.enabled=true`.

| Value | Default | Description |
|---|---|---|
| `controller.replicas` | `1` | Number of controller pod replicas. |
| `controller.leaderElection.enabled` | `false` | Enable leader election when running multiple replicas. |
| `controller.leaderElection.leaseDuration` | `15s` | How long the leader holds the lease before renewal is required. |
| `controller.leaderElection.renewDeadline` | `10s` | How long the leader retries renewal before giving up. |
| `controller.leaderElection.retryPeriod` | `2s` | Interval between leader-election retries. |
| `controller.metrics.enabled` | `true` | Expose Prometheus metrics on the controller pod. |
| `controller.metrics.httpEndpoint` | `:8080` | Metrics listen address. |
| `controller.metrics.profilePath` | `""` | Optional pprof profile path. Empty disables profiling. |
| `controller.priorityClassName` | `system-node-critical` | Priority class for the controller pod. |
| `controller.networkPolicy.enabled` | `false` | Create a NetworkPolicy for the controller. |

The controller schedules onto control-plane nodes by default (`nodeSelector`, `tolerations`, and `affinity` are configurable). Additional `controller.*` values set pod annotations, security contexts, and container resource limits.

## Kubelet plugin

Deployed as a DaemonSet on GPU nodes when either resource plugin is enabled.

| Value | Default | Description |
|---|---|---|
| `kubeletPlugin.updateStrategy` | `RollingUpdate` with `maxUnavailable: 100%` | DaemonSet update strategy. Allows all plugin pods on a node to update concurrently. |
| `kubeletPlugin.priorityClassName` | `system-node-critical` | Priority class for kubelet plugin pods. |
| `kubeletPlugin.kubeletRegistrarDirectoryPath` | `/var/lib/kubelet/plugins_registry` | Host path to the kubelet plugin registry directory. |
| `kubeletPlugin.kubeletPluginsDirectoryPath` | `/var/lib/kubelet/plugins` | Host path to the kubelet plugins directory. |
| `kubeletPlugin.metrics.enabled` | `true` | Expose Prometheus metrics from plugin containers. |
| `kubeletPlugin.metrics.gpuHttpEndpoint` | `:8080` | Metrics endpoint for the GPU plugin container. |
| `kubeletPlugin.metrics.computeDomainHttpEndpoint` | `:8081` | Metrics endpoint for the ComputeDomain plugin container. |
| `kubeletPlugin.containers.gpus.healthcheckPort` | `51516` | gRPC health check port for the GPU container. Set to a negative value to disable. |
| `kubeletPlugin.containers.computeDomains.healthcheckPort` | `51515` | gRPC health check port for the ComputeDomain container. Set to a negative value to disable. |
| `kubeletPlugin.containers.computeDomains.gpuCliqueLabelEnabled` | `false` | Lets the ComputeDomain kubelet plugin set the `nvidia.com/gpu.clique` node label when GPU Feature Discovery does not own it. |
| `kubeletPlugin.networkPolicy.enabled` | `false` | Create a NetworkPolicy for kubelet plugin pods. |

The DaemonSet tolerates `nvidia.com/gpu` taints (`NoSchedule`) and requires nodes to match one of several GPU presence labels (NVIDIA GPU Operator or Node Feature Discovery). GPU and ComputeDomain plugin containers run as privileged by default. An init container prepares plugin directories before the main containers start. Additional `kubeletPlugin.*` values set per-container environment variables, resource limits, and scheduling constraints.

## Service accounts

| Value | Default | Description |
|---|---|---|
| `serviceAccount.create` | `true` | Create the main driver ServiceAccount used by the controller and kubelet plugin. |
| `serviceAccount.name` | `""` | ServiceAccount name. Generated from the release name when empty. |
| `serviceAccount.annotations` | `{}` | Annotations added to the main ServiceAccount. |
| `webhook.serviceAccount.create` | `true` | Create a separate ServiceAccount for the webhook when `webhook.enabled=true`. |
| `webhook.serviceAccount.name` | `""` | Webhook ServiceAccount name. Generated when empty. |
