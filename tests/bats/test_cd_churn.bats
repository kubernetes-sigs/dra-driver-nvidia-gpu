# shellcheck disable=SC2148
# shellcheck disable=SC2329

# ComputeDomain churn in one driver-managed IMEX mode, selected with
# TEST_CD_MODE. Concurrent workers create random workloads, delete them in
# random ways and wait for their cleanup, while the invariants of
# cd_check_invariants are checked every 10 seconds. TEST_CD_CHAOS restarts the
# controller or a kubelet plugin every TEST_CD_CHAOS_INTERVAL seconds.

setup_file() {
  load 'helpers.sh'
  load 'lib/cd_modes.sh'
  _common_setup
  if [ "${MOCK_NVML:-}" = "true" ]; then return 0; fi
  cd_mode_install
}


teardown_file() {
  if [ "${MOCK_NVML:-}" = "true" ]; then return 0; fi
  cd_mode_finish
}


setup() {
  load 'helpers.sh'
  load 'lib/cd_modes.sh'
  _common_setup
  if [ "${MOCK_NVML:-}" = "true" ]; then skip "requires IMEX daemons"; fi
  log_objects
}


teardown() {
  if [ "${MOCK_NVML:-}" = "true" ]; then return 0; fi
  cd_delete_all_workloads || true
  cd_wait_no_leaks || true
}


bats::on_failure() {
  echo -e "\n\nFAILURE HOOK START"
  log_objects
  kubectl get computedomains -A -o wide || true
  kubectl get pods -n "${CD_NS}" -l "${CD_SELECTOR}" -o wide || true
  show_kubelet_plugin_error_logs
  echo -e "FAILURE HOOK END\n\n"
}


# bats test_tags=cd-modes,cd-churn
@test "CD modes (${TEST_CD_MODE:-per-cd}): churn for ${TEST_CD_CHURN_DURATION:-120}s" {
  local dir="${BATS_TEST_TMPDIR}/churn"
  local end workers i pids=() next_chaos
  mkdir -p "${dir}"
  touch "${dir}/violations.log" "${dir}/chaos.log"
  workers=$(cd_churn_workers)
  end=$(($(date +%s) + TEST_CD_CHURN_DURATION))
  log "churn: mode ${TEST_CD_MODE}, ${workers} workers, seed ${TEST_CD_CHURN_SEED}, chaos '${TEST_CD_CHAOS}', output ${dir}"

  if cd_mode_static; then
    export CD_DAEMON_BASELINE="${dir}/daemons"
    cd_static_daemons > "${CD_DAEMON_BASELINE}"
  fi

  # fd 3 is closed so that bats does not wait for the workers' output.
  for ((i = 1; i <= workers; i++)); do
    cd_churn_worker "${i}" "${end}" "${dir}/worker-${i}.log" 3>&- &
    pids+=("$!")
  done

  next_chaos=$(($(date +%s) + TEST_CD_CHAOS_INTERVAL))
  while (($(date +%s) < end)); do
    cd_check_invariants >> "${dir}/violations.log" 2>> "${dir}/checker.err" || true
    if [[ -n "${TEST_CD_CHAOS}" ]] && (($(date +%s) >= next_chaos)); then
      cd_chaos >> "${dir}/chaos.log" 2>&1 || echo "error chaos failed" >> "${dir}/chaos.log"
      next_chaos=$(($(date +%s) + TEST_CD_CHAOS_INTERVAL))
    fi
    sleep 10
  done
  log "churn: waiting for workers to finish their last workload"
  wait "${pids[@]}" || true
  cd_check_invariants >> "${dir}/violations.log" 2>> "${dir}/checker.err" || true

  unset CD_DAEMON_BASELINE
  cd_delete_all_workloads
  cd_wait_no_leaks > "${dir}/leaks.log" || true

  cat "${dir}"/worker-*.log > "${dir}/workloads.log"
  awk '!seen[$1 " " $2]++' "${dir}/violations.log" > "${dir}/violations.uniq"
  {
    echo "mode ${TEST_CD_MODE}, seed ${TEST_CD_CHURN_SEED}, ${workers} workers, ${TEST_CD_CHURN_DURATION}s"
    echo "workloads: $(grep -c '^ready ' "${dir}/workloads.log") ran, $(grep -c '^aborted ' "${dir}/workloads.log") aborted, $(grep -c '^error ' "${dir}/workloads.log") errors"
    echo "ready: $(awk '$1 == "ready" {print $3}' "${dir}/workloads.log" | cd_percentiles)"
    echo "cleanup: $(awk '$1 == "cleanup" {print $3}' "${dir}/workloads.log" | cd_percentiles)"
    echo "force-deleted: $(grep -c 'force=true' "${dir}/workloads.log"), ComputeDomain first: $(grep -c 'cd_first=true' "${dir}/workloads.log")"
    echo "chaos: $(grep -c '^chaos ' "${dir}/chaos.log") restarts"
    echo "violations: $(wc -l < "${dir}/violations.uniq")"
    cat "${dir}/violations.uniq"
    echo "workload errors:"
    grep '^error ' "${dir}/workloads.log" "${dir}/chaos.log" || true
    echo "leaks:"
    cat "${dir}/leaks.log"
  } | tee "${dir}/report.txt"

  run grep -c '^ready ' "${dir}/workloads.log"
  refute_output "0"
  run cat "${dir}/violations.uniq"
  assert_output ""
  run grep -h '^error ' "${dir}/workloads.log" "${dir}/chaos.log"
  assert_output ""
  run cat "${dir}/leaks.log"
  assert_output ""
}
