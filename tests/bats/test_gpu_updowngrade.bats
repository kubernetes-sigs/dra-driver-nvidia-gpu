# shellcheck disable=SC2148
# shellcheck disable=SC2329

setup_file() {
  load 'helpers.sh'
  local _iargs=("--set" "logVerbosity=6")
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
}


# Executed before entering each test in this file.
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


# bats test_tags=fastfeedback
@test "GPUs: upgrade: wipe-state, install-last-stable, upgrade-to-current-dev (simple GPU)" {
  if [ "${MOCK_NVML:-}" = "true" ]; then skip "MOCK_NVML is set"; fi
  # Stage 1: clean slate
  helm uninstall "${TEST_HELM_RELEASE_NAME}" -n dra-driver-nvidia-gpu --wait --timeout=30s
  kubectl wait --for=delete pods -A -l app.kubernetes.io/name=dra-driver-nvidia-gpu --timeout=10s
  bash tests/bats/clean-state-dirs-all-nodes.sh
  kubectl get crd computedomains.resource.nvidia.com
  timeout -v 10 kubectl delete crd computedomains.resource.nvidia.com

  # Stage 2: install last-stable (this guarantees to install the "old" CRD)
  iupgrade_wait "${TEST_CHART_LASTSTABLE_REPO}" "${TEST_CHART_LASTSTABLE_VERSION}" NOARGS

  # Stage 3: apply workload, do not delete (users care about old workloads to
  # ideally still run, but at the very least be deletable after upgrade).
  local _specpath="tests/bats/specs/gpu-simple-full.yaml"
  local _podname="pod-full-gpu"
  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "${_podname}" --timeout=8s
  run kubectl logs "${_podname}"
  assert_output --partial "UUID: GPU-"
  echo "${output}" | wc -l | grep 1

  # Capture the checkpoint content written by last-stable (valuable debug input
  # if the checkpoint upgrade breaks).
  local _node _kpod
  _node=$(kubectl get pod "${_podname}" -o jsonpath='{.spec.nodeName}')
  log "workload runs on node: ${_node}"
  _kpod=$(kubectl get pods -n dra-driver-nvidia-gpu \
    -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    --field-selector spec.nodeName="${_node}" \
    -o jsonpath='{.items[0].metadata.name}')
  log "kubelet-plugin pod on that node: ${_kpod}"
  log "checkpoint.json written by last-stable:"
  kubectl exec -n dra-driver-nvidia-gpu "${_kpod}" -c gpus -- \
    cat /var/lib/kubelet/plugins/gpu.nvidia.com/checkpoint.json
  echo

  # Stage 4: update CRD, as a user would do.
  kubectl apply -f "${CRD_UPGRADE_URL}"

  # Stage 5: install target version (as users would do).
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" NOARGS

  # Stage 6: confirm deleting old workload works (critical, see above).
  kubectl delete -f  "${_specpath}"
  kubectl wait --for=delete pods "${_podname}" --timeout=15s

  # Stage 7: fresh create-confirm-delete workload cycle.
  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "${_podname}" --timeout=8s
  run kubectl logs "${_podname}"
  assert_output --partial "UUID: GPU-"
  kubectl delete -f  "${_specpath}"
  kubectl wait --for=delete pods "${_podname}" --timeout=15s
}


# bats test_tags=fastfeedback
@test "GPUs: upgrade: wipe-state, install-last-stable, upgrade-to-current-dev (DynMIG)" {
  if [ "${MOCK_NVML:-}" = "true" ]; then skip "MOCK_NVML is set"; fi

  # DynamicMIG must be enabled in both last-stable and current dev installs.
  local _iargs=("--set" "featureGates.DynamicMIG=true")

  mig_confirm_disabled_on_all_nodes

  # Stage 1: clean slate
  helm uninstall "${TEST_HELM_RELEASE_NAME}" -n dra-driver-nvidia-gpu --wait --timeout=30s
  kubectl wait --for=delete pods -A -l app.kubernetes.io/name=dra-driver-nvidia-gpu --timeout=10s
  bash tests/bats/clean-state-dirs-all-nodes.sh
  kubectl get crd computedomains.resource.nvidia.com
  timeout -v 10 kubectl delete crd computedomains.resource.nvidia.com

  # Stage 2: install last-stable (this guarantees to install the "old" CRD)
  iupgrade_wait "${TEST_CHART_LASTSTABLE_REPO}" "${TEST_CHART_LASTSTABLE_VERSION}" _iargs

  # Stage 3: apply workload, do not delete (users care about old workloads to
  # ideally still run, but at the very least be deletable after upgrade).
  local _specpath="tests/bats/specs/gpu-simple-mig.yaml"
  local _podname="pod-mig1g"
  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "${_podname}" --timeout=15s
  run kubectl logs "${_podname}"
  assert_output --partial "UUID: MIG-"
  assert_output --partial "UUID: GPU-"
  echo "${output}" | wc -l | grep 2

  # Capture the checkpoint content written by last-stable (valuable debug input
  # if the checkpoint upgrade breaks).
  local _node _kpod
  _node=$(kubectl get pod "${_podname}" -o jsonpath='{.spec.nodeName}')
  log "workload runs on node: ${_node}"
  _kpod=$(kubectl get pods -n dra-driver-nvidia-gpu \
    -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    --field-selector spec.nodeName="${_node}" \
    -o jsonpath='{.items[0].metadata.name}')
  log "kubelet-plugin pod on that node: ${_kpod}"
  log "checkpoint.json written by last-stable:"
  kubectl exec -n dra-driver-nvidia-gpu "${_kpod}" -c gpus -- \
    cat /var/lib/kubelet/plugins/gpu.nvidia.com/checkpoint.json
  echo

  # Stage 4: update CRD, as a user would do.
  kubectl apply -f "${CRD_UPGRADE_URL}"

  # Stage 5: install target version (as users would do).
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs

  # Stage 6: confirm deleting old workload works (critical, see above).
  kubectl delete -f  "${_specpath}"
  kubectl wait --for=delete pods "${_podname}" --timeout=15s

  # After unprepare by the new binary: the GI/CI created by last-stable must be
  # destroyed and MIG mode disabled.
  mig_confirm_disabled_on_all_nodes

  # Stage 7: fresh create-confirm-delete workload cycle.
  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "${_podname}" --timeout=15s
  run kubectl logs "${_podname}"
  assert_output --partial "UUID: MIG-"
  assert_output --partial "UUID: GPU-"
  kubectl delete -f  "${_specpath}"
  kubectl wait --for=delete pods "${_podname}" --timeout=15s

  # Confirm cleanup.
  mig_confirm_disabled_on_all_nodes
}


# bats test_tags=fastfeedback
@test "GPUs: corrupted checkpoint leads to diff being logged" {
  if [ "${MOCK_NVML:-}" = "true" ]; then skip "MOCK_NVML is set"; fi

  # Stage 1: clean slate, install current dev. The plugin creates an empty
  # checkpoint at first startup.
  helm uninstall "${TEST_HELM_RELEASE_NAME}" -n dra-driver-nvidia-gpu --wait --timeout=30s
  kubectl wait --for=delete pods -A -l app.kubernetes.io/name=dra-driver-nvidia-gpu --timeout=10s
  bash tests/bats/clean-state-dirs-all-nodes.sh
  kubectl get crd computedomains.resource.nvidia.com
  timeout -v 10 kubectl delete crd computedomains.resource.nvidia.com
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" NOARGS

  # Stage 2: pick any kubelet plugin pod.
  local _kpod _node
  _kpod=$(kubectl get pods -n dra-driver-nvidia-gpu \
    -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    -o jsonpath='{.items[0].metadata.name}')
  _node=$(kubectl get pod -n dra-driver-nvidia-gpu "${_kpod}" -o jsonpath='{.spec.nodeName}')
  log "targeting plugin pod ${_kpod} on node ${_node}"

  # Stage 3: two independent mutations to v2, both in one sed:
  #   1) Zero v2's checksum -- this trips the error.
  #   2) Insert a dummy unknown field -- this shows up in the diff. We
  #      want to confirm that the diff shows a field that is not
  #      contained in both JSON documents.
  # Inserting only an unknown field passes verification (the deserializer
  # drops it, so the recomputed checksum still matches), so the checksum
  # mutation is also required to exercise the diagnostic path.
  kubectl exec -n dra-driver-nvidia-gpu "${_kpod}" -c gpus -- sh -c '
    set -ex
    CP=/var/lib/kubelet/plugins/gpu.nvidia.com/checkpoint.json
    echo "BEFORE:" && cat "$CP" && echo
    sed -i "s/\"v2\":{\"checksum\":[0-9]*/\"v2\":{\"checksum\":0,\"dummy\":\"this-should-show-in-diff\"/" "$CP"
    echo "AFTER:" && cat "$CP" && echo
  '

  # Stage 4: bounce the plugin pod so it re-reads the corrupted file at
  # startup. The daemonset creates a replacement with a new name.
  kubectl delete pod -n dra-driver-nvidia-gpu "${_kpod}"

  local _newkpod="" _deadline=$((SECONDS + 60))
  while true; do
    _newkpod=$(kubectl get pods -n dra-driver-nvidia-gpu \
      -l dra-driver-nvidia-gpu-component=kubelet-plugin \
      --field-selector spec.nodeName="${_node}" \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null) || true
    if [ -n "${_newkpod}" ] && [ "${_newkpod}" != "${_kpod}" ]; then
      break
    fi
    (( SECONDS > _deadline )) && { echo "timeout waiting for replacement pod"; return 1; }
    sleep 1
  done
  log "replacement kubelet-plugin pod: ${_newkpod}"

  # Stage 5: poll for the diagnostic log line in the new pod's logs.
  _deadline=$((SECONDS + 60))
  while ! kubectl logs -n dra-driver-nvidia-gpu "${_newkpod}" -c gpus 2>/dev/null \
      | grep -q "checkpoint failed checksum verification; unified diff"; do
    if (( SECONDS > _deadline )); then
      echo "timeout waiting for unified diff log line"
      kubectl logs -n dra-driver-nvidia-gpu "${_newkpod}" -c gpus | tail -100 || true
      return 1
    fi
    sleep 2
  done

  run kubectl logs -n dra-driver-nvidia-gpu "${_newkpod}" -c gpus
  assert_output --partial "checkpoint failed checksum verification; unified diff"
  assert_output --partial "dummy"

  # Stage 6: cleanup. Wipe kubelet plugin state dirs to delete the corrupted
  # checkpoint, then helm uninstall.
  bash tests/bats/clean-state-dirs-all-nodes.sh
  helm uninstall "${TEST_HELM_RELEASE_NAME}" -n dra-driver-nvidia-gpu --wait --timeout=30s || true
}
