# Prerequisites

Cluster, software, and hardware requirements for the DRA Driver for NVIDIA GPUs.

> Tip: Most of these prerequisites can be installed and managed for you by the [NVIDIA GPU Operator](#install-prerequisites-with-nvidia-gpu-operator).


| Requirement | Version / Notes |
|---|---|
| Kubernetes | v1.34.2 or later, with at least one node that has one or more NVIDIA GPUs. The use of DRA became GA in Kubernetes v1.34+ and earlier versions required the `DynamicResourceAllocation` feature gate. |
| Helm | v3.8 or later. |
| NVIDIA Driver | v565 or later for GPU allocation. v570.158.01 or later if using [ComputeDomains](#computedomains-additional-prerequisites). |
| CDI  | Enabled in your container runtime. This is enabled by default in containerd 2.0+ and CRIO v1.27+. The DRA Driver uses CDI to expose GPUs to containers.  |
| Node Feature Discovery (NFD) | Labels GPU nodes in the cluster. The DRA Driver uses these labels to target the GPU kubelet plugin to the correct nodes. |

## ComputeDomains additional prerequisites

If you plan to use ComputeDomains, you also need:

- NVIDIA Driver v570.158.01 or later. The `IMEXDaemonsWithDNSNames` feature gate is enabled by default and requires this driver version. The ComputeDomain plugin will fail to start on older drivers unless `IMEXDaemonsWithDNSNames` is explicitly disabled.
- Multi-Node NVLink (MNNVL) hardware. Nodes must be connected via NVLink fabric, such as GB200 NVL72 and similar systems.
- GPU Feature Discovery (GFD) deployed via the [GPU Operator](#install-prerequisites-with-nvidia-gpu-operator). GFD generates the `nvidia.com/gpu.clique` node labels required by ComputeDomains.
- On all GPU nodes where the `nvidia-imex-*` packages are installed, the `nvidia-imex.service` systemd unit must be disabled:

```bash
systemctl disable --now nvidia-imex.service && systemctl mask nvidia-imex.service
```

### Host-managed IMEX (`HostManagedIMEX` feature gate)

`HostManagedIMEX` is an alpha, install-wide mode for clusters where the cluster
operator already owns the host `nvidia-imex` daemon. It **inverts** the rule
above: the driver does not run per-ComputeDomain IMEX daemons, so the host
`nvidia-imex.service` must be **configured and running** (not masked) on every
participating GPU node before workloads use IMEX.

Additional prerequisites when `featureGates.HostManagedIMEX=true`:

- Host `nvidia-imex` installed and `nvidia-imex.service` **enabled and running**,
  with a consistent `/etc/nvidia-imex/nodes_config.cfg` across the IMEX domain.
- The `nvidia-caps-imex-channels` device major must be registered (in
  `/proc/devices`) and channel `0` usable **before the kubelet plugin starts**
  (the plugin discovers the major at startup and does not republish later).
- `IMEXDaemonsWithDNSNames` and `ComputeDomainCliques` are forced off
  automatically when the gate is enabled; no manual override is needed.

Only `allocationMode: Single` (or unset) is supported, and at most one active
isolated ComputeDomain per host IMEX domain. ComputeDomains should be created
with `numNodes: 0`.

## Install prerequisites with NVIDIA GPU Operator

The [NVIDIA GPU Operator](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/index.html) is a Kubernetes operator that automates the deployment and lifecycle management of all NVIDIA software components needed to provision and monitor GPUs in a cluster.

It can manage the following DRA Driver for NVIDIA GPUs prerequisites for you:

- NVIDIA Driver (v565+ for GPU allocation, v570.158.01+ for ComputeDomains). The GPU Operator installs a [default driver](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/platform-support.html#gpu-operator-component-matrix) that meets the DRA Driver's prerequisites. To use a specific version, see [Common chart customization options](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/getting-started.html#common-chart-customization-options) in the GPU Operator documentation.
- CDI enabled through the NVIDIA Container Toolkit.
- Node Feature Discovery (NFD).
- GPU Feature Discovery (GFD), required for ComputeDomains.

If you choose to install the GPU Operator, follow the [DRA Driver for NVIDIA GPUs install guide](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/dra-intro-install.html) in the GPU Operator documentation. It covers installing the GPU Operator with the NVIDIA Kubernetes Device Plugin disabled and installing the DRA Driver for NVIDIA GPUs.
