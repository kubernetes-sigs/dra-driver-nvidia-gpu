---
title: Troubleshooting
linkTitle: Troubleshooting
weight: 60
description: Collect diagnostic data and tune log verbosity when something goes wrong.
---

## Collecting data

### Kubelet plugin logs

Collect all kubelet plugin logs into a single file:

```bash
kubectl logs \
    -n dra-driver-nvidia-gpu \
    -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    --prefix \
    --all-containers \
    --timestamps \
    --tail=-1 \
    > dra-driver-dbg_plugins_$(date -u +"%Y-%m-%dT%H%M%SZ").log
```

In a larger-scale cluster this can fetch a lot of data. Keep `--prefix` and `--timestamps` — they're critical for correlating events across pods.

### ComputeDomain daemon logs (for a specific ComputeDomain)

Paste this shell function into your terminal:

```bash
get_all_cd_daemon_logs_for_cd_name() {
  if [ -z "$*" ]; then echo "missing arg: ComputeDomain name"; return 1; fi
  CD_NAME="$1"
  CD_UID=$(kubectl describe computedomains.resource.nvidia.com "${CD_NAME}" | grep UID | awk '{print $2}')
  CD_LABEL_KV="resource.nvidia.com/computeDomain=${CD_UID}"
  _filename="dra-driver-dbg_cd-daemons_$(date -u +"%Y-%m-%dT%H%M%SZ").log.gz"
  echo "fetching CD daemon logs for CD: $CD_LABEL_KV ($CD_NAME), creating $_filename"
  kubectl logs \
    -n dra-driver-nvidia-gpu \
    -l "${CD_LABEL_KV}" \
    --all-containers \
    --timestamps \
    --tail=-1 \
    --prefix | gzip > "${_filename}"
}
```

Run it for a specific `ComputeDomain`:

```bash
get_all_cd_daemon_logs_for_cd_name imex-channel-injection
```

## Controlling log verbosity

### At install time

Log verbosity can be set for all components using the `--set logVerbosity=<V>` parameter during `helm install` or `helm upgrade -i`.

### Post-install

Verbosity can be changed after deployment, per component, using the mechanisms below. None of the components pick up verbosity changes at runtime — a pod restart is always required.

**Controller:**

```bash
kubectl set env deployment dra-driver-nvidia-gpu-controller -n dra-driver-nvidia-gpu LOG_VERBOSITY=6
```

This restarts the controller pod.

**Kubelet plugins:**

```bash
kubectl set env ds dra-driver-nvidia-gpu-kubelet-plugin -n dra-driver-nvidia-gpu LOG_VERBOSITY=6
```

This restarts all kubelet plugin pods.

**ComputeDomain daemons:**

Set the verbosity of daemons started *in the future* (this restarts the controller pod):

```bash
kubectl set env deployment dra-driver-nvidia-gpu-controller -n dra-driver-nvidia-gpu LOG_VERBOSITY_CD_DAEMON=6
```

## Filing an issue

If you can't resolve the problem, open an [issue in the DRA Driver repository](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/issues) and attach the logs collected above.
