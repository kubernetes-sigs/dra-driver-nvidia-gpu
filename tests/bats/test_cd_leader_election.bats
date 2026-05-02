# shellcheck disable=SC2148,SC2329

setup_file() {
  load 'helpers.sh'
  _common_setup

  local _iargs=(
    "--set" "controller.replicas=2"
    "--set" "controller.leaderElection.enabled=true"
    "--set" "controller.leaderElection.leaseDuration=8s"
    "--set" "controller.leaderElection.renewDeadline=5s"
    "--set" "controller.leaderElection.retryPeriod=2s"
    "--set" "controller.affinity=null"
  )
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
}

teardown_file() {
  load 'helpers.sh'
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" NOARGS
}

setup() {
  load 'helpers.sh'
  _common_setup
  log_objects
}

bats::on_failure() {
  echo -e "\n\nFAILURE HOOK START"
  log_objects
  echo "--- controller pod logs (last 50 lines each) ---"
  kubectl logs \
    -l dra-driver-nvidia-gpu-component=controller \
    -n dra-driver-nvidia-gpu \
    --prefix --tail=50 || true
  kubectl get lease dra-driver-nvidia-gpu-controller \
    -n dra-driver-nvidia-gpu -o yaml || true
  echo -e "FAILURE HOOK END\n\n"
}

_get_lease_holder() {
  kubectl get lease dra-driver-nvidia-gpu-controller \
    -n dra-driver-nvidia-gpu \
    -o jsonpath='{.spec.holderIdentity}'
}

# lockID format: "<pod-name>-<uuid>" (set by runWithLeaderElection)
_holder_identity_to_pod_name() {
  local identity="$1"
  local pod
  for pod in $(kubectl get pods \
      -n dra-driver-nvidia-gpu \
      -l dra-driver-nvidia-gpu-component=controller \
      --no-headers \
      -o custom-columns=':metadata.name' 2>/dev/null); do
    if [[ "$identity" == "${pod}-"* ]]; then
      echo "$pod"
      return 0
    fi
  done
  return 1
}

# Polls until lease holder changes from <old_identity> or timeout elapses.
_wait_for_new_leader() {
  local old_identity="$1"
  local timeout="$2"
  local start=$SECONDS
  while true; do
    local current
    current=$(_get_lease_holder)
    if [[ -n "$current" && "$current" != "$old_identity" ]]; then
      echo "$current"
      return 0
    fi
    if (( SECONDS - start >= timeout )); then
      log "timeout waiting for new leader (old: $old_identity, current: $current)"
      return 1
    fi
    sleep 1
  done
}

# bats test_tags=fastfeedback
@test "CD controller: leader election: exactly one leader among two replicas" {
  if [ "${DISABLE_COMPUTE_DOMAINS:-}" = "true" ]; then skip "compute domain controller not deployed"; fi

  kubectl wait --for=condition=READY \
    pods \
    -l dra-driver-nvidia-gpu-component=controller \
    -n dra-driver-nvidia-gpu \
    --timeout=20s

  sleep 4

  local holder
  holder=$(_get_lease_holder)
  log "lease holder identity: $holder"
  [ -n "$holder" ]

  local leader_pod
  leader_pod=$(_holder_identity_to_pod_name "$holder")
  log "leader pod: $leader_pod"
  [ -n "$leader_pod" ]

  run kubectl get pod "$leader_pod" \
    -n dra-driver-nvidia-gpu \
    -o jsonpath='{.status.phase}'
  assert_output "Running"
}

# bats test_tags=fastfeedback
@test "CD controller: leader election: standby acquires lease after leader is force-deleted" {
  if [ "${DISABLE_COMPUTE_DOMAINS:-}" = "true" ]; then skip "compute domain controller not deployed"; fi

  sleep 4

  local old_holder
  old_holder=$(_get_lease_holder)
  [ -n "$old_holder" ]

  local leader_pod
  leader_pod=$(_holder_identity_to_pod_name "$old_holder")
  log "current leader pod: $leader_pod (identity: $old_holder)"
  [ -n "$leader_pod" ]

  kubectl delete pod "$leader_pod" \
    -n dra-driver-nvidia-gpu \
    --force --grace-period=0
  kubectl wait --for=delete pod/"$leader_pod" \
    -n dra-driver-nvidia-gpu --timeout=15s

  local new_holder
  new_holder=$(_wait_for_new_leader "$old_holder" 20)
  log "new lease holder identity: $new_holder"
  [ -n "$new_holder" ]

  local new_leader_pod
  new_leader_pod=$(_holder_identity_to_pod_name "$new_holder")
  log "new leader pod: $new_leader_pod"
  [ -n "$new_leader_pod" ]

  [ "$new_leader_pod" != "$leader_pod" ]

  run kubectl get pod "$new_leader_pod" \
    -n dra-driver-nvidia-gpu \
    -o jsonpath='{.status.phase}'
  assert_output "Running"
}