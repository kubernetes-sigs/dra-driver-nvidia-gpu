---
title: Install the driver
linkTitle: Install
weight: 40
description: Helm install steps for the DRA driver.
---

Install the DRA Driver for NVIDIA GPUs and validate that GPU or ComputeDomain allocation is working on your cluster.

Before you begin:

- Confirm all [prerequisites](prerequisites.md) are met.
- If you have the NVIDIA GPU Operator installed, follow the [GPU Operator install guide](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/dra-intro-install.html) instead.

---

## Install the chart

The following command installs the DRA Driver with both GPU allocation and ComputeDomain support enabled.

By default, both resource plugins are enabled. If you only need one, the other can be left enabled with no impact on the cluster, or you can disable it explicitly:

- To disable ComputeDomain support, add `--set resources.computeDomains.enabled=false`.
- To disable GPU allocation, add `--set resources.gpus.enabled=false`.

{{% alert title="Note" %}}
On GKE, include `--set nvidiaDriverRoot=/home/kubernetes/bin/nvidia` so the driver uses the default NVIDIA driver install path on GKE.
{{% /alert %}}

```bash
helm install dra-driver-nvidia-gpu oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
    --version {{< param "driver_version" >}} \
    --create-namespace \
    --namespace dra-driver-nvidia-gpu \
    --set gpuResourcesEnabledOverride=true
```

Example output:

```
NAME: dra-driver-nvidia-gpu
LAST DEPLOYED: Wed Apr 29 02:21:24 2026
NAMESPACE: dra-driver-nvidia-gpu
STATUS: deployed
REVISION: 1
DESCRIPTION: Install complete
TEST SUITE: None
```

For additional configuration options, see [Optional: Configure Helm values](#optional-configure-helm-values).

## Verify installation

After install, confirm all components are running and the expected DeviceClasses are registered.

1. Check that all pods are `Running` and `Ready`:

```bash
kubectl get pod -n dra-driver-nvidia-gpu
```

Example output (with GPU allocation and ComputeDomains enabled):

```
NAME                                                READY   STATUS    RESTARTS   AGE
dra-driver-nvidia-gpu-controller-7fb7956988-4kv59  1/1     Running   0          1m
dra-driver-nvidia-gpu-kubelet-plugin-5qhc7         2/2     Running   0          1m
```

The `controller` pod runs the ComputeDomain controller (1 container). The `kubelet-plugin` pod runs two containers, one for GPU resources (`gpus`) and one for ComputeDomain resources (`compute-domains`), so it shows `2/2` when both are enabled. One `kubelet-plugin` pod appears per GPU node.

If you installed with `--set resources.computeDomains.enabled=false`, the `controller` pod will not be present and the `kubelet-plugin` pod will show `1/1`. The same is true if you disabled GPU allocation during install.

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
kubectl logs dra-driver-nvidia-gpu-kubelet-plugin-<hash> -n dra-driver-nvidia-gpu
```

For additional help, consider filing an [issue in the DRA Driver repository](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/issues).

## Optional: Configure Helm values

The following parameters are most commonly set at install time.

| Parameter | Default | Description |
|---|---|---|
| `nvidiaDriverRoot` | `/` | Path to the GPU driver root on the host. If the NVIDIA GPU Operator manages the NVIDIA GPU driver on your nodes, set to `/run/nvidia/driver`, the default location for Operator managed drivers. For GKE, use `/home/kubernetes/bin/nvidia`. Incorrect values are a common error during install. |
| `resources.gpus.enabled` | `true` | Enable the GPU kubelet plugin. Requires `gpuResourcesEnabledOverride=true`. |
| `resources.computeDomains.enabled` | `true` | Enable the ComputeDomain controller and kubelet plugin. |
| `gpuResourcesEnabledOverride` | `false` | Required to enable GPU allocation resources. |
| `featureGates` | `{}` | Map of feature gate name to boolean. See [Feature gates](reference/feature-gates/) for available feature gates. |
| `logVerbosity` | `4` | Log verbosity level (0–7). Higher values produce more output. |

For the full parameter list, see [Helm chart values](reference/helm-values/). To dump values from the chart:

```bash
helm show values oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu --version {{< param "driver_version" >}}
```

## Optional: Admission webhook

The admission webhook validates opaque configuration in `ResourceClaim` and `ResourceClaimTemplate` specs, providing early feedback on invalid values. It is disabled by default.
Refer to the [API reference](reference/api/) for more details on this configuration.

Prerequisite: [cert-manager](https://cert-manager.io/) must be installed in your cluster.

1. Install cert-manager:

```bash
helm install \
  cert-manager oci://quay.io/jetstack/charts/cert-manager \
  --namespace cert-manager \
  --create-namespace \
  --set crds.enabled=true \
  --wait
```

2. Enable the webhook by including `--set webhook.enabled=true` in the Helm install command. To use a pre-existing TLS secret instead of cert-manager, set `webhook.tls.mode=secret` and provide `webhook.tls.secret.name` and `webhook.tls.secret.caBundle`.

```bash
helm install dra-driver-nvidia-gpu oci://registry.k8s.io/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
    --version {{< param "driver_version" >}} \
    --create-namespace \
    --namespace dra-driver-nvidia-gpu \
    --set gpuResourcesEnabledOverride=true \
    --set webhook.enabled=true
```

Example output:

```
NAME: dra-driver-nvidia-gpu
LAST DEPLOYED: Wed Apr 29 02:21:24 2026
NAMESPACE: dra-driver-nvidia-gpu
STATUS: deployed
REVISION: 1
DESCRIPTION: Install complete
TEST SUITE: None
```

3. Optionally: Verify webhook pod is `Running` and `Ready`:

```bash
kubectl get pod -n dra-driver-nvidia-gpu
```

Example output:

```
NAME                                                READY   STATUS    RESTARTS   AGE
dra-driver-nvidia-gpu-controller-7fb7956988-4kv59   1/1     Running   0          25s
dra-driver-nvidia-gpu-kubelet-plugin-5qhc7          2/2     Running   0          25s
dra-driver-nvidia-gpu-webhook-6c9dd4956d-r4r7z      1/1     Running   0          25s
```

## Optional: Install a development build

Release builds land at `registry.k8s.io/dra-driver-nvidia`. Between releases, every merge to `main` (and to `release-*` branches) is also built and pushed to the staging registry `us-central1-docker.pkg.dev/k8s-staging-images/dra-driver-nvidia`, tagged `<VERSION>-<short-SHA>` (for example `v0.5.0-dev-a1b2c3d4`), where `<VERSION>` comes from the repository's [`VERSION`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/main/VERSION) file at that commit.

{{% alert title="Note" %}}
Staging images and charts are unversioned, unsupported builds meant for testing a specific commit. Do not run them in production.
{{% /alert %}}

1. Find the short SHA of the commit you want to test, and read the `VERSION` file at that commit:

```bash
COMMIT="$(git ls-remote https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu.git refs/heads/main | cut -c1-8)"
VERSION="$(curl -sSfL https://raw.githubusercontent.com/kubernetes-sigs/dra-driver-nvidia-gpu/main/VERSION)"
TAG="${VERSION}-${COMMIT}"
echo "installing: ${TAG}"
```

2. Install (or upgrade) using the staging chart at that tag:

```bash
helm upgrade -i dra-driver-nvidia-gpu \
    oci://us-central1-docker.pkg.dev/k8s-staging-images/dra-driver-nvidia/charts/dra-driver-nvidia-gpu \
    --version "${TAG#v}" \
    --namespace dra-driver-nvidia-gpu \
    --create-namespace \
    --set gpuResourcesEnabledOverride=true \
    --wait
```

Append any other `--set` flags you'd normally use (`nvidiaDriverRoot`, `resources.computeDomains.enabled=false`, and so on).

## Run a sample GPU allocation workload

Deploy a sample workload that allocates a GPU through the DRA Driver and verifies it is shared correctly between containers. For additional examples, see the [`demo/` folder](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/tree/main/demo) in the repository.

{{% alert title="Note" %}}
GPU resource allocation must be enabled at install time (`--set gpuResourcesEnabledOverride=true`). If you installed with `--set resources.gpus.enabled=false`, skip this section.
{{% /alert %}}

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

Deploy a sample workload that provisions an IMEX channel across NVLink-connected nodes and verifies the channel device is injected into the pod.

{{% alert title="Note" %}}
This section requires Multi-Node NVLink (MNNVL) hardware.
{{% /alert %}}

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
