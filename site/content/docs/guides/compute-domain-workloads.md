---
title: ComputeDomain workloads
linkTitle: ComputeDomain workloads
weight: 30
description: Create a ComputeDomain, claim a channel, and run a Multi-Node NVLink workload.
---

For background on what a `ComputeDomain` is and how it fits together, see
[ComputeDomains](../concepts/compute-domains.md).

## Prerequisites

Refer to [Prerequisites](../prerequisites.md) for hardware and software requirements, including the ComputeDomain-specific requirements for Multi-Node NVLink hardware, GPU Feature Discovery, and `nvidia-imex` service configuration.

## Create a ComputeDomain

The minimal `ComputeDomain` spec requires only the name of the `ResourceClaimTemplate` the controller will create for channel allocation:

```yaml
apiVersion: resource.nvidia.com/v1beta1
kind: ComputeDomain
metadata:
  name: my-compute-domain
spec:
  numNodes: 0
  channel:
    resourceClaimTemplate:
      name: imex-channel-0
```

`numNodes` is deprecated. Set it to `0` (the recommended value when `IMEXDaemonsWithDNSNames` is enabled — its default state).

After applying this resource, the controller creates:

- A per-domain `DaemonSet` of `compute-domain-daemon` pods, one per GPU node.
- A `ResourceClaimTemplate` named `imex-channel-0` (or whatever name you gave it), which workload pods use to request a channel.

## Use the channel in a workload

Reference the `ResourceClaimTemplate` name you set in `spec.channel.resourceClaimTemplate.name` when writing your workload:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-workload
spec:
  containers:
  - name: app
    image: my-image
    command: ["bash", "-c"]
    args: ["ls -la /dev/nvidia-caps-imex-channels; sleep 9999"]
    resources:
      claims:
      - name: imex-channel-0
  resourceClaims:
  - name: imex-channel-0
    resourceClaimTemplateName: imex-channel-0
```

The pod will not start until the local IMEX daemon is ready.

### Channel allocation modes

The `spec.channel.allocationMode` field controls how many IMEX channels are injected:

| Mode | Value | Description |
|---|---|---|
| Single | `Single` (default) | Injects a single IMEX channel into the workload container |
| All | `All` | Injects all available IMEX channels (up to the hardware maximum) |

Use `All` for workloads that need access to every channel in the IMEX domain.

## Check status

```bash
kubectl get computedomain my-compute-domain -o yaml
```

The `status.status` field reports `Ready` when all expected IMEX daemons have joined. The `status.nodes` list shows each node's IP, clique ID, and individual daemon status.

To see the `ComputeDomainClique` objects created for this domain:

```bash
kubectl get computedomainclique -n dra-driver-nvidia-gpu
```

## Feature gates

Both feature gates that affect ComputeDomains are Beta and enabled by default. You do not need to set them for standard operation.

| Feature gate | Stage | Default | Effect |
|---|---|---|---|
| `IMEXDaemonsWithDNSNames` | Beta | `true` | Daemons communicate using DNS names instead of raw IP addresses. This is the recommended mode and required by `ComputeDomainCliques`. |
| `ComputeDomainCliques` | Beta | `true` | Uses `ComputeDomainClique` CRD objects to track daemon membership per clique instead of storing that information in `ComputeDomain.status.nodes`. Requires `IMEXDaemonsWithDNSNames`. |

To disable a Beta gate (for example, to test a downgrade path):

```yaml
featureGates:
  ComputeDomainCliques: false
  IMEXDaemonsWithDNSNames: false
```

See [Feature gates](../reference/feature-gates/) for all available gates.

## Multi-node `nvbandwidth` test (with MPI)

A two-node [`nvbandwidth`](https://github.com/NVIDIA/nvbandwidth) test that consumes four GPUs on each node, run through the [MPI Operator](https://github.com/kubeflow/mpi-operator). This validates that `ComputeDomain` channels work correctly across nodes under real inter-GPU traffic.

1. Install the MPI Operator:

```bash
kubectl create -f https://github.com/kubeflow/mpi-operator/releases/download/v0.6.0/mpi-operator.yaml
```

2. Create the spec file. The `MPIJob` worker pods use `podAffinity` on the `nvidia.com/gpu.clique` topology key so both workers land in the same NVLink domain:

```yaml
cat <<EOF > nvbandwidth-test-job.yaml
---
apiVersion: resource.nvidia.com/v1beta1
kind: ComputeDomain
metadata:
  name: nvbandwidth-test-compute-domain
spec:
  numNodes: 0
  channel:
    resourceClaimTemplate:
      name: nvbandwidth-test-compute-domain-channel
---
apiVersion: kubeflow.org/v2beta1
kind: MPIJob
metadata:
  name: nvbandwidth-test
spec:
  slotsPerWorker: 4
  launcherCreationPolicy: WaitForWorkersReady
  runPolicy:
    cleanPodPolicy: Running
  sshAuthMountPath: /home/mpiuser/.ssh
  mpiReplicaSpecs:
    Launcher:
      replicas: 1
      template:
        metadata:
          labels:
            nvbandwidth-test-replica: mpi-launcher
        spec:
          affinity:
            nodeAffinity:
              requiredDuringSchedulingIgnoredDuringExecution:
                nodeSelectorTerms:
                - matchExpressions:
                  - key: node-role.kubernetes.io/control-plane
                    operator: Exists
          containers:
          - image: ghcr.io/nvidia/k8s-samples:nvbandwidth-v0.7-8d103163
            name: mpi-launcher
            securityContext:
              runAsUser: 1000
            command:
            - mpirun
            args:
            - --bind-to
            - core
            - --map-by
            - ppr:4:node
            - -np
            - "8"
            - --report-bindings
            - -q
            - nvbandwidth
            - -t
            - multinode_device_to_device_memcpy_read_ce
    Worker:
      replicas: 2
      template:
        metadata:
          labels:
            nvbandwidth-test-replica: mpi-worker
        spec:
          affinity:
            podAffinity:
              requiredDuringSchedulingIgnoredDuringExecution:
              - labelSelector:
                  matchExpressions:
                  - key: nvbandwidth-test-replica
                    operator: In
                    values:
                    - mpi-worker
                topologyKey: nvidia.com/gpu.clique
          containers:
          - image: ghcr.io/nvidia/k8s-samples:nvbandwidth-v0.7-8d103163
            name: mpi-worker
            securityContext:
              runAsUser: 1000
            command:
            - /usr/sbin/sshd
            args:
            - -De
            - -f
            - /home/mpiuser/.sshd_config
            resources:
              limits:
                nvidia.com/gpu: 4
              claims:
              - name: compute-domain-channel
          resourceClaims:
          - name: compute-domain-channel
            resourceClaimTemplateName: nvbandwidth-test-compute-domain-channel
EOF
kubectl apply -f nvbandwidth-test-job.yaml
```

3. Inspect results. The launcher log should print an `nvbandwidth` matrix of device-to-device bandwidth across both nodes:

```bash
kubectl logs --tail=-1 -l job-name=nvbandwidth-test-launcher
```

Example output:

```
Running multinode_device_to_device_memcpy_read_ce.
memcpy CE GPU(row) -> GPU(column) bandwidth (GB/s)
           0         1         2         3         4         5         6         7
 0       N/A    798.02    798.25    798.02    798.02    797.88    797.73    797.95
 ...
SUM multinode_device_to_device_memcpy_read_ce 44685.29
```

4. Clean up:

```bash
kubectl delete -f nvbandwidth-test-job.yaml
```