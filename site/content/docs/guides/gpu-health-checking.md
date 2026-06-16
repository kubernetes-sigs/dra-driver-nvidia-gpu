---
title: GPU health checking
linkTitle: GPU health checking
weight: 40
description: >
  Monitor GPU health using NVML and apply device taints to prevent new workloads
  from scheduling on unhealthy GPUs.
---

The `NVMLDeviceHealthCheck` feature gate enables continuous GPU health monitoring
through the [NVIDIA Management Library (NVML)](https://developer.nvidia.com/management-library-nvml).
When a GPU enters an error state, the driver applies a
[device taint](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#device-taints-and-tolerations)
to the corresponding [`ResourceSlice`](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#resourceslice),
signaling the Kubernetes scheduler to avoid placing new workloads on the affected
device.

Key capabilities:

- Continuous NVML monitoring: an event monitor watches each GPU for XID
  errors and device-loss events for as long as the kubelet plugin runs.
- Automatic device taints: unhealthy devices are tainted on their
  `ResourceSlice` so the scheduler stops placing new workloads on them.
- Configurable XID handling: you control which XID codes are treated as
  non-fatal, so application-level errors do not block scheduling.

This is most useful for distributed training jobs, where a single unhealthy GPU
can stall or corrupt an entire job; inference serving, where degraded devices
can cause silent numerical errors; and cluster observability, where non-fatal
warnings are surfaced to monitoring tools without affecting scheduling.

## Feature status - Alpha

`NVMLDeviceHealthCheck` is an Alpha feature gate, disabled by default.

| Feature gate | Default | Stage | Since |
|---|---|---|---|
| `NVMLDeviceHealthCheck` | `false` | Alpha | v25.12.0 |

> [!NOTE]
>
> This page describes the v0.4.0 and later implementation of health checking with the driver.

## How it works

When enabled, the GPU kubelet plugin starts an
[NVML event monitor](https://docs.nvidia.com/deploy/nvml-api/group__nvmlEvents.html).
When a health
event occurs on a GPU, the driver updates the `ResourceSlice` for the affected device
with a device taint.

The monitor tracks three event categories:

| Event | Taint key | Default effect | Description |
|---|---|---|---|
| XID error (fatal) | `gpu.nvidia.com/xid` | `NoSchedule` | A critical GPU hardware or firmware error. |
| XID error (non-fatal) | `gpu.nvidia.com/xid` | `None` | An application-level error that does not indicate hardware degradation. |
| GPU lost | `gpu.nvidia.com/gpu-lost` | `NoSchedule` | The GPU has become inaccessible to the driver. |
| Unmonitored | `gpu.nvidia.com/unmonitored` | `None` | The device cannot be monitored by NVML. |

### Taint effects

The driver applies the following [upstream Kubernetes device taint effects](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#device-taints-and-tolerations):

- `NoSchedule` — the Kubernetes scheduler does not allocate the device to new
  workloads. Existing workloads that already hold a claim to the device are not
  evicted.
- `None` — informational only; scheduling is not affected, but the taint is
  visible in the `ResourceSlice`.

The driver only ever applies the `None` and `NoSchedule` effects. It never evicts running workloads with `NoExecute`. If you would like to implement eviction rules when a device is tainted, an administrator can create a `DeviceTaintRule` with `effect: NoExecute`. Follow the Kubernetes documentation for [taints set up admins](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#taints-set-by-an-admin) for details.

### XID errors

XID codes are NVIDIA-defined error identifiers for GPU hardware and firmware
conditions. The driver classifies XIDs as fatal or non-fatal. Fatal XIDs produce
a `NoSchedule` taint and non-fatal XIDs produce a `None` taint.

By default, the driver sets the following XID errors as non-fatal because they indicate application-level failures rather than hardware degradation.  

| XID | Description |
|---|---|
| 13 | Graphics Engine Exception |
| 31 | GPU memory page fault |
| 43 | GPU stopped processing |
| 45 | Preemptive cleanup due to previous errors |
| 68 | Video processor exception |
| 109 | Context Switch Timeout Error |

Refer to the [NVIDIA XID Errors documentation](https://docs.nvidia.com/deploy/xid-errors/index.html) for full descriptions for XID errors and codes.

### Configure non-fatal XID list

You can add XIDs to the non-fatal list using the `ADDITIONAL_XIDS_TO_IGNORE`
environment variable on the GPU kubelet plugin's `gpus` container. Set it via
Helm values:

```yaml
kubeletPlugin:
  containers:
    gpus:
      env:
        - name: ADDITIONAL_XIDS_TO_IGNORE
          value: "48,62"
```

The value is a comma-separated list of XID codes. XIDs in this list receive a
`None` taint effect instead of `NoSchedule`.

Apply the change with `helm upgrade`, either by passing a values file:

```bash
helm upgrade dra-driver-nvidia-gpu oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
    --namespace dra-driver-nvidia-gpu \
    -f values.yaml
```

The new value takes effect after the `kubelet-plugin` DaemonSet pods restart.

## Prerequisites

- The [`DRADeviceTaints`](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#device-taints-and-tolerations)
  Kubernetes feature gate must be enabled on the `kube-apiserver`,
  `kube-controller-manager`, and `kube-scheduler`.
  In Kubernetes v1.34 and 1.35, `DRADeviceTaints` is disabled by default and must be explicitly enabled.
  In Kubernetes v1.36, it is enabled by default.
- NVIDIA DRA driver v0.4.0 or later installed via Helm.

## Enabling the feature

Add the following to your Helm values:

```yaml
featureGates:
  NVMLDeviceHealthCheck: true
```

Then apply the change with `helm upgrade`:

```bash
helm upgrade dra-driver-nvidia-gpu oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
    --namespace dra-driver-nvidia-gpu \
    --reuse-values \
    --set featureGates.NVMLDeviceHealthCheck=true
```

### Incompatible feature gates

`NVMLDeviceHealthCheck` cannot be combined with the following feature gates.
Enabling any combination causes the driver to fail at startup.

| Feature gate | Reason |
|---|---|
| `DynamicMIG` | Dynamic [MIG](https://docs.nvidia.com/datacenter/tesla/mig-user-guide/) changes the GPU device hierarchy at runtime, which is incompatible with the static device placement map the health monitor relies on. |
| `PassthroughSupport` | Passthrough devices are not accessible via NVML and cannot be monitored. |

## Recovering from an unhealthy device

Device taints persist until the GPU kubelet plugin restarts. There is no
automated taint removal in the current release.

To clear taints after a hardware issue is resolved:

1. Confirm the hardware error is resolved (for example, by checking
   [`nvidia-smi`](https://docs.nvidia.com/deploy/nvidia-smi/index.html) output or
   kernel logs).
2. Restart the GPU kubelet plugin by rolling its `kubelet-plugin` DaemonSet:

```bash
kubectl rollout restart daemonset/dra-driver-nvidia-gpu-kubelet-plugin -n dra-driver-nvidia-gpu
```

On restart, the GPU kubelet plugin re-evaluates device health. Devices with no active NVML health events will not receive taints.

> [!NOTE]
>
> If the underlying hardware issue persists, the taint is reapplied after restart.

Kubernetes administrators can use
[`DeviceTaintRule`](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#taints-set-by-an-admin)
objects to manually remove or override device taints without restarting the driver.

> [!NOTE]
>
> `DeviceTaintRule` is gated separately from `DRADeviceTaints`. It requires the
> [`DRADeviceTaintRules`](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#taints-set-by-an-admin)
> feature gate and the `resource.k8s.io/v1beta2` API. 

## Limitations and considerations

- **No automated recovery**: Taint removal requires a driver restart or a manual
  `DeviceTaintRule` override. The driver does not clear taints when hardware
  recovers.
- **One taint per key per device**: Each device holds at most one taint per taint
  key. If multiple XID events occur on the same device, only the most recent value
  is retained.
- **Incompatible feature gates**: Cannot be used with `DynamicMIG` or
  `PassthroughSupport`. See [Incompatible feature gates](#incompatible-feature-gates).
- **Publish failure handling**: If the driver fails to update the `ResourceSlice`
  after a health event (for example, due to a transient API server error), the
  failure is logged but not retried. The `ResourceSlice` may remain stale until
  the next successful publish or driver restart.

