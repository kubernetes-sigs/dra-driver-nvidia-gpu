---
title: Prerequisites
linkTitle: Prerequisites
weight: 30
description: Cluster, node, and tooling requirements before installing the driver.
---

Cluster, software, and hardware requirements for the DRA Driver for NVIDIA GPUs.

{{% alert title="Tip" %}}
[NVIDIA GPU Operator](#install-prerequisites-with-nvidia-gpu-operator) can install and manage the component software identified in the following table, such as the GPU driver, container toolkit, and so on as a convenience.
{{% /alert %}}

## Software support matrix

This page describes DRA Driver for NVIDIA GPUs v{{< param "driver_version" >}}.
GPU allocation and ComputeDomains can be enabled independently. If you enable
both, satisfy the requirements for both features.

| Component | Supported version / requirement | Applies to |
|---|---|---|
| Kubernetes | v1.34.2 or later.<br><br>Some features require enabling Kubernetes feature gates and can require newer Kubernetes versions.  Refer to [Feature gates](reference/feature-gates.md). | GPU allocation |
| Kubernetes | v1.32 or later; v1.34.2 or later is recommended. DRA is enabled by default in v1.34 and later. On v1.32 and v1.33, enable the `DynamicResourceAllocation` feature gate and the corresponding `resource.k8s.io` API groups. | ComputeDomains |
| Helm | v3.8 or later. | Both |
| NVIDIA GPU Driver | v565 or later for a direct installation. GPU Operator v25.10.0 and later uses v580 or later for DRA. | GPU allocation |
| NVIDIA GPU Driver | v570.158.01 or later for a direct installation. GPU Operator v25.10.0 and later uses v580 or later for DRA. | ComputeDomains |
| NVIDIA Container Toolkit | v1.18.0 or higher. | Both |
| Container runtime with CDI | CDI must be enabled. It is enabled by default in containerd v2.0 and later and CRI-O v1.27 and later. The DRA Driver uses CDI to expose GPUs to containers. | Both |
| Node Feature Discovery (NFD) | v0.18.2 or higher. The DRA Driver has no version-specific NFD API dependency. The driver requires NFD's NVIDIA PCI node labels to target the kubelet plugin. | Both |
| GPU Feature Discovery (GFD) | v0.18.0 or higher. For MNNVL ComputeDomains, GFD must generate the `nvidia.com/gpu.clique` node label. | ComputeDomains |

The GPU Operator owns the compatibility of the NVIDIA components that it deploys.

### Supported GPU hardware

Hardware requirements depend on the resource type and feature:

| Resource or feature | Hardware requirement |
|---|---|
| Full GPU allocation, time-slicing, or VFIO passthrough | NVIDIA Data Center GPUs |
| MPS multi-user mode | NVIDIA V100 or newer. This requirement applies specifically to `multiUser: true`. Refer to [MPS prerequisites](guides/mps.md#prerequisites). |
| MIG | A [MIG-capable data center GPU](https://docs.nvidia.com/datacenter/tesla/mig-user-guide/#supported-gpus) with Ampere architecture or newer. |
| ComputeDomains | Grace Blackwell GPUs with Multi-Node NVLink, such as NVIDIA HGX GB200 NVL72 or GB300 NVL72. The alpha `NodeLocalFabricIPC` gate also supports node-local CUDA fabric-handle IPC on GPUs and drivers that meet the requirements below. |

Check the GPU model and installed driver version on every GPU node:

```bash
nvidia-smi --query-gpu=name,driver_version --format=csv,noheader
```

For ComputeDomain-specific software and node configuration, see
[ComputeDomains additional prerequisites](#computedomains-additional-prerequisites).

## ComputeDomains additional prerequisites

If you plan to use ComputeDomains, you also need:

- NVIDIA Driver v570.158.01 or later. The `IMEXDaemonsWithDNSNames` feature gate is enabled by default and requires this driver version. The ComputeDomain plugin will fail to start on older drivers unless `IMEXDaemonsWithDNSNames` is explicitly disabled.
- Multi-Node NVLink (MNNVL) hardware, unless you enable the alpha `NodeLocalFabricIPC` mode described below. MNNVL nodes must be connected via NVLink fabric, such as GB200 NVL72 and similar systems.
- For MNNVL, a component that manages the `nvidia.com/gpu.clique` node label. Use GPU Feature Discovery (GFD), or set `kubeletPlugin.containers.computeDomains.gpuCliqueLabelEnabled=true` so the ComputeDomain kubelet plugin manages the label.

### Node-local fabric IPC

The alpha `NodeLocalFabricIPC` feature gate extends ComputeDomain channel injection to supported nodes that do not belong to an MNNVL clique. This is useful for exchanging CUDA fabric handles between isolated pods on one NVLink-connected node, including A100 systems where the installed driver reports fabric-handle support.

Enable the gate only when all of the following are true on each participating node:

- `CU_DEVICE_ATTRIBUTE_HANDLE_TYPE_FABRIC_SUPPORTED` is `1` for the allocated GPUs.
- `/proc/devices` contains `nvidia-caps-imex-channels`.
- `/proc/driver/nvidia/params` reports a nonzero `ImexChannelCount`.
- `nvidia-imex` and `nvidia-imex-ctl` are installed with the NVIDIA driver.
- NVIDIA Fabric Manager is running on NVSwitch systems such as HGX A100.
- The communicating pods use the same ComputeDomain channel ResourceClaim and are scheduled onto the same node.

```bash
--set featureGates.NodeLocalFabricIPC=true
```

On a node without a clique, the driver starts one `nvidia-imex` instance for the ComputeDomain and injects the allocated `/dev/nvidia-caps-imex-channels/channelN` device into the workload containers. Workload preparation waits until the local IMEX instance is ready. Applications must allocate and export memory with `CU_MEM_HANDLE_TYPE_FABRIC`. This feature does not share the Linux IPC namespace and does not make legacy `cudaIpcGetMemHandle` or `cudaIpcOpenMemHandle` work across isolated PID namespaces.

{{% alert color="warning" title="Warning" %}}
ComputeDomain isolation is currently domain-wide: all workloads in a ComputeDomain receive channel 0. Do not place mutually untrusted tenants in the same ComputeDomain.
{{% /alert %}}

### Driver-managed IMEX

The default `resources.computeDomains.imex.mode=driverManaged` setting makes the DRA Driver create one `nvidia-imex` DaemonSet for each `ComputeDomain`.
On all GPU nodes where the `nvidia-imex-*` packages are installed, disable the host `nvidia-imex.service` systemd unit:

```bash
systemctl disable --now nvidia-imex.service && systemctl mask nvidia-imex.service
```

### Host-managed IMEX

By default the driver owns the `nvidia-imex` daemon lifecycle, per the requirement above. For clusters where the operator already runs `nvidia-imex` as a host service (for example through systemd), set `resources.computeDomains.imex.mode=hostManaged` to stop the driver from creating per-ComputeDomain `nvidia-imex` DaemonSets. This **inverts** the requirement above: `nvidia-imex.service` must be installed, configured, and left **running** on every participating GPU node. Also, the `nvidia-imex` service should be configured with
`IMEX_CMD_ENABLED=1` and `IMEX_CMD_UNIX_DOMAIN_PATH=/etc/nvidia-imex/imex_ctrl.sock` (configurable via Helm).

{{% alert color="warning" title="Warning" %}}
Some `nvidia-imex.service` units use `ProtectSystem=full`, which prevents the service from creating the default socket under `/etc`.
For these units, set `IMEX_CMD_UNIX_DOMAIN_PATH` and `resources.computeDomains.imex.hostSocketPath` to the same writable runtime path, such as `/var/run/nvidia-imex-ctrl.sock`.
{{% /alert %}}

Host-managed mode requires two Helm values together:

```bash
--set featureGates.HostManagedIMEXDaemon=true \
--set resources.computeDomains.imex.mode=hostManaged
```

- `featureGates.HostManagedIMEXDaemon` is an alpha gate that only unlocks setting `imex.mode=hostManaged`. It does not by itself change any behavior. Setting `imex.mode=hostManaged` without this gate enabled is a startup validation error (`helm install`/`helm template` fails immediately, and if bypassed, the controller and kubelet plugin pods will fail to start).
- `imex.isolation` selects the IMEX isolation strategy, and applies under both `driverManaged` and `hostManaged` mode. It must be set to one of:
  - `domain` (default) — all workloads running in the same IMEX domain share the same channel (0). Under `driverManaged` mode this is inherent to the model (the driver creates one `nvidia-imex` daemon per ComputeDomain); under `hostManaged` mode, multiple ComputeDomains can still run against the same host IMEX domain, all receiving channel 0, with no isolation between them.
  - `channel` — intended to eventually give each workload a unique channel within an IMEX domain. Not implemented yet: setting it is a startup validation error regardless of `imex.mode`.
  - Any other value is a startup validation error.
- Changing `imex.mode` or `imex.isolation` on a cluster with active ComputeDomain workloads is not supported and is not enforced by the driver, drain and remove existing ComputeDomains first.

#### Host IMEX readiness check (Unix domain socket required)

Because there is no driver-managed daemon to report readiness under host-managed IMEX, the kubelet plugin validates the host `nvidia-imex` daemon itself at channel claim prepare time: it runs `nvidia-imex-ctl -q --u=<socket path>` chrooted into the driver root and requires a `READY` response before handing out a channel. A claim fails, and is retried, rather than succeeding silently, if the host daemon isn't reachable or ready.

To enable it:

1. On every participating GPU node, add to `/etc/nvidia-imex/config.cfg`:

   ```text
   IMEX_CMD_ENABLED=1
   IMEX_CMD_UNIX_DOMAIN_PATH=/etc/nvidia-imex/imex_ctrl.sock
   ```

   then restart `nvidia-imex.service` so it creates the socket file (IMEX config changes require a restart to take effect).
2. `nvidia-imex-ctl` must be present under the driver root alongside `nvidia-smi` (it ships with the `nvidia-imex` package).
3. If a non-default socket path is used, set `resources.computeDomains.imex.hostSocketPath` (Helm value, plumbed to the kubelet plugin as `IMEX_HOST_SOCKET_PATH`) to match. It defaults to `/etc/nvidia-imex/imex_ctrl.sock`.

After installation, follow [Validate host-managed IMEX](guides/compute-domain-workloads.md#validate-host-managed-imex)
to verify the created objects, channel injection, and Prepare retry behavior.

## Install prerequisites with NVIDIA GPU Operator

The [NVIDIA GPU Operator](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/index.html) is a Kubernetes operator that automates the deployment and lifecycle management of all NVIDIA software components needed to provision and monitor GPUs in a cluster.

It can manage the following DRA Driver for NVIDIA GPUs prerequisites for you:

- NVIDIA Driver (v565+ for GPU allocation, v570.158.01+ for ComputeDomains). The GPU Operator installs a [default driver](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/platform-support.html#gpu-operator-component-matrix) that meets the DRA Driver's prerequisites. To use a specific version, see [Common chart customization options](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/getting-started.html#common-chart-customization-options) in the GPU Operator documentation.
- CDI enabled through the NVIDIA Container Toolkit.
- Node Feature Discovery (NFD).
- GPU Feature Discovery (GFD), which can own the ComputeDomain `nvidia.com/gpu.clique` label when you do not enable driver ownership.

If you choose to install the GPU Operator, follow the [DRA Driver for NVIDIA GPUs install guide](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/dra-intro-install.html) in the GPU Operator documentation. It covers installing the GPU Operator with the NVIDIA Kubernetes Device Plugin disabled and installing the DRA Driver for NVIDIA GPUs.
