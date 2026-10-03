# shellcheck disable=SC2148
# shellcheck disable=SC2329

# GPU health-checking coverage for the mock NVML environment.
# Requires NVMLDeviceHealthCheck and a health-control file the mock library
# re-reads while the plugin is running.

# Full-GPU events use NVML's 0xFFFFFFFF GI/CI sentinel.
export FULL_GPU_INSTANCE_ID=4294967295

setup_file() {
  load 'helpers.sh'
  _common_setup
  if [ "${MOCK_NVML:-}" != "true" ]; then
    return 0
  fi

  local _iargs=(
    "--set" "logVerbosity=6"
    "--set" "featureGates.NVMLDeviceHealthCheck=true"
    "--set" "kubeletPlugin.containers.gpus.env[1].name=MOCK_NVML_HEALTH_CONTROL"
    "--set" "kubeletPlugin.containers.gpus.env[1].value=/driver-root/health-control.json"
  )
  if [ "${DISABLE_COMPUTE_DOMAINS:-}" = "true" ]; then
    _iargs+=("--set" "resources.computeDomains.enabled=false")
  fi
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
}

setup() {
  load 'helpers.sh'
  _common_setup
  if [ "${MOCK_NVML:-}" != "true" ]; then
    skip "requires mock NVML health-control injection"
  fi
  clear_health_control
  log_objects
}

bats::on_failure() {
  echo -e "\n\nFAILURE HOOK START"
  log_objects
  show_kubelet_plugin_error_logs
  show_gpu_plugin_log_tails
  echo -e "FAILURE HOOK END\n\n"
}

gpu_plugin_pod() {
  kubectl get pod -n dra-driver-nvidia-gpu \
    -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    -o jsonpath='{.items[0].metadata.name}'
}

write_health_control() {
  local json="$1"
  local pod
  pod="$(gpu_plugin_pod)"
  printf '%s\n' "${json}" | kubectl exec -i -n dra-driver-nvidia-gpu "${pod}" -c gpus -- \
    sh -c 'cat > /driver-root/health-control.json.tmp && mv /driver-root/health-control.json.tmp /driver-root/health-control.json'
}

clear_health_control() {
  write_health_control '{"events":[],"recoveryActions":[]}'
}

inject_xid() {
  local id="$1"
  local gpu="$2"
  local xid="$3"
  local action="$4"
  local gi="$5"
  local ci="$6"
  write_health_control "$(printf '{"events":[{"id":"%s","gpu":%s,"xid":%s,"gpuInstanceId":%s,"computeInstanceId":%s}],"recoveryActions":[{"gpu":%s,"action":"%s"}]}' \
    "${id}" "${gpu}" "${xid}" "${gi}" "${ci}" "${gpu}" "${action}")"
}

xid_taint_effect() {
  local device="$1"
  local xid="$2"
  kubectl get resourceslices -o json | jq -r --arg device "${device}" --arg xid "${xid}" '
    [ .items[].spec.devices[]?
      | select(.name == $device)
      | .taints[]?
      | select(.key == "gpu.nvidia.com/xid" and .value == $xid)
      | .effect
    ] | first // ""
  '
}

wait_for_xid_taint() {
  local device="$1"
  local xid="$2"
  local effect="$3"
  local deadline=$((SECONDS + 30))
  local got=""
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    got="$(xid_taint_effect "${device}" "${xid}")"
    if [ "${got}" = "${effect}" ]; then
      return 0
    fi
    sleep 1
  done
  echo "timed out waiting for ${device} XID ${xid} effect ${effect}; last effect=${got:-<none>}"
  kubectl get resourceslices -o json | jq '.items[].spec.devices[]? | {name, taints}'
  return 1
}

wait_for_gpu_log() {
  local pattern="$1"
  local deadline=$((SECONDS + 30))
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    if kubectl logs -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=kubelet-plugin -c gpus --since=60s | grep -F -- "${pattern}" > /dev/null; then
      return 0
    fi
    sleep 1
  done
  echo "timed out waiting for gpu plugin log: ${pattern}"
  kubectl logs -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=kubelet-plugin -c gpus --tail=200 || true
  return 1
}

# bats test_tags=gpu-health
@test "GPUs: mock NVML MIG-scoped XID is not applied to the full GPU" {
  inject_xid "mig-scoped" 0 45 "GPU_RECOVERY_ACTION_NONE" 3 1

  wait_for_gpu_log "GI:3, CI:1"
  local effect
  effect="$(xid_taint_effect gpu-0 45)"
  [ -z "${effect}" ]
}

# bats test_tags=gpu-health
@test "GPUs: mock NVML non-fatal XID publishes an informational taint" {
  inject_xid "nonfatal-43" 0 43 "GPU_RECOVERY_ACTION_NONE" "${FULL_GPU_INSTANCE_ID}" "${FULL_GPU_INSTANCE_ID}"

  wait_for_xid_taint gpu-0 43 None
}

# bats test_tags=gpu-health
@test "GPUs: mock NVML recovery-required XID publishes a sticky NoSchedule taint" {
  inject_xid "fatal-79" 1 79 "GPU_RESET" "${FULL_GPU_INSTANCE_ID}" "${FULL_GPU_INSTANCE_ID}"
  wait_for_xid_taint gpu-1 79 NoSchedule

  # The mock can move the GPU back to NONE while the plugin is running. The
  # driver keeps the NoSchedule taint until a future recovery path removes it.
  write_health_control '{"events":[],"recoveryActions":[{"gpu":1,"action":"NONE"}]}'
  sleep 3
  local effect
  effect="$(xid_taint_effect gpu-1 79)"
  [ "${effect}" = "NoSchedule" ]
}

# bats test_tags=gpu-health
@test "GPUs: additional-xids-to-ignore overrides a recovery-required XID" {
  local _iargs=(
    "--set" "logVerbosity=6"
    "--set" "featureGates.NVMLDeviceHealthCheck=true"
    "--set" "kubeletPlugin.containers.gpus.env[1].name=MOCK_NVML_HEALTH_CONTROL"
    "--set" "kubeletPlugin.containers.gpus.env[1].value=/driver-root/health-control.json"
    "--set" "kubeletPlugin.containers.gpus.env[2].name=ADDITIONAL_XIDS_TO_IGNORE"
    "--set" "kubeletPlugin.containers.gpus.env[2].value=79"
  )
  if [ "${DISABLE_COMPUTE_DOMAINS:-}" = "true" ]; then
    _iargs+=("--set" "resources.computeDomains.enabled=false")
  fi
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
  clear_health_control

  inject_xid "ignored-79" 0 79 "GPU_RECOVERY_ACTION_GPU_RESET" "${FULL_GPU_INSTANCE_ID}" "${FULL_GPU_INSTANCE_ID}"
  wait_for_xid_taint gpu-0 79 None
}
