# shellcheck disable=SC2148
# shellcheck disable=SC2329

# ComputeDomain regressions in one driver-managed IMEX mode, selected with
# TEST_CD_MODE. `make -f tests/bats/Makefile tests-cd-modes` runs all modes.
# Every test checks the invariants of cd_check_invariants and leaves no
# objects behind.

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
  cd_check_invariants || true
  cd_find_leaks || true
  show_kubelet_plugin_error_logs
  echo -e "FAILURE HOOK END\n\n"
}


# bats test_tags=cd-modes
@test "CD modes (${TEST_CD_MODE:-per-cd}): channel injection" {
  local name
  name=$(cd_name single)
  cd_apply_workload "${name}" 1 1 any
  cd_wait_workload "${name}"
  cd_pod_channel "${name}-0"
  cd_assert_invariants
  cd_delete_workload "${name}"
  cd_wait_workload_gone "${name}"
  run cd_wait_no_leaks
  assert_success
}


# bats test_tags=cd-modes
@test "CD modes (${TEST_CD_MODE:-per-cd}): workload across a clique" {
  if (($(cd_clique_size) < 2)); then skip "needs a clique of two nodes"; fi
  local name size
  name=$(cd_name clique)
  size=$(cd_clique_size)
  cd_apply_workload "${name}" "${size}" 1 spread
  cd_wait_workload "${name}"
  kubectl wait -n "${CD_NS}" computedomain "${name}" --for=jsonpath='{.status.status}'=Ready --timeout=120s
  run bats_pipe kubectl get pods -n "${CD_NS}" -l "cd-workload=${name}" -o json \| \
    jq '[.items[].spec.nodeName] | unique | length'
  assert_output "${size}"
  cd_assert_invariants
  cd_delete_workload "${name}"
  cd_wait_workload_gone "${name}"
}


# Each workload is created right after deleting the previous one, so that a
# new ComputeDomain races the cleanup of the old one on the same node.
# bats test_tags=cd-modes
@test "CD modes (${TEST_CD_MODE:-per-cd}): node reuse without waiting for cleanup" {
  local node name i names=()
  node=$(cd_first_node)
  for i in 1 2 3; do
    name=$(cd_name reuse)
    names+=("${name}")
    cd_apply_workload "${name}" 1 1 node "${node}"
    cd_wait_workload "${name}"
    cd_assert_invariants
    cd_delete_workload "${name}"
  done
  for name in "${names[@]}"; do
    cd_wait_workload_gone "${name}"
  done
  run cd_wait_no_leaks
  assert_success
}


# bats test_tags=cd-modes
@test "CD modes (${TEST_CD_MODE:-per-cd}): cleanup after force-deleted pods" {
  local node name
  node=$(cd_first_node)
  name=$(cd_name force)
  cd_apply_workload "${name}" 1 1 node "${node}"
  cd_wait_workload "${name}"
  cd_delete_workload "${name}" true
  cd_wait_workload_gone "${name}"
  run cd_wait_no_leaks
  assert_success

  name=$(cd_name force)
  cd_apply_workload "${name}" 1 1 node "${node}"
  cd_wait_workload "${name}"
  cd_assert_invariants
  cd_delete_workload "${name}"
  cd_wait_workload_gone "${name}"
}


# bats test_tags=cd-modes
@test "CD modes (${TEST_CD_MODE:-per-cd}): ComputeDomain deleted before its pods run" {
  local name
  name=$(cd_name early)
  cd_apply_workload "${name}" 1 1 any
  kubectl delete computedomain -n "${CD_NS}" "${name}" --wait=false
  sleep 5
  cd_delete_workload "${name}"
  cd_wait_workload_gone "${name}"
  run cd_wait_no_leaks
  assert_success
}


# bats test_tags=cd-modes
@test "CD modes (${TEST_CD_MODE:-per-cd}): kubelet plugin restart keeps workloads" {
  local node name uid
  node=$(cd_first_node)
  name=$(cd_name kpr)
  cd_apply_workload "${name}" 1 1 node "${node}"
  cd_wait_workload "${name}"
  uid=$(kubectl get pod -n "${CD_NS}" "${name}-0" -o jsonpath='{.metadata.uid}')
  cd_restart_plugin "${node}"
  run kubectl get pod -n "${CD_NS}" "${name}-0" -o jsonpath='{.metadata.uid} {.status.phase}'
  assert_output "${uid} Running"
  cd_assert_invariants
  cd_delete_workload "${name}"
  cd_wait_workload_gone "${name}"

  name=$(cd_name kpr)
  cd_apply_workload "${name}" 1 1 node "${node}"
  cd_wait_workload "${name}"
  cd_delete_workload "${name}"
  cd_wait_workload_gone "${name}"
}


# bats test_tags=cd-modes
@test "CD modes (${TEST_CD_MODE:-per-cd}): controller restarts during create and delete" {
  local name
  name=$(cd_name ctrl)
  cd_apply_workload "${name}" 1 1 any
  cd_restart_controller
  cd_wait_workload "${name}"
  cd_assert_invariants
  cd_delete_workload "${name}"
  cd_restart_controller
  cd_wait_workload_gone "${name}"
  run cd_wait_no_leaks
  assert_success
}


# bats test_tags=cd-modes,cd-reservations
@test "CD modes (${TEST_CD_MODE:-per-cd}): one reserved channel per template" {
  if ! cd_mode_reservations; then skip "needs channel reservations"; fi
  local name c0 c1
  name=$(cd_name tmpl)
  cd_apply_workload "${name}" 2 2 colocate
  cd_wait_workload "${name}"
  c0=$(cd_pod_channel "${name}-0")
  c1=$(cd_pod_channel "${name}-1")
  [ "${c0}" != "${c1}" ]
  cd_assert_invariants
  # The controller assigns the channel at admission, into the claim status.
  run bats_pipe kubectl get resourceclaims -n "${CD_NS}" -o json \| \
    jq -r --arg p "${name}-" '.items[] | select(.metadata.name | startswith($p))
      | (.status.allocation.devices.results[] | "\(.device) \(.bindingConditions // [] | join(","))"),
        (.status.devices[]? | select(.device == "channel") | "assigned \(.data.channel)")'
  assert_success
  assert_line "channel compute-domain.nvidia.com/ChannelAdmitted"
  assert_line "assigned ${c0}"
  assert_line "assigned ${c1}"
  cd_delete_workload "${name}"
  cd_wait_workload_gone "${name}"
}


# bats test_tags=cd-modes,cd-per-cd
@test "CD modes (${TEST_CD_MODE:-per-cd}): a node runs one ComputeDomain at a time" {
  if cd_mode_static; then skip "static daemons share nodes"; fi
  local node a b
  node=$(cd_first_node)
  a=$(cd_name excl)
  b=$(cd_name excl)
  cd_apply_workload "${a}" 1 1 node "${node}"
  cd_wait_workload "${a}"
  cd_apply_workload "${b}" 1 1 node "${node}"
  run cd_wait_workload "${b}" 60
  assert_failure
  cd_assert_invariants
  cd_delete_workload "${a}"
  cd_wait_workload "${b}"
  cd_assert_invariants
  cd_delete_workload "${b}"
  cd_wait_workload_gone "${a}"
  cd_wait_workload_gone "${b}"
}


# bats test_tags=cd-modes,cd-static
@test "CD modes (${TEST_CD_MODE:-per-cd}): static daemons serve ComputeDomains on one node" {
  if ! cd_mode_static; then skip "needs static daemons"; fi
  local node names=() name i
  node=$(cd_first_node)
  export CD_DAEMON_BASELINE="${BATS_TEST_TMPDIR}/daemons"
  cd_static_daemons > "${CD_DAEMON_BASELINE}"
  for i in 1 2 3; do
    name=$(cd_name shared)
    names+=("${name}")
    cd_apply_workload "${name}" 1 1 node "${node}"
  done
  for name in "${names[@]}"; do
    cd_wait_workload "${name}"
  done
  run bats_pipe kubectl logs -n "${CD_NS}" -l "cd-workload in (${names[0]},${names[1]},${names[2]})" \| sort -u \| wc -l
  assert_output "3"
  cd_assert_invariants
  for name in "${names[@]}"; do
    cd_delete_workload "${name}"
  done
  for name in "${names[@]}"; do
    cd_wait_workload_gone "${name}"
  done
  cd_assert_invariants
}


# bats test_tags=cd-modes,cd-static
@test "CD modes (${TEST_CD_MODE:-per-cd}): static daemons serve two ComputeDomains across a clique" {
  if ! cd_mode_static; then skip "needs static daemons"; fi
  if (($(cd_clique_size) < 2)); then skip "needs a clique of two nodes"; fi
  local a b size
  size=$(cd_clique_size)
  export CD_DAEMON_BASELINE="${BATS_TEST_TMPDIR}/daemons"
  cd_static_daemons > "${CD_DAEMON_BASELINE}"
  a=$(cd_name spread)
  b=$(cd_name spread)
  cd_apply_workload "${a}" "${size}" 1 spread
  cd_apply_workload "${b}" "${size}" 1 spread
  cd_wait_workload "${a}"
  cd_wait_workload "${b}"
  cd_assert_invariants
  cd_delete_workload "${a}"
  cd_delete_workload "${b}"
  cd_wait_workload_gone "${a}"
  cd_wait_workload_gone "${b}"
  cd_assert_invariants
}


# bats test_tags=cd-modes,cd-reservations
@test "CD modes (${TEST_CD_MODE:-per-cd}): pods of a template share its channel on a node" {
  if ! cd_mode_reservations; then skip "needs channel reservations"; fi
  local name c0 c1
  name=$(cd_name share)
  cd_apply_workload "${name}" 2 1 colocate
  cd_wait_workload "${name}"
  c0=$(cd_pod_channel "${name}-0")
  c1=$(cd_pod_channel "${name}-1")
  assert_equal "${c0}" "${c1}"
  cd_assert_invariants
  cd_delete_workload "${name}"
  cd_wait_workload_gone "${name}"
}


# bats test_tags=cd-modes,cd-reservations
@test "CD modes (${TEST_CD_MODE:-per-cd}): one shared channel device per node" {
  if ! cd_mode_reservations; then skip "needs channel reservations"; fi
  run bats_pipe kubectl get resourceslices --field-selector spec.driver=compute-domain.nvidia.com -o json \| \
    jq -r '[.items[].spec.devices[]? | select(.attributes.type.string | . == "imex-channel" or . == "channel")]
      | map("\(.name) type=\(.attributes.type.string) id=\(.attributes.id.int // "none") multi=\(.allowMultipleAllocations)") | unique[]'
  assert_success
  assert_line 'channel type=imex-channel id=none multi=true'
  # Channel 0 stays for ComputeDomains created before the gate.
  assert_line 'channel-0 type=channel id=0 multi=null'
  assert_equal "$(wc -l <<< "${output}")" 2
}


# bats test_tags=cd-modes,cd-reservations
@test "CD modes (${TEST_CD_MODE:-per-cd}): the channel device names the owner or clique of its node" {
  if ! cd_mode_reservations; then skip "needs channel reservations"; fi
  local name uid node deadline
  name=$(cd_name owner)
  cd_apply_workload "${name}" 1 1 any
  cd_wait_workload "${name}"
  uid=$(kubectl get computedomain -n "${CD_NS}" "${name}" -o jsonpath='{.metadata.uid}')
  node=$(kubectl get pods -n "${CD_NS}" -l "cd-workload=${name}" -o jsonpath='{.items[0].spec.nodeName}')
  if cd_mode_node_owner; then
    # The scheduler skips this node for other ComputeDomains.
    assert_equal "$(cd_channel_device_owner "${node}")" "${uid}"
  else
    assert_equal "$(cd_channel_device_owner "${node}")" ""
    assert_equal "$(cd_channel_device_attr "${node}" cliqueID)" \
      "$(kubectl get node "${node}" -o jsonpath='{.metadata.labels.nvidia\.com/gpu\.clique}')"
  fi
  cd_delete_workload "${name}"
  cd_wait_workload_gone "${name}"
  if cd_mode_node_owner; then
    deadline=$((SECONDS + 60))
    while [[ -n "$(cd_channel_device_owner "${node}")" ]]; do
      ((SECONDS < deadline)) || fail "node ${node} is still owned by ${uid}"
      sleep 2
    done
  fi
}


# bats test_tags=cd-modes
@test "CD modes (${TEST_CD_MODE:-per-cd}): a ComputeDomain sets channel or channels" {
  local name
  name=$(cd_name spec)
  run kubectl apply --dry-run=server -f - <<< "
apiVersion: resource.nvidia.com/v1beta1
kind: ComputeDomain
metadata: {name: ${name}, namespace: ${CD_NS}}
spec:
  channel: {resourceClaimTemplate: {name: ${name}-a}}
  channels: [{resourceClaimTemplate: {name: ${name}-b}}]"
  assert_failure
  assert_output --partial "exactly one of channel and channels must be set"
  run kubectl apply --dry-run=server -f - <<< "
apiVersion: resource.nvidia.com/v1beta1
kind: ComputeDomain
metadata: {name: ${name}, namespace: ${CD_NS}}
spec:
  channels: [{resourceClaimTemplate: {name: ${name}-a}}, {resourceClaimTemplate: {name: ${name}-a}}]"
  assert_failure
  assert_output --partial "resourceClaimTemplate names must be unique"
}


# Changes the daemon template, so it runs after the tests that expect the
# daemons to stay.
# bats test_tags=cd-modes,cd-static
@test "CD modes (${TEST_CD_MODE:-per-cd}): static daemon rollout keeps workloads" {
  if ! cd_mode_static; then skip "needs static daemons"; fi
  local name uid before verbosity
  name=$(cd_name roll)
  cd_apply_workload "${name}" 1 1 any
  cd_wait_workload "${name}"
  uid=$(kubectl get pod -n "${CD_NS}" "${name}-0" -o jsonpath='{.metadata.uid}')
  before=$(cd_static_daemons)

  verbosity=$(kubectl get computedomaindaemonset default -o jsonpath='{.spec.logVerbosity}')
  kubectl patch computedomaindaemonset default --type=merge \
    -p "{\"spec\": {\"logVerbosity\": $(( ${verbosity:-0} == 6 ? 5 : 6 ))}}"

  # Every daemon is replaced once, while the workload keeps running.
  local deadline=$((SECONDS + 1800)) now node old
  while true; do
    now=$(cd_static_daemons)
    local replaced=0 total=0
    while read -r node old; do
      total=$((total + 1))
      grep -q "^${node} " <<< "${now}" && ! grep -qx "${node} ${old}" <<< "${now}" && replaced=$((replaced + 1))
    done <<< "${before}"
    ((replaced == total)) && break
    ((SECONDS < deadline))
    run kubectl get pod -n "${CD_NS}" "${name}-0" -o jsonpath='{.metadata.uid} {.status.phase}'
    assert_output "${uid} Running"
    sleep 10
  done
  kubectl wait computedomaindaemonset default --for=condition=Ready --timeout=600s
  run kubectl get pod -n "${CD_NS}" "${name}-0" -o jsonpath='{.metadata.uid} {.status.phase}'
  assert_output "${uid} Running"
  cd_assert_invariants
  cd_delete_workload "${name}"
  cd_wait_workload_gone "${name}"
}


# bats test_tags=cd-modes,cd-static
@test "CD modes (${TEST_CD_MODE:-per-cd}): static daemons outlive deletion until workloads end" {
  if ! cd_mode_static; then skip "needs static daemons"; fi
  local name
  name=$(cd_name del)
  cd_apply_workload "${name}" 1 1 any
  cd_wait_workload "${name}"
  export CD_DAEMON_BASELINE="${BATS_TEST_TMPDIR}/daemons"
  cd_static_daemons > "${CD_DAEMON_BASELINE}"

  kubectl delete computedomaindaemonset default --wait=false
  sleep 30
  kubectl wait computedomaindaemonset default --for=condition=DeletionBlocked --timeout=60s
  cd_wait_workload "${name}" 10
  cd_assert_invariants

  unset CD_DAEMON_BASELINE
  cd_delete_workload "${name}"
  cd_wait_workload_gone "${name}"
  kubectl wait computedomaindaemonset default --for=delete --timeout=600s
  cd_apply_cdds
}
