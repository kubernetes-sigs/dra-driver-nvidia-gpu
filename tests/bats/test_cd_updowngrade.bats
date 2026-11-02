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


@test "CDs: downgrade: current-dev -> last-stable" {
  if [ "${MOCK_NVML:-}" = "true" ]; then skip "requires IMEX daemon"; fi
  # Stage 1: apply workload, but do not delete.
  kubectl apply -f demo/specs/imex/channel-injection.yaml
  kubectl wait --for=condition=READY pods imex-channel-injection --timeout=60s
  run kubectl logs imex-channel-injection
  assert_imex_channel_injected

  # Stage 2: upgrade as users would do (explicitly not downgrade the CRD).
  iupgrade_wait "$TEST_CHART_LASTSTABLE_REPO" "$TEST_CHART_LASTSTABLE_VERSION" NOARGS

  # Stage 3: confirm workload deletion works post-upgrade.
  timeout -v 80 kubectl delete -f demo/specs/imex/channel-injection.yaml
  kubectl wait --for=delete pods imex-channel-injection --timeout=10s

  # Stage 4: fresh create-confirm-delete workload cycle.
  apply_check_delete_workload_imex_chan_inject
}


# bats test_tags=fastfeedback
@test "CDs: upgrade: wipe-state, install-last-stable, upgrade-to-current-dev" {
  if [ "${MOCK_NVML:-}" = "true" ]; then skip "requires IMEX daemon"; fi
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
  kubectl apply -f demo/specs/imex/channel-injection.yaml
  kubectl wait --for=condition=READY pods imex-channel-injection --timeout=60s
  run kubectl logs imex-channel-injection
  assert_imex_channel_injected

  # Stage 4: update CRD, as a user would do.
  kubectl apply -f "${CRD_UPGRADE_URL}"

  # Stage 5: install target version (as users would do).
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" NOARGS

  # Stage 6: confirm deleting old workload works (critical, see above).
  timeout -v 60 kubectl delete -f demo/specs/imex/channel-injection.yaml
  kubectl wait --for=delete pods imex-channel-injection --timeout=60s

  # Stage 7: fresh create-confirm-delete workload cycle.
  apply_check_delete_workload_imex_chan_inject
}
