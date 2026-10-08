# shellcheck disable=SC2148
# shellcheck disable=SC2329

# Tests for KEP-4680 device health reporting: the GPU kubelet plugin reports
# the health of allocated devices to the kubelet, which surfaces it in
# pod.status.containerStatuses[].allocatedResourcesStatus.
#
# XIDs are injected through mock NVML's failure injection, so these tests only
# run against mock NVML. The injected failure is sticky for the lifetime of the
# NVML library in the plugin process: every test restores the mock config and
# restarts the kubelet plugin on teardown.

_mock_config_path() {
  echo "${TEST_NVIDIA_DRIVER_ROOT}/config/config.yaml"
}

# Run a shell script on the (single) node with the mock NVML config directory
# mounted. The bats runner container has no access to the node filesystem.
_run_on_mock_config_dir() {
  local _script="$1"
  local _dir _script_b64
  _dir="$(dirname "$(_mock_config_path)")"
  _script_b64="$(printf '%s' "${_script}" | base64 -w0)"
  kubectl delete pod nvml-mock-config-editor --ignore-not-found --wait=true
  kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: nvml-mock-config-editor
  labels:
    env: batssuite
spec:
  restartPolicy: Never
  containers:
  - name: ctr
    image: ubuntu:24.04
    command: ["bash", "-c", "echo ${_script_b64} | base64 -d | bash -ex"]
    volumeMounts:
    - name: config
      mountPath: /config
  volumes:
  - name: config
    hostPath:
      path: ${_dir}
      type: Directory
EOF
  kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/nvml-mock-config-editor --timeout=60s
  kubectl logs nvml-mock-config-editor
  kubectl delete pod nvml-mock-config-editor --wait=true
}

# Make every mock GPU trip into ECC-uncorrectable mode on its first guarded
# NVML call and queue the given XID on the NVML event set.
_inject_xid() {
  local _xid="$1"
  _run_on_mock_config_dir "
    [ -f /config/config.yaml.bats-orig ] || cp /config/config.yaml /config/config.yaml.bats-orig
    cp /config/config.yaml.bats-orig /config/config.yaml
    sed -i '/^device_defaults:/a\\  failure:\\n    mode: ecc_uncorrectable\\n    xid:\\n      code: ${_xid}' /config/config.yaml
    grep -A4 '^device_defaults:' /config/config.yaml
  "
}

_restore_mock_config() {
  _run_on_mock_config_dir "
    if [ -f /config/config.yaml.bats-orig ]; then
      mv /config/config.yaml.bats-orig /config/config.yaml
    fi
  "
}

# The mock reads its config when the NVML library is loaded, and injected
# failures and taints only live in the plugin process: a restart applies a
# config change and starts over with an untainted device set.
_restart_kubelet_plugin() {
  kubectl delete pod -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=kubelet-plugin --wait=true
  sleep 1
  kubectl wait --for=condition=READY pods -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=kubelet-plugin --timeout=60s
}

_allocated_device_name() {
  kubectl get resourceclaims -o jsonpath='{.items[0].status.allocation.devices.results[0].device}'
}

# Wait for the health the kubelet reports for the pod's (single) allocated
# device. The kubelet updates the pod status on its own sync loop.
_wait_for_allocated_health() {
  local _podname="$1"
  local _health="$2"
  kubectl wait "pod/${_podname}" --timeout=120s \
    --for=jsonpath='{.status.containerStatuses[0].allocatedResourcesStatus[0].resources[0].health}'="${_health}"
}

_device_taint() {
  local _device="$1"
  local _key="$2"
  kubectl get resourceslices -o json | jq -r --arg d "${_device}" --arg k "${_key}" \
    '.items[] | select(.spec.driver == "gpu.nvidia.com") | .spec.devices[]? | select(.name == $d) | .taints[]? | select(.key == $k) | "\(.value):\(.effect)"'
}

_wait_for_device_taint() {
  local _device="$1"
  local _key="$2"
  local _expected="$3"
  local _start=$SECONDS
  while (( SECONDS - _start < 90 )); do
    if [ "$(_device_taint "${_device}" "${_key}")" = "${_expected}" ]; then
      return 0
    fi
    sleep 2
  done
  log "device ${_device} does not carry taint ${_key}=${_expected}; has: '$(_device_taint "${_device}" "${_key}")'"
  return 1
}

_install_with_health_check() {
  local _iargs=("--set" "logVerbosity=6" "--set" "featureGates.NVMLDeviceHealthCheck=true" "$@")
  if [ "${DISABLE_COMPUTE_DOMAINS:-}" = "true" ]; then
    _iargs+=("--set" "resources.computeDomains.enabled=false")
  fi
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
}

setup_file() {
  load 'helpers.sh'
  _common_setup
  if [ "${MOCK_NVML:-}" != "true" ]; then
    return 0
  fi
  _install_with_health_check
}

setup() {
  load 'helpers.sh'
  _common_setup
  if [ "${MOCK_NVML:-}" != "true" ]; then
    skip "XID injection requires mock NVML"
  fi
  log_objects
}

teardown() {
  if [ "${MOCK_NVML:-}" != "true" ]; then
    return 0
  fi
  kubectl delete -f tests/bats/specs/gpu-simple-full.yaml --ignore-not-found --wait=true --timeout=60s
  _restore_mock_config
  _restart_kubelet_plugin
}

bats::on_failure() {
  echo -e "\n\nFAILURE HOOK START"
  log_objects
  kubectl get pod pod-full-gpu -o jsonpath='{.status.containerStatuses[0].allocatedResourcesStatus}' || true
  kubectl get resourceslices -o yaml || true
  show_kubelet_plugin_error_logs
  show_gpu_plugin_log_tails
  echo -e "FAILURE HOOK END\n\n"
}


# bats test_tags=fastfeedback,gpu-health
@test "GPUs: device health: allocated GPU is reported Healthy" {
  kubectl apply -f tests/bats/specs/gpu-simple-full.yaml
  kubectl wait --for=condition=READY pods pod-full-gpu --timeout=60s

  _wait_for_allocated_health pod-full-gpu Healthy
}


# bats test_tags=gpu-health
@test "GPUs: device health: fatal XID marks allocated GPU Unhealthy" {
  kubectl apply -f tests/bats/specs/gpu-simple-full.yaml
  kubectl wait --for=condition=READY pods pod-full-gpu --timeout=60s
  _wait_for_allocated_health pod-full-gpu Healthy
  local _device
  _device="$(_allocated_device_name)"
  [ -n "${_device}" ]

  # The mock does not report a GPU recovery action, so the XID is fatal.
  _inject_xid 79
  _restart_kubelet_plugin

  _wait_for_device_taint "${_device}" gpu.nvidia.com/xid "79:NoSchedule"
  _wait_for_allocated_health pod-full-gpu Unhealthy
  # The workload keeps running: health is reported, not enforced.
  kubectl wait --for=condition=READY pods pod-full-gpu --timeout=10s
}


# bats test_tags=gpu-health
@test "GPUs: device health: ignored XID keeps allocated GPU Healthy" {
  # env[0] is taken by the mock NVML sysfs root, see iupgrade_wait.
  _install_with_health_check \
    "--set" "kubeletPlugin.containers.gpus.env[1].name=ADDITIONAL_XIDS_TO_IGNORE" \
    "--set-string" "kubeletPlugin.containers.gpus.env[1].value=79"

  kubectl apply -f tests/bats/specs/gpu-simple-full.yaml
  kubectl wait --for=condition=READY pods pod-full-gpu --timeout=60s
  local _device
  _device="$(_allocated_device_name)"
  [ -n "${_device}" ]

  _inject_xid 79
  _restart_kubelet_plugin

  # An ignored XID is non-fatal: informational taint, device stays Healthy.
  _wait_for_device_taint "${_device}" gpu.nvidia.com/xid "79:None"
  _wait_for_allocated_health pod-full-gpu Healthy

  _install_with_health_check
}
