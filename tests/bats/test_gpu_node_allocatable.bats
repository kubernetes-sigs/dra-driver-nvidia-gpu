# shellcheck disable=SC2148
# shellcheck disable=SC2329

# Tests for KEP-5517 node-allocatable overhead publishing.
# Requires driver feature gate: NodeAllocatableResources=true.
# Requires cluster feature gate: DRANodeAllocatableResources=true (alpha,
# Kubernetes 1.37+); tests are skipped when the API server drops the field.

setup_file() {
  load 'helpers.sh'
  _common_setup
  local _iargs=("--set" "logVerbosity=6"
    "--set" "featureGates.NodeAllocatableResources=true"
    "--set" "nodeAllocatableOverhead.gpu.memory.perPod=256Mi"
    "--set" "nodeAllocatableOverhead.gpu.memory.perContainer=64Mi"
    "--set" "nodeAllocatableOverhead.gpu.cpu.perPod=500m")
  if [ "${DISABLE_COMPUTE_DOMAINS:-}" = "true" ]; then
    _iargs+=("--set" "resources.computeDomains.enabled=false")
  fi
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
}

setup() {
  load 'helpers.sh'
  _common_setup
  log_objects
}

bats::on_failure() {
  echo -e "\n\nFAILURE HOOK START"
  log_objects
  show_kubelet_plugin_error_logs
  show_gpu_plugin_log_tails
  echo -e "FAILURE HOOK END\n\n"
}

# Skip when the cluster's API server does not retain the (alpha, 1.37+)
# nodeAllocatableResources field: a server-side dry-run create of a minimal
# ResourceSlice reveals whether the field survives or is pruned. The slice must
# reference an existing node, otherwise admission rejects it outright.
skip_without_cluster_gate() {
  local _node _out _rc
  _node=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
  _out=$(kubectl create --dry-run=server -o json -f - 2>&1 <<EOF
apiVersion: resource.k8s.io/v1
kind: ResourceSlice
metadata:
  name: bats-nar-gate-probe
spec:
  driver: probe.batssuite.example.com
  nodeName: ${_node}
  pool:
    name: bats-nar-gate-probe
    generation: 1
    resourceSliceCount: 1
  devices:
  - name: probe
    nodeAllocatableResources:
      memory:
        overhead:
          perPod: 1Mi
EOF
  ) && _rc=0 || _rc=$?
  if [ "${_rc}" -ne 0 ]; then
    echo "gate probe dry-run failed: ${_out}"
    return 1
  fi
  if ! grep -q nodeAllocatableResources <<< "${_out}"; then
    skip "cluster does not enable DRANodeAllocatableResources (field dropped by API server)"
  fi
}

# bats test_tags=fastfeedback,node-allocatable
@test "GPUs: NodeAllocatableResources — ResourceSlice publishes gpu overhead" {
  skip_without_cluster_gate

  local _overheads=""
  for _ in $(seq 1 12); do
    _overheads=$(kubectl get resourceslices.resource.k8s.io -o json \
      | jq -c '[.items[].spec.devices[]
                | select(.nodeAllocatableResources != null)
                | {name, nar: .nodeAllocatableResources}]')
    [ "${_overheads}" != "[]" ] && break
    sleep 5
  done
  echo "devices with overhead: ${_overheads}"

  run jq -r '.[0].nar.memory.overhead.perPod' <<< "${_overheads}"
  assert_output "256Mi"
  run jq -r '.[0].nar.memory.overhead.perContainer' <<< "${_overheads}"
  assert_output "64Mi"
  run jq -r '.[0].nar.cpu.overhead.perPod' <<< "${_overheads}"
  assert_output "500m"
  # Overhead is the only branch this driver publishes; Mapping must be unset.
  run jq -r '[.[] | .nar[] | has("mapping")] | any' <<< "${_overheads}"
  assert_output "false"
}

# bats test_tags=fastfeedback,node-allocatable
@test "GPUs: NodeAllocatableResources — scheduler records overhead in pod status" {
  skip_without_cluster_gate

  local _specpath="tests/bats/specs/gpu-node-allocatable.yaml"

  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "pod-node-allocatable-0" --timeout=60s

  run kubectl logs "pod-node-allocatable-0" -c ctr
  assert_output --partial "UUID: GPU-"

  local _status
  _status=$(kubectl get pod "pod-node-allocatable-0" -o json \
    | jq -c '.status.nodeAllocatableResourceClaimStatuses')
  echo "nodeAllocatableResourceClaimStatuses: ${_status}"

  run jq -r '.[0].containers[0]' <<< "${_status}"
  assert_output "ctr"
  run jq -r '.[0].overhead[] | select(.name == "memory") | .perPod' <<< "${_status}"
  assert_output "256Mi"
  run jq -r '.[0].overhead[] | select(.name == "memory") | .perContainer' <<< "${_status}"
  assert_output "64Mi"
  run jq -r '.[0].overhead[] | select(.name == "cpu") | .perPod' <<< "${_status}"
  assert_output "500m"

  kubectl delete -f "${_specpath}"
  kubectl wait --for=delete pods "pod-node-allocatable-0" --timeout=30s
}

# The two negative-path tests below reinstall the driver with the feature gate
# disabled and are ordered last: bats runs the tests of a file in order, and the
# next test file's setup_file() reinstalls its own configuration.

# bats test_tags=fastfeedback,node-allocatable
@test "GPUs: NodeAllocatableResources — values set with gate disabled fail startup" {
  skip_without_cluster_gate

  local _iargs=("--set" "logVerbosity=6"
    "--set" "featureGates.NodeAllocatableResources=false"
    "--set" "nodeAllocatableOverhead.gpu.memory.perPod=256Mi")
  if [ "${DISABLE_COMPUTE_DOMAINS:-}" = "true" ]; then
    _iargs+=("--set" "resources.computeDomains.enabled=false")
  fi
  # The gpus container is expected to fail to start, so iupgrade_wait (which
  # waits for READY plugin pods) cannot be used; run the same helm command
  # without --wait instead.
  local _mock_args=()
  if [ -n "${TEST_ALT_PROC_DEVICES:-}" ]; then
    _mock_args+=("--set" "altProcDevices=${TEST_ALT_PROC_DEVICES}")
  fi
  if [ "${MOCK_NVML:-}" = "true" ]; then
    _mock_args+=(
      "--set" "kubeletPlugin.containers.gpus.env[0].name=NVIDIA_DRA_SYSFS_ROOT"
      "--set" "kubeletPlugin.containers.gpus.env[0].value=/driver-root/sys")
  fi
  timeout -v 120 helm upgrade --install "${TEST_HELM_RELEASE_NAME}" \
    "${TEST_CHART_REPO}" \
    --version="${TEST_CHART_VERSION}" \
    --create-namespace \
    --namespace dra-driver-nvidia-gpu \
    --set gpuResourcesEnabledOverride=true \
    --set nvidiaDriverRoot="${TEST_NVIDIA_DRIVER_ROOT}" "${_mock_args[@]}" "${_iargs[@]}"

  local _pod=""
  local _logs=""
  for _ in $(seq 1 24); do
    _pod=$(kubectl get pod -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=kubelet-plugin \
      -o name 2>/dev/null | head -1)
    if [ -n "${_pod}" ]; then
      # Either fetch may fail transiently (no previous container yet, or the
      # container restarting), so both are tolerated inside the poll loop.
      _logs=$( { kubectl logs -n dra-driver-nvidia-gpu "${_pod}" -c gpus --tail=20 2>/dev/null || true; \
                 kubectl logs -n dra-driver-nvidia-gpu "${_pod}" -c gpus --previous --tail=20 2>/dev/null || true; } )
      grep -q "node-allocatable overhead flags require feature gate NodeAllocatableResources" <<< "${_logs}" && break
    fi
    sleep 5
  done
  echo "gpus container logs: ${_logs}"
  run grep -c "node-allocatable overhead flags require feature gate NodeAllocatableResources" <<< "${_logs}"
  refute_output "0"
}

# bats test_tags=fastfeedback,node-allocatable
@test "GPUs: NodeAllocatableResources — gate disabled and no values publishes no field" {
  skip_without_cluster_gate

  local _iargs=("--set" "logVerbosity=6"
    "--set" "featureGates.NodeAllocatableResources=false")
  if [ "${DISABLE_COMPUTE_DOMAINS:-}" = "true" ]; then
    _iargs+=("--set" "resources.computeDomains.enabled=false")
  fi
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs

  # Give the plugin time to republish its ResourceSlices after the rollout.
  local _count="-1"
  for _ in $(seq 1 12); do
    _count=$(kubectl get resourceslices.resource.k8s.io -o json \
      | jq '[.items[].spec.devices[] | select(.nodeAllocatableResources != null)] | length')
    [ "${_count}" = "0" ] && break
    sleep 5
  done
  assert_equal "${_count}" "0"
}
