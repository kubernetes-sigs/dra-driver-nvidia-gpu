# shellcheck disable=SC2148
# shellcheck disable=SC2329

# Tests for GPU allocation robustness, lifecycle, metrics, and edge cases.
# All tests work on any GPU type (V100, A10, A100, H100, GH200, B200).

setup_file() {
  load 'helpers.sh'
  _common_setup
  local _iargs=("--set" "logVerbosity=6")
  if [ "${DISABLE_COMPUTE_DOMAINS:-}" = "true" ]; then
    _iargs+=("--set" "resources.computeDomains.enabled=false")
  fi
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
}

setup() {
  load 'helpers.sh'
  _common_setup
  MOCK_NVML_CDI_TEST_ACTIVE=false
  MOCK_NVML_UVM_TOOLS_REMOVED=false
  log_objects
}

restore_mock_nvml_uvm_tools_node() {
  local plugin_pod
  plugin_pod=$(get_one_kubelet_plugin_pod_name)
  if [ -z "${plugin_pod}" ]; then
    return 1
  fi

  kubectl exec -n dra-driver-nvidia-gpu "${plugin_pod}" -c gpus -- \
    sh -c 'test -c /driver-root/dev/nvidia-uvm-tools || mknod -m 666 /driver-root/dev/nvidia-uvm-tools c 510 1'
  MOCK_NVML_UVM_TOOLS_REMOVED=false
}

teardown() {
  if [ "${MOCK_NVML_CDI_TEST_ACTIVE:-}" = "true" ]; then
    kubectl delete -f tests/bats/specs/gpu-simple-full.yaml --ignore-not-found --wait=false || true
  fi
  if [ "${MOCK_NVML_UVM_TOOLS_REMOVED:-}" = "true" ]; then
    restore_mock_nvml_uvm_tools_node || true
  fi
}

bats::on_failure() {
  echo -e "\n\nFAILURE HOOK START"
  log_objects
  show_kubelet_plugin_error_logs
  show_gpu_plugin_log_tails
  echo -e "FAILURE HOOK END\n\n"
}


# --- 1. Metrics smoke test ---

# bats test_tags=fastfeedback,gpu-robustness
@test "GPUs: kubelet-plugin exposes Prometheus metrics" {
  # Get one kubelet-plugin pod name
  local plugin_pod
  plugin_pod=$(get_one_kubelet_plugin_pod_name)
  [ -n "${plugin_pod}" ]

  # Curl the metrics endpoint from inside the pod
  run kubectl exec -n dra-driver-nvidia-gpu "${plugin_pod}" -c gpus -- \
    sh -c 'curl -sf http://localhost:8080/metrics 2>/dev/null || wget -qO- http://localhost:8080/metrics'
  assert_output --partial "nvidia_dra_prepared_devices"
  # nvidia_dra_requests_total is a counter that only appears after the first
  # DRA request. At setup_file time no pods have used a GPU yet, so the metric
  # may not be registered. Check for it only if it exists.
  if echo "$output" | grep -q "nvidia_dra_requests_total"; then
    assert_output --partial "nvidia_dra_requests_total"
  fi
}


# --- 2. ResourceClaim lifecycle states ---

# bats test_tags=fastfeedback,gpu-robustness
@test "GPUs: ResourceClaim lifecycle — allocated on pod create, released on pod delete" {
  local _specpath="tests/bats/specs/gpu-simple-full.yaml"
  local _podname="pod-full-gpu"

  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "${_podname}" --timeout=30s

  # Claim should be in allocated state while pod is running
  local claim_name
  claim_name=$(kubectl get resourceclaims -o jsonpath='{.items[0].metadata.name}')
  [ -n "${claim_name}" ]
  run kubectl get resourceclaim "${claim_name}" -o jsonpath='{.status.allocation}'
  assert_output --partial "devices"

  # Delete pod — claim should be released (deleted since it's from a template)
  kubectl delete -f "${_specpath}"
  kubectl wait --for=delete pods "${_podname}" --timeout=30s

  # Verify no claims remain (template-generated claims are deleted with the pod)
  local remaining
  remaining=$(kubectl get resourceclaims --no-headers 2>/dev/null | wc -l)
  [ "${remaining}" -eq 0 ]
}


# --- 3. GPU re-acquisition after pod delete and re-create ---

# bats test_tags=fastfeedback,gpu-robustness
@test "GPUs: GPU re-acquired after pod delete and re-create" {
  local _specpath="tests/bats/specs/gpu-simple-full.yaml"
  local _podname="pod-full-gpu"

  # First allocation
  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "${_podname}" --timeout=30s

  run kubectl logs "${_podname}"
  assert_output --partial "UUID: GPU-"
  local uid1="${output}"

  # Delete
  kubectl delete -f "${_specpath}"
  kubectl wait --for=delete pods "${_podname}" --timeout=30s

  # Re-create — GPU should be re-acquired
  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "${_podname}" --timeout=30s

  run kubectl logs "${_podname}"
  assert_output --partial "UUID: GPU-"

  # Cleanup
  kubectl delete -f "${_specpath}"
  kubectl wait --for=delete pods "${_podname}" --timeout=30s
}


# --- 4. Multiple claims in one pod ---

# bats test_tags=fastfeedback,gpu-robustness,multi-gpu
@test "GPUs: pod with two ResourceClaimTemplates gets two distinct GPUs" {
  local _specpath="tests/bats/specs/gpu-two-claims-one-pod.yaml"
  local _podname="pod-two-claims"

  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "${_podname}" --timeout=30s

  # Pod should see multiple GPUs via nvidia-smi
  run kubectl logs "${_podname}"
  assert_output --partial "UUID: GPU-"

  # Count GPUs visible — should be 2 (one per claim)
  local gpu_count
  gpu_count=$(kubectl logs "${_podname}" | grep -c "UUID: GPU-")
  [ "${gpu_count}" -ge 2 ]

  kubectl delete -f "${_specpath}"
  kubectl wait --for=delete pods "${_podname}" --timeout=30s
}


# --- 5. CEL selector tests ---

# bats test_tags=fastfeedback,gpu-robustness
@test "GPUs: CEL selector matches GPU by architecture attribute" {
  # Get the actual architecture from the ResourceSlice (e.g., "Hopper", "Ampere")
  local arch
  arch=$(kubectl get resourceslices -A -o json | \
    jq -r '[.items[] | select(.spec.driver=="gpu.nvidia.com") | .spec.devices[]? | select(.attributes.type.string=="gpu")] | .[0].attributes.architecture.string')
  [ -n "${arch}" ] && [ "${arch}" != "null" ]
  echo "Detected GPU architecture: ${arch}"

  # Create a RCT with CEL selector matching the actual architecture
  # (architecture is a single word without spaces, safe for YAML embedding)
  cat <<SPEC | kubectl apply -f -
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: rct-cel-match
  labels:
    env: batssuite
spec:
  spec:
    devices:
      requests:
      - name: gpu
        exactly:
          deviceClassName: gpu.nvidia.com
          selectors:
          - cel:
              expression: 'device.attributes["gpu.nvidia.com"].architecture == "${arch}"'
---
apiVersion: v1
kind: Pod
metadata:
  name: pod-cel-match
  labels:
    env: batssuite
spec:
  restartPolicy: Never
  containers:
  - name: ctr
    image: ubuntu:24.04
    command: ["nvidia-smi", "-L"]
    resources:
      claims:
      - name: gpu
  resourceClaims:
  - name: gpu
    resourceClaimTemplateName: rct-cel-match
SPEC

  kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pods pod-cel-match --timeout=30s

  run kubectl logs pod-cel-match
  assert_output --partial "UUID: GPU-"

  kubectl delete pod pod-cel-match --force 2>/dev/null
  kubectl delete resourceclaimtemplate rct-cel-match 2>/dev/null
}


# bats test_tags=fastfeedback,gpu-robustness
@test "GPUs: CEL selector rejects non-matching productName" {
  local _specpath="tests/bats/specs/gpu-cel-nomatch.yaml"
  local _podname="pod-cel-nomatch"

  kubectl apply -f "${_specpath}"

  # Pod should stay Pending — no GPU matches the selector
  sleep 5
  local phase
  phase=$(kubectl get pod "${_podname}" -o jsonpath='{.status.phase}')
  [ "${phase}" = "Pending" ]

  kubectl delete -f "${_specpath}" --force 2>/dev/null
}


# --- 8. GPU count vs ResourceSlice consistency ---

# bats test_tags=fastfeedback,gpu-robustness
@test "GPUs: ResourceSlice device count matches host GPU count" {
  # Get GPU count from ResourceSlice
  local slice_count
  slice_count=$(kubectl get resourceslices -A -o json | \
    jq '[.items[] | select(.spec.driver=="gpu.nvidia.com") | .spec.devices[]? | select(.attributes.type.string=="gpu")] | length')

  [ "${slice_count}" -gt 0 ]
  echo "ResourceSlice reports ${slice_count} GPU(s)"

  # Get GPU count by counting /dev/nvidia[0-9]* device nodes on the host
  # via a privileged pod with the host /dev mounted.
  cat <<SPEC | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: pod-gpu-count
  labels:
    env: batssuite
spec:
  restartPolicy: Never
  containers:
  - name: ctr
    image: ubuntu:24.04
    command: ["bash", "-c"]
    args: ["ls /host-dev/nvidia[0-9]* 2>/dev/null | wc -l"]
    securityContext:
      privileged: true
    volumeMounts:
    - name: dev
      mountPath: /host-dev
      readOnly: true
  volumes:
  - name: dev
    hostPath:
      path: /dev
SPEC

  kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pods pod-gpu-count --timeout=30s
  local host_count
  host_count=$(kubectl logs pod-gpu-count | tr -d '[:space:]')

  echo "Host /dev/nvidia* device count: ${host_count}"
  [ "${slice_count}" -eq "${host_count}" ]

  kubectl delete pod pod-gpu-count --force 2>/dev/null
}


# --- 9. Concurrent claim create/delete churn ---

# bats test_tags=fastfeedback,gpu-robustness
@test "GPUs: rapid claim create/delete does not leak resources" {
  local _specpath="tests/bats/specs/gpu-simple-full.yaml"
  local _podname="pod-full-gpu"

  # Record initial GPU count in ResourceSlice
  local initial_count
  initial_count=$(kubectl get resourceslices -A -o json | \
    jq '[.items[] | select(.spec.driver=="gpu.nvidia.com") | .spec.devices[]? | select(.attributes.type.string=="gpu")] | length')

  # Churn: 10 rapid create/delete cycles
  for i in $(seq 1 10); do
    kubectl apply -f "${_specpath}"
    kubectl wait --for=condition=READY pods "${_podname}" --timeout=30s
    kubectl delete -f "${_specpath}"
    kubectl wait --for=delete pods "${_podname}" --timeout=30s
  done

  # Verify no orphaned claims remain
  local remaining_claims
  remaining_claims=$(kubectl get resourceclaims --no-headers 2>/dev/null | wc -l)
  [ "${remaining_claims}" -eq 0 ]

  # Verify GPU count unchanged in ResourceSlice
  local final_count
  final_count=$(kubectl get resourceslices -A -o json | \
    jq '[.items[] | select(.spec.driver=="gpu.nvidia.com") | .spec.devices[]? | select(.attributes.type.string=="gpu")] | length')
  [ "${initial_count}" -eq "${final_count}" ]
}


# --- 10. Incomplete CDI device nodes ---

# bats test_tags=fastfeedback,gpu-robustness,mock-nvml
@test "MockNVML: incomplete CDI edits fail preparation and are not cached" {
  if [ "${MOCK_NVML:-}" != "true" ]; then
    skip "mock NVML only"
  fi

  local _specpath="tests/bats/specs/gpu-simple-full.yaml"
  local _podname="pod-full-gpu"
  local plugin_pod
  plugin_pod=$(get_one_kubelet_plugin_pod_name)
  [ -n "${plugin_pod}" ]

  # Remove one required common node from the mock driver root, then restart the
  # plugin once so neither the common nor per-device CDI cache masks the test.
  kubectl exec -n dra-driver-nvidia-gpu "${plugin_pod}" -c gpus -- \
    rm -f /driver-root/dev/nvidia-uvm-tools
  MOCK_NVML_UVM_TOOLS_REMOVED=true

  kubectl delete pod -n dra-driver-nvidia-gpu "${plugin_pod}" --wait=true --timeout=60s
  kubectl wait --for=condition=READY pods \
    -n dra-driver-nvidia-gpu \
    -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    --timeout=60s

  plugin_pod=$(get_one_kubelet_plugin_pod_name)
  local plugin_uid
  plugin_uid=$(kubectl get pod -n dra-driver-nvidia-gpu "${plugin_pod}" -o jsonpath='{.metadata.uid}')
  local plugin_restart_count
  plugin_restart_count=$(kubectl get pod -n dra-driver-nvidia-gpu "${plugin_pod}" \
    -o jsonpath='{.status.containerStatuses[?(@.name=="gpus")].restartCount}')

  MOCK_NVML_CDI_TEST_ACTIVE=true
  kubectl apply -f "${_specpath}"
  kubectl wait \
    --for=jsonpath='{.status.containerStatuses[0].state.waiting.reason}'=ContainerCreating \
    pod/"${_podname}" \
    --timeout=30s
  wait_for_pod_event pod/"${_podname}" FailedPrepareDynamicResources 30

  run kubectl events --for pod/"${_podname}"
  assert_output --partial "failed to validate NVIDIA CDI common edits"
  assert_output --partial "/dev/nvidia-uvm-tools"

  # Restoring the mock driver installation must let kubelet's retry prepare the
  # same pod without another plugin restart. This proves the incomplete edits
  # were not cached.
  restore_mock_nvml_uvm_tools_node
  kubectl wait --for=condition=READY pod/"${_podname}" --timeout=60s

  local current_plugin_pod
  current_plugin_pod=$(get_one_kubelet_plugin_pod_name)
  run kubectl get pod -n dra-driver-nvidia-gpu "${current_plugin_pod}" \
    -o jsonpath='{.metadata.uid}:{.status.containerStatuses[?(@.name=="gpus")].restartCount}'
  assert_output "${plugin_uid}:${plugin_restart_count}"

  run kubectl logs "${_podname}"
  assert_output --partial "UUID: GPU-"

  kubectl delete -f "${_specpath}"
  kubectl wait --for=delete pod/"${_podname}" --timeout=30s
  MOCK_NVML_CDI_TEST_ACTIVE=false
}
