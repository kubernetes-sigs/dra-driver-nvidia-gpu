# shellcheck disable=SC2148
# shellcheck disable=SC2329

# Currently, the tests defined in this file deliberately depend on each other
# and are expected to execute in the order defined. In the future, we want to
# build test dependency injection (with fixtures), and work towards clean
# _isolation_ between tests. To that end, we will hopefully find fast and
# reliable strategies to conceptually prevent cross-contamination from
# happening. Tools like `etcdctl` will be helpful.

# Hint: when developing/testing a specific test, for faster iteration add the
# comment `# bats test_tags=bats:focus` on top of the current test. This can
# help (requires the test to be self-contained though, i.e. it sets up its
# dependencies' does not rely on a previous test for that).

# Note(JP): bats swallows output of setup upon success (regardless of cmdline
# args such as `--show-output-of-passing-tests`). Ref:
# https://github.com/bats-core/bats-core/issues/540#issuecomment-1013521656 --
# it however does emit output upon failure.


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
  get_all_cd_daemon_logs_for_cd_name "imex-channel-injection" || true
  echo -e "FAILURE HOOK END\n\n"
}


# bats test_tags=fastfeedback
@test "CDs: daemon shutdown: confirm CD status cleanup" {
  if [ "${MOCK_NVML:-}" = "true" ]; then skip "requires IMEX daemon"; fi
  kubectl apply -f demo/specs/imex/channel-injection.yaml
  kubectl wait --for=condition=READY pods imex-channel-injection --timeout=100s
  run kubectl logs imex-channel-injection
  assert_output --partial "channel0"

  local LOGPATH="${BATS_TEST_TMPDIR}/cd-daemon.log"
  local PNAME
  local CD_UID
  CD_UID=$(kubectl describe computedomains.resource.nvidia.com imex-channel-injection | grep UID | awk '{print $2}')
  log "CD UID: ${CD_UID}"
  PNAME=$( \
    kubectl get pods -n dra-driver-nvidia-gpu | \
    grep "${CD_UID}" | \
    awk '{print $1}'
  )
  log "CD daemon pod name: ${PNAME}"

  # Expect `nodes` key to be present in CD status.
  sleep 4
  run bats_pipe kubectl get computedomain imex-channel-injection -o json \| jq '.status'
  assert_output --partial 'nodes'

  echo "attach background log follower to daemon pod: $PNAME"
  kubectl logs -n dra-driver-nvidia-gpu --follow "$PNAME" > "$LOGPATH" 2>&1 &
  kubectl delete pods imex-channel-injection

  # Note: the log follower child process terminates when the pod terminates.
  kubectl wait --for=delete pods imex-channel-injection --timeout=10s

  # Expect `nodes` key to not be be present (single-node CD). Note that with
  # `ComputeDomainCliques=true` (new default), the node status removal is not
  # done anymore by the CD daemons, but asynchronously by the CD controller.
  # Hence, the update expectedly takes "a moment" (at most ~two seconds, though,
  # expectedlyt. Ideally, we check periodically up until a deadline. Quick&dirty
  # and to-be-fixed (TODO): sleep a brief moment.
  sleep 3
  run bats_pipe kubectl get computedomain imex-channel-injection -o json \| jq '.status'
  refute_output --partial 'nodes'

  # Inspect CD daemon log, dump tail for easier debug-on-failure.
  cat "$LOGPATH" | tail -n 50

  # Explicitly confirm cleanup-on-shutdown behavior by inspecting CD log.
  cat "$LOGPATH" | grep "Successfully removed daemon" | \
    grep "from ComputeDomain default/imex-channel-injection"

  # Delete CD.
  kubectl delete computedomain imex-channel-injection
}


# bats test_tags=fastfeedback
@test "CDs: reject unknown field in opaque cfg in CD chan ResourceClaim" {
  if [ "${MOCK_NVML:-}" = "true" ]; then skip "requires IMEX daemon"; fi
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" NOARGS

  envsubst < tests/bats/specs/rc-opaque-cfg-unknown-field.yaml.tmpl > \
    "${BATS_TEST_TMPDIR}"/rc-opaque-cfg-unknown-field.yaml
  cd "${BATS_TEST_TMPDIR}"

  local SPEC="rc-opaque-cfg-unknown-field.yaml"

  # Create pod with random name suffix.
  # Store ref of the form `pod/batssuite-pod-boc-brs2l`.
  local POD
  POD=$(kubectl create -f "${SPEC}" | grep pod | awk '{print $1;}')

  # Confirm ContainerCreating state (no failure yet though).
  kubectl wait \
    --for=jsonpath='{.status.containerStatuses[0].state.waiting.reason}'=ContainerCreating \
    --timeout=10s \
    "${POD}"

  # Rather quickly, we expect an event with reason
  # `FailedPrepareDynamicResources`. That's not typically the method users
  # discover the error.
  wait_for_pod_event "${POD}" FailedPrepareDynamicResources 10

  # This is how users probably see this error first.
  kubectl describe "${POD}" | grep FailedPrepareDynamicResources | \
    grep "error preparing devices"  | \
    grep 'strict decoding error: unknown field "unexpectedField"'

  # Confirm that precise root cause can also be inferred from
  # CD kubelet plugin logs.
  kubectl logs \
    -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    -n dra-driver-nvidia-gpu \
    --prefix --tail=-1 | \
      grep 'Permanent error' | \
      grep 'strict decoding error: unknown field "unexpectedField"'

  # Clean up.
  kubectl delete "${POD}"
  kubectl delete resourceclaim batssuite-rc-bad-opaque-config
  kubectl wait --for=delete "${POD}" --timeout=10s
}


# bats test_tags=fastfeedback
@test "CDs: self-initiated unprepare of stale RCs in PrepareStarted" {
  if [ "${MOCK_NVML:-}" = "true" ]; then skip "requires IMEX daemon"; fi
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" NOARGS

  # Stage 1: provoke partially prepared claim.
  #
  # Based on the "catch unknown field in opaque cfg in ResourceClaim" test
  # above: Provoke a permanent Prepare() error, leaving behind a partially
  # prepared claim in the checkpoint.
  envsubst < tests/bats/specs/rc-opaque-cfg-unknown-field.yaml.tmpl > \
    "${BATS_TEST_TMPDIR}"/rc-opaque-cfg-unknown-field.yaml
  local SPEC="${BATS_TEST_TMPDIR}/rc-opaque-cfg-unknown-field.yaml"
  local POD
  POD=$(kubectl create -f "${SPEC}" | grep pod | awk '{print $1;}')
  kubectl wait \
    --for=jsonpath='{.status.containerStatuses[0].state.waiting.reason}'=ContainerCreating \
    --timeout=10s \
    "${POD}"
  wait_for_pod_event "${POD}" FailedPrepareDynamicResources 10
  run kubectl logs \
    -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    -n dra-driver-nvidia-gpu \
    --prefix --tail=-1
  assert_output --partial 'strict decoding error: unknown field "unexpectedField"'

  # Stage 2: test that cleanup routine leaves this claim alone ('not stale')
  #
  # Re-install, flip log verbosity just to enforce container restart. This
  # ensures that the cleanup runs immediately (it runs upon startup, and then
  # only N minutes later again).
  local _iargs=("--set" "logVerbosity=5")
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
  sleep 1   # give the on-startup cleanup a chance to run.
  run kubectl logs \
    -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    -n dra-driver-nvidia-gpu \
    --prefix --tail=-1
  assert_output --partial "partially prepared claim not stale: default/batssuite-rc-bad-opaque-config"

  # Stage 3: simulate stale claim, test cleanup.
  #
  # To that end, uninstall the driver and then remove both pod and RC from the API server.
  # Then, re-install DRA driver and confirm detection and removal of stale claim.
  helm uninstall -n dra-driver-nvidia-gpu dra-driver-nvidia-gpu-batssuite --wait
  kubectl delete "${POD}" --force
  kubectl delete resourceclaim batssuite-rc-bad-opaque-config
  local _iargs=("--set" "logVerbosity=6")
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
  sleep 1  # give the on-startup cleanup a chance to run.

  run kubectl logs \
    -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    -n dra-driver-nvidia-gpu \
    --prefix --tail=-1
  assert_output --partial "Deleted claim from checkpoint: default/batssuite-rc-bad-opaque-config"
  assert_output --partial "Checkpointed RC cleanup: unprepared stale claim: default/batssuite-rc-bad-opaque-config"

  # Stage 4: appendix -- happens shortly thereafter: we do get a
  # UnprepareResourceClaims() call for this claim. Why? It's a noop because the
  # cleanup above was faster.
  sleep 4
  run kubectl logs \
    -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    -n dra-driver-nvidia-gpu \
    --prefix --tail=-1
  assert_output --partial "Unprepare noop: claim not found in checkpoint data"
}


# bats test_tags=fastfeedback
@test "CDs: global CD status" {
  if [ "${MOCK_NVML:-}" = "true" ]; then skip "requires IMEX daemon"; fi
  kubectl apply -f demo/specs/imex/channel-injection.yaml
  kubectl wait --for=condition=READY pods imex-channel-injection --timeout=100s

  # Expect global CD status to be `Ready`. TODO: check periodicaly, until deadline
  sleep 4
  run bats_pipe kubectl get computedomain imex-channel-injection -o json \| jq '.status.status'
  assert_output --partial 'Ready'
  refute_output --partial 'NotReady'

  # Delete worker pod, and hence expect CD daemon pod to be torn down.
  kubectl delete pods imex-channel-injection
  kubectl wait --for=delete pods imex-channel-injection --timeout=10s

  # Expect CD controller to handle the .status.nodes list to become empty.
  # As of now, because of `numNodes=1` in the workload, this is expected to
  # result in the global CD status transitioning to NotReady. TODO: check
  # periodically, until deadline.
  sleep 5
  run bats_pipe kubectl get computedomain imex-channel-injection -o json \| jq '.status.status'
  assert_output --partial 'NotReady'

  kubectl delete -f demo/specs/imex/channel-injection.yaml --ignore-not-found=true
}


# bats test_tags=fastfeedback
@test "CDs: daemon Ready -> NotReady transition (not in context of shutdown)" {
  if [ "${MOCK_NVML:-}" = "true" ]; then skip "requires IMEX daemon"; fi
  kubectl apply -f demo/specs/imex/channel-injection.yaml
  kubectl wait --for=condition=READY pods imex-channel-injection --timeout=100s
  run kubectl logs imex-channel-injection
  assert_output --partial "channel0"

  local CD_UID
  CD_UID=$(kubectl describe computedomains.resource.nvidia.com imex-channel-injection | grep UID | awk '{print $2}')
  log "CD UID: ${CD_UID}"

  local DAEMON_POD
  DAEMON_POD=$(kubectl get pods -n dra-driver-nvidia-gpu | grep "${CD_UID}" | awk '{print $1}')
  log "CD daemon pod name: ${DAEMON_POD}"

  sleep 4
  # Expect global CD status to be Ready.
  local CD_STATUS
  CD_STATUS=$(kubectl get computedomain imex-channel-injection -o json | jq '.status.status')
  log "CD status ${CD_STATUS}"
  run bats_pipe kubectl get computedomain imex-channel-injection -o json \| jq '.status.status'
  assert_output --partial 'Ready'
  refute_output --partial 'NotReady'

  # Find the PID of the IMEX daemon child process inside the daemon pod,
  # using the /proc filesystem.
  local IMEX_PID
  IMEX_PID=$(kubectl exec -n dra-driver-nvidia-gpu "$DAEMON_POD" -- sh -c \
    'for d in /proc/[0-9]*; do [ "$(cat $d/comm 2>/dev/null)" = "nvidia-imex" ] && echo ${d##*/} && break; done')
  log "nvidia-imex PID in daemon pod: ${IMEX_PID}"

  # Send SIGSTOP to pause the IMEX daemon child process.
  kubectl exec -n dra-driver-nvidia-gpu "$DAEMON_POD" -- sh -c "kill -STOP ${IMEX_PID}"

  # Wait for the daemon pod to transition to NotReady.
  kubectl wait \
    --for=condition=Ready=false \
    -n dra-driver-nvidia-gpu pod/"$DAEMON_POD" \
    --timeout=30s

  # Expect global CD status to be NotReady. The node entry should still be
  # present (unlike in a shutdown scenario where the node is removed entirely).
  sleep 4
  run bats_pipe kubectl get computedomain imex-channel-injection -o json \| jq '.status.status'
  assert_output --partial 'NotReady'

  # Confirm the node entry is still present with NotReady status — key
  # distinction from shutdown where the node entry is removed entirely.
  run bats_pipe kubectl get computedomain imex-channel-injection -o json \| jq '.status.nodes[0].status'
  assert_output --partial 'NotReady'

  # Resume the IMEX process, transitioning the pod back to Ready.
  kubectl exec -n dra-driver-nvidia-gpu "$DAEMON_POD" -- sh -c "kill -CONT ${IMEX_PID}"

  # Wait for the daemon pod to transition back to Ready.
  kubectl wait \
    --for=condition=Ready \
    -n dra-driver-nvidia-gpu pod/"$DAEMON_POD" \
    --timeout=60s

  # Expect global CD status to recover to Ready.
  sleep 4
  run bats_pipe kubectl get computedomain imex-channel-injection -o json \| jq '.status.status'
  assert_output --partial 'Ready'
  refute_output --partial 'NotReady'

  # Clean up.
  kubectl delete -f demo/specs/imex/channel-injection.yaml
  kubectl wait --for=delete pods imex-channel-injection --timeout=10s
}


# bats test_tags=fastfeedback
@test "CDs: IMEX channel injection (featureGates.ComputeDomainClique=true)" {
  if [ "${MOCK_NVML:-}" = "true" ]; then skip "requires IMEX daemon"; fi
  local _iargs=("--set" "logVerbosity=6" "--set" "featureGates.ComputeDomainCliques=true")
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
  apply_check_delete_workload_imex_chan_inject
}
