---
title: ComputeDomain workloads
linkTitle: ComputeDomain workloads
weight: 30
description: Create a ComputeDomain, claim a channel, and run a Multi-Node NVLink workload.
---

For background on what a `ComputeDomain` is and how it fits together, see
[ComputeDomains](../concepts/compute-domains.md).

## Prerequisites

Refer to [Prerequisites](../prerequisites.md) for hardware and software requirements, including the ComputeDomain-specific requirements for Multi-Node NVLink hardware, `nvidia.com/gpu.clique` label ownership, and `nvidia-imex` service configuration.

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
- A `ResourceClaimTemplate` named `imex-channel-0`, which workload pods use to request a channel.

These objects describe the default `driverManaged` mode.
In `hostManaged` mode, the controller creates the workload `ResourceClaimTemplate` but does not create the per-domain daemon DaemonSet.
The host service must be ready through its configured command socket before a workload claim can prepare.
Host-managed mode currently provides domain isolation and channel 0 only, even if the `ComputeDomain` requests `allocationMode: All`.
See [Host-managed IMEX](../prerequisites.md#host-managed-imex) before you select this mode.

## Use the channel in a workload

Reference the `ResourceClaimTemplate` name you set in `spec.channel.resourceClaimTemplate.name` when writing your workload:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-workload
spec:
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions:
          - key: nvidia.com/gpu.clique
            operator: Exists
  containers:
  - name: app
    image: ubuntu:22.04
    command: ["bash", "-c"]
    args:
    - |
      set -eu
      test -c /dev/nvidia-caps-imex-channels/channel0
      echo "IMEX channel0 is available"
      ls -la /dev/nvidia-caps-imex-channels
      sleep 9999
    resources:
      claims:
      - name: imex-channel-0
  resourceClaims:
  - name: imex-channel-0
    resourceClaimTemplateName: imex-channel-0
```

The required node affinity prevents the workload from being used as a false-positive
test on a node that is not part of an NVLink clique.
The pod will not start until the local IMEX daemon is ready, and its startup command
fails unless `channel0` is a character device.
After the pod starts, verify the device explicitly; `Running` by itself is not a
successful IMEX validation:

```bash
kubectl exec my-workload -- test -c /dev/nvidia-caps-imex-channels/channel0
kubectl logs my-workload
```

The first command must exit successfully, and the logs must include
`IMEX channel0 is available`.

### Channel allocation modes

In `driverManaged` mode, the `spec.channel.allocationMode` field controls how many
IMEX channels are injected:

| Mode | Value | Description |
|---|---|---|
| Single | `Single` (default) | Injects a single IMEX channel into the workload container |
| All | `All` | Injects all available IMEX channels (up to the hardware maximum) |

Use `All` only in `driverManaged` mode for workloads that need access to every
channel in the IMEX domain.
In `hostManaged` mode, domain isolation shares channel 0 among workloads.
The controller forces the generated workload claim to `Single`, so a requested
`All` is equivalent to `Single` when using host-managed mode and does not expose additional channels.

## Check status

```bash
kubectl get computedomain my-compute-domain -o yaml
```

Interpret `status.status` according to the configured lifecycle mode:

- In `driverManaged` mode, `Ready` reflects the readiness of the per-ComputeDomain
  daemons managed by the driver. When `ComputeDomainCliques` is enabled, inspect
  the `ComputeDomainClique` objects for daemon membership:

  ```bash
  kubectl get computedomainclique -n dra-driver-nvidia-gpu
  ```

- In `hostManaged` mode, `Ready` means only that the controller admitted the
  `ComputeDomain` and created its workload `ResourceClaimTemplate`. It does not
  report the health of the administrator-managed `nvidia-imex` service.
  Host daemon health is checked later, on each node, when the kubelet plugin
  prepares a workload channel claim. Host-managed mode does not create
  `ComputeDomainClique` objects.

## Validate host-managed IMEX

Use this validation after installing the driver with both
`featureGates.HostManagedIMEXDaemon=true` and
`resources.computeDomains.imex.mode=hostManaged`.
Complete the [host-managed prerequisites](../prerequisites.md#host-managed-imex)
first, including configuring the same command socket path in the host service and
the Helm value.

1. On every node that can run the workload, verify the administrator-managed
   service and command socket:

   ```bash
   sudo systemctl is-active nvidia-imex.service
   sudo /usr/bin/nvidia-imex-ctl -q --u=/etc/nvidia-imex/imex_ctrl.sock
   ```

   The first command must report `active` and the second must report `READY`.
   Substitute the configured `resources.computeDomains.imex.hostSocketPath` when
   it is not the default.

2. Create the `ComputeDomain` shown above, then verify that the controller created
   only its workload claim template:

   ```bash
   CD_UID="$(kubectl get computedomain my-compute-domain -o jsonpath='{.metadata.uid}')"

   kubectl get resourceclaimtemplate imex-channel-0
   kubectl get daemonset --all-namespaces \
       -l "resource.nvidia.com/computeDomain=${CD_UID}"
   kubectl get resourceclaimtemplate --all-namespaces \
       -l "resource.nvidia.com/computeDomain=${CD_UID},resource.nvidia.com/computeDomainTarget=Daemon"
   ```

   The first command must return `imex-channel-0`. The two label queries must
   return no resources: host-managed mode creates neither a per-domain daemon
   DaemonSet nor a daemon `ResourceClaimTemplate`.

3. Save the clique-affined workload above as `my-workload.yaml`, apply it, and
   verify `channel0` with both `test -c` and the workload log commands shown
   after the manifest.
   A `Running` pod without that device check is not sufficient.

4. In a maintenance window on a test node, exercise readiness failure and
   recovery. Record the node used in step 3, then delete that pod:

   ```bash
   TEST_NODE="$(kubectl get pod my-workload -o jsonpath='{.spec.nodeName}')"
   echo "${TEST_NODE}"
   kubectl delete -f my-workload.yaml
   ```

   In `my-workload.yaml`, add this expression alongside the existing
   `nvidia.com/gpu.clique` expression, replacing `<test-node-name>` with the
   value printed above:

   ```yaml
   - key: kubernetes.io/hostname
     operator: In
     values:
     - <test-node-name>
   ```

   On that node, stop `nvidia-imex.service`:

   ```bash
   sudo systemctl stop nvidia-imex.service
   ```

   Reapply the same pod manifest from your administration host:

   ```bash
   kubectl apply -f my-workload.yaml
   kubectl describe pod my-workload
   kubectl get events --field-selector reason=FailedPrepareDynamicResources \
       --sort-by=.lastTimestamp
   ```

   The pod must not start while the command socket is unavailable: claim
   preparation fails and the kubelet retries. On the same node, restart the
   service and wait for the command socket to report `READY`. The same pending
   pod and claim then proceed without being recreated:

   ```bash
   sudo systemctl start nvidia-imex.service
   sudo /usr/bin/nvidia-imex-ctl -q --u=/etc/nvidia-imex/imex_ctrl.sock
   ```

   From your administration host, verify recovery and channel injection:

   ```bash
   kubectl wait --for=condition=Ready pod/my-workload --timeout=5m
   kubectl exec my-workload -- test -c /dev/nvidia-caps-imex-channels/channel0
   ```

5. Delete the workload and `ComputeDomain`. The controller removes the workload
   claim template, but it does not stop or reconfigure the host service:

   ```bash
   kubectl delete pod my-workload
   kubectl delete computedomain my-compute-domain
   ```

   Keep `nvidia-imex.service` running and manage its lifecycle outside the DRA
   Driver.

## Feature gates

The default driver-managed mode uses two Beta feature gates that are enabled by default.

| Feature gate | Stage | Default | Description |
|---|---|---|---|
| `IMEXDaemonsWithDNSNames` | Beta | `true` | Makes daemons communicate using DNS names instead of raw IP addresses and is required by `ComputeDomainCliques`. |
| `ComputeDomainCliques` | Beta | `true` | Uses `ComputeDomainClique` CRD objects to track daemon membership per clique instead of storing that information in `ComputeDomain.status.nodes` and requires `IMEXDaemonsWithDNSNames`. |
| `HostManagedIMEXDaemon` | Alpha | `false` | Allows you to select `resources.computeDomains.imex.mode=hostManaged` without changing the lifecycle mode by itself. |

To disable a Beta gate (for example, to test a downgrade path):

```yaml
featureGates:
  ComputeDomainCliques: false
  IMEXDaemonsWithDNSNames: false
```

See [Feature gates](../reference/feature-gates/) for all available gates.
