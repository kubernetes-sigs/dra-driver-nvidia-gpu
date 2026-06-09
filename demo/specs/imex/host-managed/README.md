# Host-managed IMEX — examples & runbook

These specs exercise the `HostManagedIMEX` alpha feature gate: the cluster
operator owns the host `nvidia-imex` daemon, and the driver only advertises and
injects channel 0 (no per-ComputeDomain IMEX DaemonSets). See the design and
operator guide for the full contract; see `docs/prerequisites.md` →
"Host-managed IMEX" for host prerequisites.

## Per-node prerequisites (every GPU node)

Two things must be true on every participating GPU node **before the kubelet
plugin starts**:

### 1. Host `nvidia-imex` running with a consistent node list

`nvidia-imex.service` must be **running** (not masked), reading the same
`/etc/nvidia-imex/nodes_config.cfg` on every node in the IMEX domain. The file is
one peer per line — IPv4, IPv6, or DNS name — and **must include this node's own
address** (IMEX treats itself as a peer). Every node in the domain needs an
**identical** list; if they disagree, IMEX silently refuses cross-node memory
operations.

Example `/etc/nvidia-imex/nodes_config.cfg` for a 2-node domain:

```text
10.0.0.11
10.0.0.12
```

Use the addresses the daemons actually bind/route on. The list is one entry per
node in the NVLink clique (e.g. 18 lines for a full GB200 NVL72 rack). Keep it
identical across peers and restart `nvidia-imex` after any change. See operator
guide §3.2 for discovery strategies (static file, a boot-time script that
scrapes the `nvidia.com/gpu.clique` label, a node-local controller, or BCM/NMX).

### 2. The `nvidia-caps-imex-channels` major + channel 0

The kubelet plugin reads `/proc/devices` at startup to find the
`nvidia-caps-imex-channels` device major, and workloads need channel `0`. Verify
(and create the device node if missing):

```bash
# 1. The kernel must have registered the IMEX-channels major:
grep nvidia-caps-imex-channels /proc/devices
major="$(awk '$2 == "nvidia-caps-imex-channels" {print $1}' /proc/devices)"
test -n "$major" || { echo "IMEX channel major not registered — load/fix the NVIDIA driver first"; exit 1; }

# 2. channel0 must exist and be usable:
sudo mkdir -p /dev/nvidia-caps-imex-channels
test -e /dev/nvidia-caps-imex-channels/channel0 || \
  sudo mknod /dev/nvidia-caps-imex-channels/channel0 c "$major" 0
sudo chmod 0666 /dev/nvidia-caps-imex-channels/channel0
ls -l /dev/nvidia-caps-imex-channels/channel0   # -> crw-rw-rw- ... <major>, 0 ... channel0
```

Preferred over manual `mknod` are the methods that recreate `channel0`
automatically at driver load:

- the NVIDIA kernel-module parameter `NVreg_CreateImexChannel0=1`, or
- `sudo nvidia-modprobe -c 0`.

CDI injects this device into the container from its major/minor, so a present
host node is the signal the node is set up correctly. Only channel `0` is used
by this mode.

## Install (DGXC GB200 example)

```bash
helm upgrade --install dra-driver-nvidia-gpu \
  ./deployments/helm/dra-driver-nvidia-gpu \
  -n dra-driver-nvidia-gpu --create-namespace \
  -f demo/specs/imex/host-managed/dgxc-gb200-values.yaml \
  --set image.repository=<your-multiarch-image> --set image.tag=<tag> \
  --wait
```

Verify:

```bash
kubectl -n dra-driver-nvidia-gpu get pods            # controller Running (not CrashLoop), plugins Ready
kubectl get deviceclass compute-domain-default-channel.nvidia.com   # present
kubectl get deviceclass compute-domain-daemon.nvidia.com            # NOT present
kubectl get resourceslice -A -o yaml | grep -A2 'name: channel-0'   # one per GPU node
kubectl get ds,po -A | grep computedomain-daemon                    # nothing
```

## Smoke (expect channel0)

```bash
kubectl apply -f demo/specs/imex/host-managed/smoke-channel-injection.yaml
kubectl wait --for=condition=Ready pod/hostmanaged-smoke --timeout=120s
kubectl logs hostmanaged-smoke        # -> shows channel0
kubectl delete -f demo/specs/imex/host-managed/smoke-channel-injection.yaml
```

## Negative (expect a permanent prepare error)

```bash
kubectl apply -f demo/specs/imex/host-managed/negative-allocationmode-all.yaml
kubectl describe pod hostmanaged-all  # stays ContainerCreating; permanent error re: allocationMode "Single"
kubectl logs -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=kubelet-plugin -c compute-domains --tail=50
kubectl delete -f demo/specs/imex/host-managed/negative-allocationmode-all.yaml
```

## Cross-node data path (co-clique nodes only)

`nvbandwidth`/NCCL workers need GPUs. Re-install (or upgrade) with GPU DRA on:

```bash
helm upgrade ... -f dgxc-gb200-values.yaml \
  --set resources.gpus.enabled=true --set gpuResourcesEnabledOverride=true
```

Then adapt `../nvbandwidth-test-job.yaml` for host-managed (ComputeDomain
`numNodes: 0`, `allocationMode: Single`; no daemon pods expected) and run it
across two nodes that share a `nvidia.com/gpu.clique` value.
