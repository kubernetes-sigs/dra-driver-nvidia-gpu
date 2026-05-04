# Install

This page walks through installing the DRA Driver for NVIDIA GPUs and validating that GPU or ComputeDomain allocation is working correctly on your cluster.

Before starting, make sure all [prerequisites](prerequisites.md) are met.
If you have the NVIDIA GPU Operator installed, its recommended that you following the [GPU Operator install guide](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/dra-intro-install.html) instead.

---

## Install

1. Add the Helm repository:

```bash
helm repo add nvidia https://helm.ngc.nvidia.com/nvidia && helm repo update
```

2. Install the driver. The following commands install the DRA Driver with GPU allocation and ComputeDomain support. If you only plan to use one of the resource plugins, the other can be safely ignored without interference on your cluster. You can choose to disbale either plugin by including `--set resources.computeDomains.enabled=false` to disable ComputeDomain support, or `--set resources.gpus.enabled=false` and remove `--set gpuResourcesEnabledOverride=true` from the Helm command.

>Note, if you are installing on GKE, include `--set nvidiaDriverRoot=/home/kubernetes/bin/nvidia` to have the driver use the default NVIDIA Driver install path on GKE.


```bash
helm install nvidia-dra-driver-gpu nvidia/nvidia-dra-driver-gpu \
    --version=25.12.0 \
    --create-namespace \
    --namespace nvidia-dra-driver-gpu \
    --set gpuResourcesEnabledOverride=true
```

Example output:

```
NAME: nvidia-dra-driver-gpu
LAST DEPLOYED: Wed Apr 29 02:21:24 2026
NAMESPACE: nvidia-dra-driver-gpu
STATUS: deployed
REVISION: 1
DESCRIPTION: Install complete
TEST SUITE: None
```

Refer to [Configure Helm](#optional-configure-helm) below for more configuration options.


## Verify installation

After install, confirm all components are running and the expected DeviceClasses are registered.

1. Check that all pods are `Running` and `Ready`:

```bash
kubectl get pod -n nvidia-dra-driver-gpu
```

Example output (with GPU allocation and ComputeDomains enabled):

```
NAME                                                READY   STATUS    RESTARTS   AGE
nvidia-dra-driver-gpu-controller-<hash>-<hash>     1/1     Running   0          1m
nvidia-dra-driver-gpu-kubelet-plugin-<hash>        2/2     Running   0          1m
```

The `controller` pod runs the ComputeDomain controller (1 container). The `kubelet-plugin` pod runs two containers, one for GPU resources (`gpus`) and one for ComputeDomain resources (`compute-domains`), so it shows `2/2` when both are enabled. One `kubelet-plugin` pod appears per GPU node.

If you installed with `--set resources.computeDomains.enabled=false`, the `controller` pod will not be present and the `kubelet-plugin` pod will show `1/1`.

2. Confirm the DeviceClasses were registered:

```bash
kubectl get deviceclass
```

Example output:

```
NAME                                      AGE
compute-domain-daemon.nvidia.com           1m
compute-domain-default-channel.nvidia.com  1m
gpu.nvidia.com                             1m
mig.nvidia.com                             1m
vfio.gpu.nvidia.com                        1m
```

`gpu.nvidia.com` is used for standard GPU allocation. `mig.nvidia.com` and `vfio.gpu.nvidia.com` are registered but only usable with the appropriate hardware and configuration. The `compute-domain-*` classes are used by the ComputeDomain controller.

If you installed with only ComputeDomain support, `gpu.nvidia.com`, `mig.nvidia.com`, `vfio.gpu.nvidia.com` will not be installed.

If you installed with only GPU allocation support, `compute-domain-daemon.nvidia.com`, `compute-domain-default-channel.nvidia.com` will not be installed.

3. Confirm GPU nodes have advertised their ResourceSlices:

```bash
kubectl get resourceslice -o wide
```

Example output:

```
NAME                                              NODE          DRIVER                      POOL          AGE
00-gpu.nvidia.com-worker-gpu-01-kx9f2             worker-gpu-01 gpu.nvidia.com              worker-gpu-01 3m
00-compute-domain.nvidia.com-worker-gpu-01-ab3d7  worker-gpu-01 compute-domain.nvidia.com   worker-gpu-01 3m
```

The ResourceSlice name is auto-generated from the driver name, node name, and a random suffix.
The pool name matches the node name, since each node gets its own pool.

When GPU allocation support is enabled, each GPU node should appear with `gpu.nvidia.com` slices listing its available devices.

When ComputeDomain support is enabled, each GPU node should also appear with `compute-domain.nvidia.com` slices listing
its available IMEX daemon and channel devices.

If no slices appear, the kubelet plugin is not communicating with the API server.
Check that the driver pods are running and your GPUs are in a healthy state.

```bash
kubectl logs nvidia-dra-driver-gpu-kubelet-plugin-<hash> -n nvidia-dra-driver-gpus
```

For additional help, consider filing an [issue in the DRA Driver repository](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/issues).

## Optional: Configure Helm

The following parameters are most commonly set at install time.

| Parameter | Default | Description |
|---|---|---|
| `nvidiaDriverRoot` | `/` | Path to the GPU driver root on the host. If the NVIDIA GPU Operator manages the NVIDIA GPU driver on your nodes, use `/run/nvidia/driver`, the default location for Operator managed drivers. For GKE, use `/home/kubernetes/bin/nvidia` Incorrect values are a common source of error. |
| `resources.gpus.enabled` | `true` | Enable the GPU kubelet plugin. Requires `gpuResourcesEnabledOverride=true`. |
| `resources.computeDomains.enabled` | `true` | Enable the ComputeDomain controller and kubelet plugin. |
| `gpuResourcesEnabledOverride` | `false` | Required to enable GPU allocation resources. |
| `featureGates` | `{}` | Map of feature gate name to boolean. See [Feature gates](reference/feature-gates.md). |
| `logVerbosity` | `4` | Log verbosity level (0–7). Higher values produce more output. |

To list all available parameters:

```bash
helm show values nvidia/nvidia-dra-driver-gpu
```

---

## Optional: Admission webhook

The admission webhook validates opaque configuration in `ResourceClaim` and `ResourceClaimTemplate` specs, providing early feedback on invalid values. It is disabled by default.

Prerequisite: [cert-manager](https://cert-manager.io/) must be installed in your cluster.

1. Install cert-manager:

```bash
helm install \
    --repo https://charts.jetstack.io \
    --version v1.16.3 \
    --create-namespace \
    --namespace cert-manager \
    --wait \
    --set crds.enabled=true \
    cert-manager \
    cert-manager
```

2. Enable the webhook:

```bash
helm install nvidia-dra-driver-gpu nvidia/nvidia-dra-driver-gpu \
    --version=25.12.0 \
    --create-namespace \
    --namespace nvidia-dra-driver-gpu \
    --set gpuResourcesEnabledOverride=true \
    --set webhook.enabled=true
```

To use a pre-existing TLS secret instead of cert-manager, set `webhook.tls.mode=secret` and provide `webhook.tls.secret.name` and `webhook.tls.secret.caBundle`.


## Run a sample GPU allocation workload

This section inlcude steps for deploying a sample application for GPU allocation and ComputeDomains on your cluster.
For additional examples, refer to the `/demo folder`[https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/tree/main/demo] in the repository.

> **Note:** GPU resource allocation must be enabled at install time (`--set gpuResourcesEnabledOverride=true`). If you installed with `--set resources.gpus.enabled=false`, skip this section.

1. Create a namespace for the test workload:

```bash
kubectl create namespace dra-gpu-share-test
```

Example output:

```
namespace/dra-gpu-share-test created
```

2. Create a `ResourceClaimTemplate` like the following example. This defines the type of GPU resource to request, a single device from the `gpu.nvidia.com` device class. When a pod references this template, Kubernetes creates a per-pod `ResourceClaim` from it:

```yaml
apiVersion: resource.k8s.io/v1         # Kubernetes 1.34+
# apiVersion: resource.k8s.io/v1beta2  # Kubernetes 1.32 and 1.33
kind: ResourceClaimTemplate
metadata:
  namespace: dra-gpu-share-test
  name: single-gpu
spec:
  spec:
    devices:
      requests:
      - name: gpu
        exactly:
          deviceClassName: gpu.nvidia.com
```

3. Apply the manifest:

```bash
kubectl apply -f dra-gpu-share-claim-template.yaml
```

Example output:

```
resourceclaimtemplate.resource.k8s.io/single-gpu created
```

4. Create the test pod in `dra-gpu-share-pod.yaml`. Both containers (`ctr0` and `ctr1`) reference the same claim (`shared-gpu`), demonstrating that DRA allows multiple containers within a pod to share a single GPU:

```yaml
apiVersion: v1
kind: Pod
metadata:
  namespace: dra-gpu-share-test
  name: pod
  labels:
    app: pod
spec:
  containers:
  - name: ctr0
    image: ubuntu:22.04
    command: ["bash", "-c"]
    args: ["nvidia-smi -L; trap 'exit 0' TERM; sleep 9999 & wait"]
    resources:
      claims:
      - name: shared-gpu
  - name: ctr1
    image: ubuntu:22.04
    command: ["bash", "-c"]
    args: ["nvidia-smi -L; trap 'exit 0' TERM; sleep 9999 & wait"]
    resources:
      claims:
      - name: shared-gpu
  resourceClaims:
  - name: shared-gpu
    resourceClaimTemplateName: single-gpu
  tolerations:
  - key: "nvidia.com/gpu"
    operator: "Exists"
    effect: "NoSchedule"
```

5. Apply the manifest:

```bash
kubectl apply -f dra-gpu-share-pod.yaml
```

Example output:

```
pod/pod created
```

6. Verify both containers use the same GPU:

```bash
kubectl logs pod -n dra-gpu-share-test --all-containers --prefix
```

Example output shows the same GPU UUID from both containers:

```
[pod/pod/ctr0] GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-4404041a-04cf-1ccf-9e70-f139a9b1e23c)
[pod/pod/ctr1] GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-4404041a-04cf-1ccf-9e70-f139a9b1e23c)
```

7. Clean up:

```bash
kubectl delete -f dra-gpu-share-pod.yaml -f dra-gpu-share-claim-template.yaml
kubectl delete namespace dra-gpu-share-test
```

Example output:

```
pod "pod" deleted
resourceclaimtemplate.resource.k8s.io "single-gpu" deleted
namespace "dra-gpu-share-test" deleted
```

---

## Run a sample ComputeDomain workload

> **Note:** This section requires Multi-Node NVLink (MNNVL) hardware.

1. Validate clique node labels. GPU Feature Discovery labels each MNNVL-capable node with `nvidia.com/gpu.clique`. Confirm all expected nodes have this label:

```bash
(echo -e "NODE\tLABEL\tCLIQUE"; kubectl get nodes -o json | \
    jq -r '.items[] | [.metadata.name, "nvidia.com/gpu.clique", .metadata.labels["nvidia.com/gpu.clique"]] | @tsv') | \
    column -t
```

Example output:

```
NODE           LABEL                    CLIQUE
gpu-node-001   nvidia.com/gpu.clique    a1b2c3d4-e5f6-7890-abcd-ef1234567890.0
gpu-node-002   nvidia.com/gpu.clique    a1b2c3d4-e5f6-7890-abcd-ef1234567890.0
```

Each value should have the shape `<CLUSTER_UUID>.<CLIQUE_ID>`. If any nodes are missing the label, confirm that GPU Feature Discovery is deployed and running on the affected nodes.

2. Create a `ComputeDomain`. This groups nodes connected via NVLink fabric and provisions the IMEX channels needed for cross-node GPU communication. The `channel.resourceClaimTemplate` field names a `ResourceClaimTemplate` that the controller creates automatically, which pods then use to claim a channel:

```bash
cat <<EOF > imex-compute-domain.yaml
apiVersion: resource.nvidia.com/v1beta1
kind: ComputeDomain
metadata:
  name: imex-channel-injection
spec:
  numNodes: 0
  channel:
    resourceClaimTemplate:
      name: imex-channel-0
EOF
kubectl apply -f imex-compute-domain.yaml
```

Example output:

```
computedomain.resource.nvidia.com/imex-channel-injection created
```

3. Create the test pod. `nodeAffinity` restricts scheduling to nodes labeled `nvidia.com/gpu.clique`, and the pod claims the IMEX channel provisioned by the `ComputeDomain`:

```bash
cat <<EOF > imex-test-pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: imex-channel-injection
spec:
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions:
          - key: nvidia.com/gpu.clique
            operator: Exists
  containers:
  - name: ctr
    image: ubuntu:22.04
    command: ["bash", "-c"]
    args: ["ls -la /dev/nvidia-caps-imex-channels; trap 'exit 0' TERM; sleep 9999 & wait"]
    resources:
      claims:
      - name: imex-channel-0
  resourceClaims:
  - name: imex-channel-0
    resourceClaimTemplateName: imex-channel-0
EOF
kubectl apply -f imex-test-pod.yaml
```

Example output:

```
pod/imex-channel-injection created
```

4. Verify IMEX channel injection:

```bash
kubectl logs imex-channel-injection
```

Example output should list one or more channel device files under `/dev/nvidia-caps-imex-channels`:

```
total 0
drwxr-xr-x 2 root root  60 ...
crw-rw-rw- 1 root root 507, 0 ... channel0
```

5. Clean up:

```bash
kubectl delete -f imex-test-pod.yaml -f imex-compute-domain.yaml
```

Example output:

```
pod "imex-channel-injection" deleted
computedomain.resource.nvidia.com "imex-channel-injection" deleted
```
