# Prerequisites

Before installing the DRA Driver, ensure the following requirements are met:

- Kubernetes cluster running Kubernetes v1.32 or later.
- One or more NVIDIA GPUs available in at least one node in the cluster.
- Helm v3 or later.
- `DynamicResourceAllocation` feature gate enabled. Enabled by default in Kubernetes v1.34+. In v1.32 and v1.33, it must be enabled manually. See [Enable DRA on Kubernetes v1.32 and v1.33](#enable-dra-on-kubernetes-v132-and-v133).

> Note: A recommended way to manage the following prerequisites is to [install the NVIDIA GPU Operator](#install-nvidia-gpu-operator).

- NVIDIA Driver v565 or later for GPU allocation, or v570.158.01 or later for ComputeDomains (see [ComputeDomains additional prerequisites](#computedomains-additional-prerequisites)). If you want to manage your own NVIDIA Driver installation, include the `--set driver.enabled=false` flag in the GPU Operator install command below and refer to the [NVIDIA Driver Installation Guide](https://docs.nvidia.com/datacenter/tesla/driver-installation-guide/index.html) for driver install steps.
- NVIDIA Container Toolkit: installs and configures the container runtime on each node, including enabling Container Device Interface (CDI) support, which the DRA Driver uses to expose GPUs to containers.
- Node Feature Discovery (NFD): labels GPU nodes in the cluster. The DRA Driver uses these labels to target the GPU plugin to the correct nodes.
- GPU Feature Discovery (GFD): generates GPU topology labels on each node, including the `nvidia.com/gpu.clique` labels required by ComputeDomains. Required if you plan to use ComputeDomains.

## Install Prerequistes with NVIDIA GPU Operator

The [NVIDIA GPU Operator](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/index.html) is a Kubernetes operator that automates the deployment and lifecycle management of all NVIDIA software components needed to provision and monitor GPUs in a cluster. 

It can manage the following DRA Driver for NVIDIA GPUs prerequisites for you:

- NVIDIA Driver v565 or later for GPU allocation, or v570.158.01 or later for ComputeDomains (see [ComputeDomains additional prerequisites](#computedomains-additional-prerequisites)).
  The GPU Operator installs a [default driver](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/platform-support.html#gpu-operator-component-matrix) that meets the DRA Driver's prerequistes, or you can [configure it to install a specific driver](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/getting-started.html#common-chart-customization-options) that meets the prerequites.
- NVIDIA Container Toolkit
- Node Feature Discovery (NFD)
- GPU Feature Discovery (GFD) (for ComputeDomains)

If you choose to install the GPU Operator, refer to the [DRA Driver for NVIDA GPUs install guide] (https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/dra-intro-install.html) in the GPU Operator documentation.
This covers installing the GPU Operator with the NVIDIA Kubernetes Device Plugin (required when using the DRA Driver for GPU allocation) and installing the DRA Driver for NVIDIA GPUs.

## ComputeDomains additional prerequisites

If you plan to use ComputeDomains, you also need:

- NVIDIA Driver v570.158.01 or later. The `IMEXDaemonsWithDNSNames` feature gate is enabled by default and requires this driver version. The ComputeDomain plugin will fail to start on older drivers unless `IMEXDaemonsWithDNSNames` is explicitly disabled.
- Multi-Node NVLink (MNNVL) hardware. Nodes must be connected via NVLink fabric, such as GB200 NVL72 or H100 NVLink configurations.
- GPU Feature Discovery (GFD) deployed via the GPU Operator (see above). It generates the `nvidia.com/gpu.clique` node labels required by ComputeDomains.
- If the `nvidia-imex-*` packages are installed, the `nvidia-imex.service` systemd unit must be disabled on all GPU nodes:

```bash
systemctl disable --now nvidia-imex.service && systemctl mask nvidia-imex.service
```

## Enable DRA on Kubernetes v1.32 and v1.33

On Kubernetes v1.34 and later, `DynamicResourceAllocation` is enabled by default and no additional configuration is required.

On Kubernetes v1.32 and v1.33, enable the following on each component:

| Component | Requirement |
|---|---|
| kube-apiserver | Enable the `DynamicResourceAllocation` feature gate and the `resource.k8s.io/v1beta1` and `resource.k8s.io/v1beta2` API groups |
| kube-controller-manager | Enable the `DynamicResourceAllocation` feature gate |
| kube-scheduler | Enable the `DynamicResourceAllocation` feature gate |
| kubelet | Enable the `DynamicResourceAllocation` feature gate |

How you apply these depends on your cluster setup. For managed Kubernetes distributions (EKS, GKE, AKS, and others), refer to your provider's documentation. Not all providers support enabling `DynamicResourceAllocation` on v1.32 or v1.33 clusters.

### Example: kubeadm

The following `kubeadm-init.yaml` enables DRA for a new cluster using [kubeadm](https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/control-plane-flags/):

```yaml
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
apiServer:
  extraArgs:
  - name: "feature-gates"
    value: "DynamicResourceAllocation=true"
  - name: "runtime-config"
    value: "resource.k8s.io/v1beta1=true,resource.k8s.io/v1beta2=true"
controllerManager:
  extraArgs:
  - name: "feature-gates"
    value: "DynamicResourceAllocation=true"
scheduler:
  extraArgs:
  - name: "feature-gates"
    value: "DynamicResourceAllocation=true"
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
featureGates:
  DynamicResourceAllocation: true
```
