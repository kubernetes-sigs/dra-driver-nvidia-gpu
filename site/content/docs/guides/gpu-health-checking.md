---
title: GPU health checking
linkTitle: GPU health checking
weight: 60
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

With this feature enabled, unhealthy devices are tainted in the
`ResourceSlice` so the scheduler stops placing new workloads on them.

## Feature status

`NVMLDeviceHealthCheck` is an Alpha feature gate, disabled by default.

| Feature gate | Default | Stage | Since |
|---|---|---|---|
| `NVMLDeviceHealthCheck` | `false` | Alpha | v0.4.0 |

`NVMLDeviceHealthCheck` is mutually exclusive with the `DynamicMIG`,
`PassthroughSupport`, and `MPSSupport` feature gates.
Refer to the [feature gate constraints](../reference/feature-gates/#constraints) documentation for more details.

## Prerequisites

- The [`DRADeviceTaints`](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#device-taints-and-tolerations)
  Kubernetes feature gate must be enabled on the `kube-apiserver`,
  `kube-controller-manager`, and `kube-scheduler`.
  In Kubernetes v1.34 and 1.35, `DRADeviceTaints` is disabled by default and must be explicitly enabled.
  In Kubernetes v1.36, the feature gate is enabled by default.
- NVIDIA DRA driver v0.4.0 or later installed via Helm.

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

The driver applies the following [Kubernetes device taint effects](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#device-taints-and-tolerations):

- `NoSchedule`: The Kubernetes scheduler does not allocate the device to new
  workloads. Existing workloads that already hold a claim to the device are not
  evicted.
- `None`: This taint is informational only. Scheduling is not affected, but the taint is
  visible in the `ResourceSlice`.

The driver only applies the `None` and `NoSchedule` effects.
The driver never evicts running workloads with `NoExecute`.
If you want to implement eviction rules when a device is tainted, create a `DeviceTaintRule` with `effect: NoExecute`.
Follow the Kubernetes documentation for [taints set by an admin](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#taints-set-by-an-admin) for details.

### XID errors

XID codes are NVIDIA-defined error identifiers for GPU hardware and firmware
conditions.
The driver classifies XIDs as fatal or non-fatal.
Fatal XIDs produce a `NoSchedule` taint and non-fatal XIDs produce a `None` taint.
Refer to the [NVIDIA XID Errors documentation](https://docs.nvidia.com/deploy/xid-errors/latest/introduction.html) for information about XID errors and codes.

The driver does not classify XIDs from a fixed list of codes.
For each XID event, it queries the recovery action that NVML currently reports for the parent GPU and classifies the event from that action:

| Recovery action | Classification | Taint effect |
| --- | --- | --- |
| `NONE` | Non-fatal | `None` |
| `RECOVER IMEX DOMAIN` | Non-fatal | `None` |
| `GPU RESET` | Fatal | `NoSchedule` |
| `NODE REBOOT` | Fatal | `NoSchedule` |
| `DRAIN P2P` | Fatal | `NoSchedule` |
| `DRAIN AND RESET` | Fatal | `NoSchedule` |

`RECOVER IMEX DOMAIN` requests recovery of the IMEX domain rather than of the local GPU, so the driver keeps the event informational for GPU scheduling.

If the driver cannot query the recovery action, for example when the parent GPU handle is unavailable, it treats the event as fatal unless the XID is listed in `--additional-xids-to-ignore`.

To treat specific XID errors as non-fatal regardless of the reported recovery action, specify a comma-separated list in the `--additional-xids-to-ignore` CLI argument or the `ADDITIONAL_XIDS_TO_IGNORE` environment variable.
A listed XID never produces a `NoSchedule` taint.
The driver still queries the recovery action and records it in the log.

> [!NOTE]
>
> In v0.5.0 and earlier, the driver classified XIDs 13, 31, 43, 45, 68, and 109
> as non-fatal from a built-in list, and `--additional-xids-to-ignore` added to
> that list. The built-in list has been removed and the argument is now an
> override.

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

## View taints on ResourceSlice

Each taint appears on an individual device entry in `ResourceSlice.spec.devices`, not on the `ResourceSlice` itself.
Use `jq` to show only device entries that have taints:

```bash
kubectl get resourceslices -o json | jq '
  .items[].spec.devices[]
  | select((.taints // []) | length > 0)
  | {
      device: .name,
      taints: [.taints[] | {key, value, effect, timeAdded}]
    }
'
```

The following output shows an example non-fatal XID event:

```json
    {
  "device": "gpu-0-mig-1g12gb-19-0",
  "taints": [
    {
      "key": "gpu.nvidia.com/xid",
      "value": "43",
      "effect": "None",
      "timeAdded": "2026-07-22T02:24:46Z"
    }
  ]
}
```

The response includes the following details:
* The `device` field identifies the affected device entry in the `ResourceSlice`.
* The `key` field identifies the health event category, and `gpu.nvidia.com/xid` indicates an XID error.
* The `value` field contains the decimal XID code reported by NVML, which is `43` in this example.
* The `effect` field is `None` because NVML reported a recovery action of `NONE` for the parent GPU, so this taint records the event without preventing new allocations. When the recovery action calls for a GPU reset, a node reboot, or a drain, the effect is `NoSchedule`, which prevents new allocations that do not tolerate the taint.
* The `timeAdded` field records when the API server added the taint. The GPU kubelet plugin leaves this field unset when it adds or changes a taint so that the API server assigns the timestamp.

## Recovering from an unhealthy device

Device taints persist until the GPU kubelet plugin restarts. There is no
automated taint removal in the current release.

To clear taints after a hardware issue is resolved:

1. Confirm the hardware error is resolved. Use dmesg to check the kernal logs.
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
  key. Once a device carries a `NoSchedule` taint for a key, later events on that
  key do not change it; the taint remains until recovery removes it, so a
  subsequent non-fatal XID cannot downgrade it to `None`. For a key that holds a
  `None` taint, a later event replaces the value and effect.
- **Mutually exclusive feature gates**: Cannot be used with `DynamicMIG`,
  `PassthroughSupport`, or `MPSSupport`.
- **Publish failure handling**: If the driver fails to update the `ResourceSlice`
  after a health event (for example, due to a transient API server error), the
  failure is logged but not retried. The `ResourceSlice` may remain stale until
  the next successful publish or driver restart.
