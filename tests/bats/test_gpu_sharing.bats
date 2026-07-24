# shellcheck disable=SC2148
# shellcheck disable=SC2329

# Tests for GPU sharing strategies: TimeSlicing and MPS.
# Requires feature gates: TimeSlicingSettings=true, MPSSupport=true.
# Works on any GPU type.

setup_file() {
  load 'helpers.sh'
  _common_setup
  local _iargs=("--set" "logVerbosity=6"
    "--set" "featureGates.TimeSlicingSettings=true"
    "--set" "featureGates.MPSSupport=true"
    "--set" "featureGates.ConsumableShares=true"
    "--set" "consumableShares=4")
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


# bats test_tags=fastfeedback,gpu-sharing
@test "GPUs: TimeSlicing — 2 containers share GPU with Short interval" {
  local _specpath="tests/bats/specs/gpu-timeslicing.yaml"
  local _podname="pod-timeslicing"

  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "${_podname}" --timeout=30s

  # Both containers should see the same GPU
  run kubectl logs "${_podname}" -c ctr0
  assert_output --partial "UUID: GPU-"
  local uid0="${output}"

  run kubectl logs "${_podname}" -c ctr1
  assert_output --partial "UUID: GPU-"
  local uid1="${output}"

  # Same GPU shared via TimeSlicing
  assert_equal "$uid0" "$uid1"

  kubectl delete -f "${_specpath}"
  kubectl wait --for=delete pods "${_podname}" --timeout=30s
}


# bats test_tags=fastfeedback,gpu-sharing
@test "GPUs: MPS — 2 containers share GPU with MPS config" {
  if [ "${MOCK_NVML:-}" = "true" ]; then skip "requires real MPS daemon"; fi
  local _specpath="tests/bats/specs/gpu-mps.yaml"
  local _podname="pod-mps"

  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "${_podname}" --timeout=60s

  # Both containers should see the same GPU
  run kubectl logs "${_podname}" -c ctr0
  assert_output --partial "UUID: GPU-"
  local uid0="${output}"

  run kubectl logs "${_podname}" -c ctr1
  assert_output --partial "UUID: GPU-"
  local uid1="${output}"

  # Same GPU shared via MPS
  assert_equal "$uid0" "$uid1"

  # Verify MPS control daemon deployment was created by the driver
  local mps_count
  mps_count=$(kubectl get deployments -A --no-headers 2>/dev/null | grep "mps-control-daemon" | wc -l)
  [ "${mps_count}" -ge 1 ]

  kubectl delete -f "${_specpath}"
  kubectl wait --for=delete pods "${_podname}" --timeout=30s
}

# bats test_tags=fastfeedback,gpu-sharing,consumable-shares
@test "GPUs: ConsumableShares — memory mode with 2 independent pods" {
  local _specpath="tests/bats/specs/gpu-consumable-shares-memory.yaml"

  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "pod-share-memory-0" --timeout=45s
  kubectl wait --for=condition=READY pods "pod-share-memory-1" --timeout=45s

  run kubectl logs "pod-share-memory-0" -c ctr
  assert_output --partial "UUID: GPU-"

  run kubectl logs "pod-share-memory-1" -c ctr
  assert_output --partial "UUID: GPU-"

  kubectl delete -f "${_specpath}"
  kubectl wait --for=delete pods "pod-share-memory-0" --timeout=30s
  kubectl wait --for=delete pods "pod-share-memory-1" --timeout=30s
}

# bats test_tags=fastfeedback,gpu-sharing,consumable-shares
@test "GPUs: ConsumableShares — integer shares mode with 2 independent pods" {
  local _specpath="tests/bats/specs/gpu-consumable-shares-integer.yaml"

  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "pod-share-integer-0" --timeout=45s
  kubectl wait --for=condition=READY pods "pod-share-integer-1" --timeout=45s

  run kubectl logs "pod-share-integer-0" -c ctr
  assert_output --partial "UUID: GPU-"

  run kubectl logs "pod-share-integer-1" -c ctr
  assert_output --partial "UUID: GPU-"

  kubectl delete -f "${_specpath}"
  kubectl wait --for=delete pods "pod-share-integer-0" --timeout=30s
  kubectl wait --for=delete pods "pod-share-integer-1" --timeout=30s
}

# bats test_tags=fastfeedback,gpu-sharing,mps-decoupled
@test "GPUs: MPS Decoupled — 2 independent claims share node MPS daemon and cleanup" {
  if [ "${MOCK_NVML:-}" = "true" ]; then skip "requires real MPS daemon"; fi
  local _specpath="tests/bats/specs/gpu-mps-decoupled.yaml"

  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "pod-mps-decoupled-0" --timeout=60s
  kubectl wait --for=condition=READY pods "pod-mps-decoupled-1" --timeout=60s

  # Exactly 1 node-level MPS control daemon deployment should exist
  local mps_count
  mps_count=$(kubectl get deployments -A --no-headers 2>/dev/null | grep "mps-control-daemon" | wc -l)
  [ "${mps_count}" -eq 1 ]

  # Delete first pod — daemon must still remain running for the second pod
  kubectl delete pod "pod-mps-decoupled-0"
  kubectl wait --for=delete pod "pod-mps-decoupled-0" --timeout=30s

  mps_count=$(kubectl get deployments -A --no-headers 2>/dev/null | grep "mps-control-daemon" | wc -l)
  [ "${mps_count}" -eq 1 ]

  # Delete second pod — daemon should now be deleted cleanly
  kubectl delete --ignore-not-found -f "${_specpath}"
  kubectl wait --for=delete pod "pod-mps-decoupled-1" --timeout=30s

  # Wait for MPS daemon to be cleaned up
  sleep 3
  mps_count=$(kubectl get deployments -A --no-headers 2>/dev/null | grep "mps-control-daemon" | wc -l)
  [ "${mps_count}" -eq 0 ]
}
