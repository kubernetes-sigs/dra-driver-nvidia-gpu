# shellcheck disable=SC2148
# shellcheck disable=SC2329

#setup_file() {
#   load 'helpers.sh'
#   local _iargs=("--set" "logVerbosity=6")
#   iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
#}


setup () {
  load 'helpers.sh'
  _common_setup
}


# bats test_tags=fastfeedback
@test "CDs: controller/plugin: startup config / detail in logs on level 0" {
  if [ "${MOCK_NVML:-}" = "true" ]; then skip "requires IMEX daemon"; fi
  local _iargs=("--set" "logVerbosity=0")
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs

  run kubectl logs -l dra-driver-nvidia-gpu-component=controller -n dra-driver-nvidia-gpu --tail=-1
  assert_output --partial "Verbosity:"
  assert_output --partial '"MPSSupport":false'
  assert_output --partial 'additionalNamespaces:'

  run kubectl logs -l dra-driver-nvidia-gpu-component=kubelet-plugin -n dra-driver-nvidia-gpu --tail=-1
  assert_output --partial "Verbosity"
  assert_output --partial "nodeName"
  assert_output --partial "Discovered GPU clique ID"
  assert_output --partial "driver version validation"
}


@test "CDs: controller: test log verbosity levels" {
  if [ "${MOCK_NVML:-}" = "true" ]; then skip "requires IMEX daemon"; fi
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" NOARGS

  # Deploy workload: give the controller something to log about.
  log_objects
  kubectl apply -f demo/specs/imex/channel-injection.yaml
  kubectl wait --for=condition=READY pods imex-channel-injection --timeout=100s
  run kubectl logs imex-channel-injection
  assert_output --partial "channel0"

  echo "test level 0"
  local _iargs=("--set" "logVerbosity=0")
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
  kubectl logs -l dra-driver-nvidia-gpu-component=controller -n dra-driver-nvidia-gpu --tail=-1
  run kubectl logs -l dra-driver-nvidia-gpu-component=controller -n dra-driver-nvidia-gpu --tail=-1
  assert_output --partial 'controller manager config'
  assert_output --partial 'maxNodesPerIMEXDomain'

  echo "test level 1"
  local _iargs=("--set" "logVerbosity=1")
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
  run kubectl logs -l dra-driver-nvidia-gpu-component=controller -n dra-driver-nvidia-gpu --tail=-1
  refute_output --partial 'Processing added or updated ComputeDomain'
  refute_output --partial 'reflector.go'
  refute_output --partial 'Caches populated'
  refute_output --partial 'round_trippers.go'
  refute_output --partial 'Listing and watching'

  echo "test level 2"
  local _iargs=("--set" "logVerbosity=2")
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
  run kubectl logs -l dra-driver-nvidia-gpu-component=controller -n dra-driver-nvidia-gpu --tail=-1
  assert_output --partial 'Processing added or updated ComputeDomain'
  assert_output --partial 'reflector.go'
  assert_output --partial 'Caches populated'
  refute_output --partial 'Listing and watching'
  refute_output --partial 'round_trippers.go'

  echo "test level 3"
  local _iargs=("--set" "logVerbosity=3")
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
  run kubectl logs -l dra-driver-nvidia-gpu-component=controller -n dra-driver-nvidia-gpu --tail=-1
  assert_output --partial 'reflector.go'
  assert_output --partial 'Caches populated'
  assert_output --partial 'Listing and watching'
  refute_output --partial 'round_trippers.go'

  echo "test level 4"
  local _iargs=("--set" "logVerbosity=4")
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
  run kubectl logs -l dra-driver-nvidia-gpu-component=controller -n dra-driver-nvidia-gpu --tail=-1
  refute_output --partial 'round_trippers.go'

  echo "test level 5"
  local _iargs=("--set" "logVerbosity=5")
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
  run kubectl logs -l dra-driver-nvidia-gpu-component=controller -n dra-driver-nvidia-gpu --tail=-1
  refute_output --partial 'round_trippers.go'

  echo "test level 6"
  local _iargs=("--set" "logVerbosity=6")
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
  run kubectl logs -l dra-driver-nvidia-gpu-component=controller -n dra-driver-nvidia-gpu --tail=-1
  assert_output --partial 'Cleanup: perform for'
  assert_output --partial 'round_trippers.go'
  assert_output --partial '"Response" verb="GET"'

  # Delete workload and hence CD daemon
  kubectl delete -f demo/specs/imex/channel-injection.yaml
  kubectl wait --for=delete pods imex-channel-injection --timeout=10s
}

@test "CDs: daemon: test log verbosity levels" {
  if [ "${MOCK_NVML:-}" = "true" ]; then skip "requires IMEX daemon"; fi
  log_objects
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" NOARGS

  echo "test level 6"
  local _iargs=("--set" "logVerbosity=6")
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs

  kubectl apply -f demo/specs/imex/channel-injection.yaml
  kubectl wait --for=condition=READY pods imex-channel-injection --timeout=100s

  # Confirm that the CD daemon logs on level six
  run get_all_cd_daemon_logs_for_cd_name "imex-channel-injection"
  assert_output --partial 'Wait for updated ComputeDomainDaemonInfo list'  # level 1 msg
  assert_output --partial 'round_trippers.go'  # level 6 msg

  # Delete workload and hence CD daemon
  kubectl delete -f demo/specs/imex/channel-injection.yaml
  kubectl wait --for=delete pods imex-channel-injection --timeout=10s

  echo "test level 0"
  log_objects
  # Reconfigure (only the) log verbosity for CD daemons spawned in the future.
  # The deployment mutation via `set env` below is expected to restart the
  # controller. Wait for the old controller pod to go away (to be sure that the
  # new LOG_VERBOSITY_CD_DAEMON setting applies), and make sure controller
  # deployment is still READY before moving on (make sure 1/1 READY).
  CPOD_OLD="$(get_current_controller_pod_name)"
  kubectl set env deployment dra-driver-nvidia-gpu-controller -n dra-driver-nvidia-gpu  LOG_VERBOSITY_CD_DAEMON=0
  echo "wait --for=delete: $CPOD_OLD"
  kubectl wait --for=delete pods "$CPOD_OLD" -n dra-driver-nvidia-gpu --timeout=10s
  echo "returned: wait --for=delete"
  CPOD_NEW="$(get_current_controller_pod_name)"
  kubectl wait --for=condition=READY pods "$CPOD_NEW" -n dra-driver-nvidia-gpu --timeout=10s
  echo "new controller pod is in effect"

  # Spawn new workload
  kubectl apply -f demo/specs/imex/channel-injection.yaml
  kubectl wait --for=condition=READY pods imex-channel-injection --timeout=100s

  # Confirm that the CD daemon now does not contain a level 1 msg
  run get_all_cd_daemon_logs_for_cd_name "imex-channel-injection"
  refute_output --partial 'Wait for updated ComputeDomainDaemonInfo list'  # expected level 1 msg

  # Delete workload
  kubectl delete -f demo/specs/imex/channel-injection.yaml
  kubectl wait --for=delete pods imex-channel-injection --timeout=10s

  # The last `kubectl set env deployment` above may mutate a field owner to
  # `kubectl-set` and that may negatively impact the next `helm upgrade -i ...`.
  # Explicitly uninstall the Helm chart now, to not affect other tests with
  # that.
  helm uninstall -n dra-driver-nvidia-gpu dra-driver-nvidia-gpu-batssuite
}
