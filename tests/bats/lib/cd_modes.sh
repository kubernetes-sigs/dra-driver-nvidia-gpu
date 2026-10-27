# shellcheck shell=bash
# Copyright The Kubernetes Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Helpers of test_cd_modes.bats and test_cd_churn.bats. TEST_CD_MODE selects
# the driver-managed IMEX mode the suites install and check.

: "${TEST_CD_MODE:=per-cd}"
: "${TEST_CD_IMAGE:=ubuntu:24.04}"
: "${TEST_CD_READY_TIMEOUT:=300}"
: "${TEST_CD_CLEANUP_TIMEOUT:=300}"
: "${TEST_CD_STUCK_TIMEOUT:=600}"
: "${TEST_CD_CHURN_DURATION:=120}"
: "${TEST_CD_CHURN_WORKERS:=}"
: "${TEST_CD_CHURN_NODES_PER_CD:=1,2}"
: "${TEST_CD_CHURN_MIN_LIFETIME:=20}"
: "${TEST_CD_CHURN_MAX_LIFETIME:=120}"
: "${TEST_CD_FORCE_DELETE_PERCENT:=10}"
: "${TEST_CD_CHAOS:=}"
: "${TEST_CD_CHAOS_INTERVAL:=180}"
: "${TEST_CD_CHURN_SEED:=$(date +%s)}"

CD_NS="default"
CD_DRIVER_NS="dra-driver-nvidia-gpu"
CD_PREFIX="cdm"
CD_SELECTOR="env=batssuite,test=cd-modes"
CD_CDDS_SPEC="demo/specs/imex/computedomaindaemonset.yaml"


cd_mode_static() { [[ "${TEST_CD_MODE}" == static ]]; }
# With channel reservations, claims share one channel device per node, so
# pods of a template share its channel on a node.
cd_mode_reservations() { [[ "${TEST_CD_MODE}" != "per-cd" ]]; }
# The controller keeps a node to one ComputeDomain.
cd_mode_node_owner() { cd_mode_reservations && ! cd_mode_static; }


# cd_mode_helm_args fills the array named by $1. Every mode key is set, so a
# mode never inherits settings of the previous one.
cd_mode_helm_args() {
  local -n _cd_args=$1
  local reservations=true cdds=false scope=computeDomain isolation=domain
  case "${TEST_CD_MODE}" in
    per-cd) reservations=false ;;
    per-cd-reservations) ;;
    per-cd-channel) isolation=channel ;;
    static) isolation=channel scope=cluster cdds=true ;;
    *) echo "unknown TEST_CD_MODE: ${TEST_CD_MODE}" >&2; return 1 ;;
  esac
  _cd_args=(
    "--set" "logVerbosity=6"
    "--set" "featureGates.ComputeDomainChannelReservations=${reservations}"
    "--set" "featureGates.ComputeDomainDaemonSet=${cdds}"
    "--set" "resources.computeDomains.imex.mode=driverManaged"
    "--set" "resources.computeDomains.imex.scope=${scope}"
    "--set" "resources.computeDomains.imex.isolation=${isolation}")
}


cd_apply_cdds() {
  kubectl apply -f "${CD_CDDS_SPEC}"
  kubectl wait computedomaindaemonset default --for=condition=Ready --timeout=600s
}


# cd_get prints objects as JSON, none if their CRD is not installed.
cd_get() {
  kubectl get "$@" -o json 2>/dev/null || echo '{"items": []}'
}


# cd_mode_check_switch fails if daemons of the other kind of mode are left, as
# a node cannot run a static and a per-ComputeDomain IMEX daemon at once.
# Users must remove those before switching, and so must the previous mode.
cd_mode_check_switch() {
  local left
  if cd_mode_static; then
    left=$(kubectl get daemonsets -n "${CD_DRIVER_NS}" -l resource.nvidia.com/computeDomain -o name)
  else
    left=$(cd_get computedomaindaemonsets | jq -r '.items[] | "computedomaindaemonset/\(.metadata.name)"'
      kubectl get daemonsets -n "${CD_DRIVER_NS}" -l resource.nvidia.com/computeDomainDaemonSet -o name)
  fi
  if [[ -n "${left}" ]]; then
    echo "left over before switching to mode ${TEST_CD_MODE}: $(paste -sd' ' - <<< "${left}")" >&2
    return 1
  fi
}


# cd_mode_install installs the chart in TEST_CD_MODE and exports the nodes
# under test.
cd_mode_install() {
  local _iargs=()
  cd_mode_helm_args _iargs
  cd_mode_check_switch
  log "installing the driver in mode ${TEST_CD_MODE}"
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
  if cd_mode_static; then
    cd_apply_cdds
  fi
  cd_detect_nodes
}


# cd_mode_finish removes what the mode deployed while it is still installed,
# as users must before switching modes: workloads with their per-ComputeDomain
# daemons, then the ComputeDomainDaemonSet with its static daemons.
cd_mode_finish() {
  cd_delete_all_workloads
  cd_wait_no_leaks
  if ! cd_mode_static; then return 0; fi
  kubectl delete computedomaindaemonsets --all --ignore-not-found --timeout=600s
  kubectl wait --for=delete daemonsets -n "${CD_DRIVER_NS}" \
    -l resource.nvidia.com/computeDomainDaemonSet --timeout=120s
}


# cd_detect_nodes exports CD_NODES, the nodes publishing IMEX channels, and
# CD_CLIQUE_NODES, the nodes of the largest clique.
cd_detect_nodes() {
  local names lines
  names=$(kubectl get resourceslices --field-selector spec.driver=compute-domain.nvidia.com -o json | \
    jq -c '[.items[].spec.nodeName // empty] | unique')
  lines=$(kubectl get nodes -o json | jq -r --argjson want "${names}" '
    .items[] | select(.metadata.name as $n | $want | any(. == $n))
      | "\(.metadata.labels["nvidia.com/gpu.clique"] // "-") \(.metadata.name)"' | sort)
  CD_NODES=$(awk '{print $2}' <<< "${lines}" | paste -sd' ')
  CD_CLIQUE_NODES=$(awk '{n[$1] = n[$1] " " $2; c[$1]++}
    END {for (k in c) if (c[k] > best) {best = c[k]; out = n[k]}; print out}' <<< "${lines}" | xargs)
  CD_HAS_CLIQUES=false
  awk '$1 != "-"' <<< "${lines}" | grep -q . && CD_HAS_CLIQUES=true
  export CD_NODES CD_CLIQUE_NODES CD_HAS_CLIQUES
  log "nodes: ${CD_NODES}, largest clique: ${CD_CLIQUE_NODES}"
  if [[ -z "${CD_NODES}" ]]; then
    echo "no node publishes IMEX channels" >&2
    return 1
  fi
}


cd_clique_size() { wc -w <<< "${CD_CLIQUE_NODES}" | tr -d ' '; }
cd_first_node() { awk '{print $1}' <<< "${CD_NODES}"; }
cd_name() { echo "${CD_PREFIX}-$1-${RANDOM}"; }


# cd_affinity_yaml prints the pod affinity of a placement: any, node (pinned
# to $3), spread (one pod per node of a clique) or colocate (one node).
cd_affinity_yaml() {
  local name="$1" placement="$2" node="$3" nodes n
  nodes="${CD_NODES}"
  [[ "${placement}" == node ]] && nodes="${node}"
  # A metadata.name selector takes one value, so each node gets its own term.
  cat <<EOF
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
EOF
  for n in ${nodes}; do
    echo "        - matchFields: [{key: metadata.name, operator: In, values: [${n}]}]"
  done
  case "${placement}" in
    spread)
      cat <<EOF
    podAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
      - {labelSelector: {matchLabels: {cd-workload: ${name}}}, topologyKey: kubernetes.io/hostname}
EOF
      if [[ "${CD_HAS_CLIQUES}" == true ]]; then
        cat <<EOF
    podAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
      - {labelSelector: {matchLabels: {cd-workload: ${name}}}, topologyKey: nvidia.com/gpu.clique}
EOF
      fi
      ;;
    colocate)
      cat <<EOF
    podAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
      - {labelSelector: {matchLabels: {cd-workload: ${name}}}, topologyKey: kubernetes.io/hostname}
EOF
      ;;
  esac
}


# cd_template_name prints the name of template $2 of ComputeDomain $1 with $3
# templates.
cd_template_name() {
  if (($3 > 1)); then echo "$1-channel-$2"; else echo "$1-channel"; fi
}


# cd_workload_yaml prints a ComputeDomain with $2 pods using $3 channel
# templates round robin, listed in spec.channels if more than one. See
# cd_affinity_yaml for $4 and $5.
cd_workload_yaml() {
  local name="$1" pods="$2" templates="$3" placement="$4" node="${5:-}"
  local num_nodes=1 i t rct
  [[ "${placement}" == spread ]] && num_nodes="${pods}"
  cat <<EOF
---
apiVersion: resource.nvidia.com/v1beta1
kind: ComputeDomain
metadata:
  name: ${name}
  namespace: ${CD_NS}
  labels: {env: batssuite, test: cd-modes, cd-workload: ${name}}
spec:
  numNodes: ${num_nodes}
EOF
  if ((templates > 1)); then
    echo "  channels:"
    for ((t = 0; t < templates; t++)); do
      echo "  - resourceClaimTemplate: {name: $(cd_template_name "${name}" "${t}" "${templates}")}"
    done
  else
    echo "  channel:"
    echo "    resourceClaimTemplate: {name: $(cd_template_name "${name}" 0 1)}"
  fi
  for ((i = 0; i < pods; i++)); do
    t=$((i % templates))
    rct=$(cd_template_name "${name}" "${t}" "${templates}")
    cat <<EOF
---
apiVersion: v1
kind: Pod
metadata:
  name: ${name}-${i}
  namespace: ${CD_NS}
  labels: {env: batssuite, test: cd-modes, cd-workload: ${name}, cd-template: ${rct}}
spec:
  terminationGracePeriodSeconds: 5
  restartPolicy: Never
  tolerations: [{key: nvidia.com/gpu, operator: Exists}]
  containers:
  - name: ctr
    image: ${TEST_CD_IMAGE}
    command: ["bash", "-c"]
    args: ["echo IMEX_CHANNELS: \$(ls /dev/nvidia-caps-imex-channels 2>/dev/null); trap 'exit 0' TERM; sleep 9999 & wait"]
    resources:
      claims: [{name: channel}]
  resourceClaims:
  - {name: channel, resourceClaimTemplateName: ${rct}}
  affinity:
$(cd_affinity_yaml "${name}" "${placement}" "${node}")
EOF
  done
}


cd_apply_workload() {
  cd_workload_yaml "$@" | kubectl apply -f -
}


cd_wait_workload() {
  kubectl wait -n "${CD_NS}" --for=condition=Ready pod -l "cd-workload=$1" --timeout="${2:-${TEST_CD_READY_TIMEOUT}}s"
}


cd_workload_summary() {
  kubectl get pods -n "${CD_NS}" -l "cd-workload=$1" -o wide --no-headers 2>/dev/null | \
    awk '{print $1 "=" $3 "@" $7}' | paste -sd' '
}


# cd_channel_device_attr prints string attribute $2 of the channel device of
# node $1, empty if unset.
cd_channel_device_attr() {
  kubectl get resourceslices --field-selector "spec.driver=compute-domain.nvidia.com,spec.nodeName=$1" -o json | \
    jq -r --arg a "$2" '[.items[].spec.devices[]? | select(.name == "channel") | .attributes[$a].string // empty] | first // empty'
}


# cd_channel_device_owner prints the ComputeDomain the channel device of node
# $1 is tainted with, empty if none.
cd_channel_device_owner() {
  kubectl get resourceslices --field-selector "spec.driver=compute-domain.nvidia.com,spec.nodeName=$1" -o json | \
    jq -r '[.items[].spec.devices[]? | select(.name == "channel") | .taints[]?
      | select(.key == "compute-domain.nvidia.com/computeDomain" and .effect == "NoSchedule") | .value] | first // empty'
}


# cd_pod_channel prints the one channel a pod sees and fails otherwise.
cd_pod_channel() {
  local line
  line=$(kubectl logs -n "${CD_NS}" "$1" | grep -m1 '^IMEX_CHANNELS:')
  echo "$1: ${line}" >&2
  [[ "${line}" =~ ^IMEX_CHANNELS:\ channel([0-9]+)$ ]] || return 1
  echo "${BASH_REMATCH[1]}"
}


# cd_delete_workload deletes pods and ComputeDomain without waiting. $2 force
# deletes the pods, $3 deletes the ComputeDomain first.
cd_delete_workload() {
  local name="$1" force="${2:-false}" cd_first="${3:-false}"
  local pod_args=("--wait=false" "--ignore-not-found")
  [[ "${force}" == true ]] && pod_args+=("--force" "--grace-period=0")
  if [[ "${cd_first}" == true ]]; then
    kubectl delete computedomain -n "${CD_NS}" "${name}" --wait=false --ignore-not-found
  fi
  kubectl delete pods -n "${CD_NS}" -l "cd-workload=${name}" "${pod_args[@]}"
  kubectl delete computedomain -n "${CD_NS}" "${name}" --wait=false --ignore-not-found
}


cd_workload_objects() {
  {
    kubectl get pods -n "${CD_NS}" -l "cd-workload=$1" -o name
    kubectl get computedomain -n "${CD_NS}" "$1" -o name --ignore-not-found
    kubectl get resourceclaims,resourceclaimtemplates -n "${CD_NS}" -o name | grep "/$1-" || true
  } 2>/dev/null
}


cd_wait_workload_gone() {
  local deadline=$((SECONDS + ${2:-${TEST_CD_CLEANUP_TIMEOUT}}))
  while [[ -n "$(cd_workload_objects "$1")" ]]; do
    if ((SECONDS >= deadline)); then
      echo "left after cleanup: $(cd_workload_objects "$1" | paste -sd' ')" >&2
      return 1
    fi
    sleep 2
  done
}


cd_delete_all_workloads() {
  kubectl delete pods -n "${CD_NS}" -l "${CD_SELECTOR}" --wait=false --ignore-not-found
  kubectl delete computedomains -n "${CD_NS}" -l "${CD_SELECTOR}" --wait=false --ignore-not-found
}


# cd_static_daemons prints "node uid" of each running static IMEX daemon.
cd_static_daemons() {
  kubectl get pods -n "${CD_DRIVER_NS}" -l resource.nvidia.com/computeDomainDaemonSet -o json | \
    jq -r '.items[] | select(.metadata.deletionTimestamp == null and .spec.nodeName != null)
      | "\(.spec.nodeName) \(.metadata.uid)"' | sort
}


# cd_check_invariants prints one "kind key message" line per violation. Set
# CD_DAEMON_BASELINE to a cd_static_daemons output to flag daemon restarts.
cd_check_invariants() {
  local cache="${CD_CHANNEL_CACHE:-${BATS_FILE_TMPDIR}/channels}"
  local pods claims cds nodes chans uid name line
  mkdir -p "${cache}"
  # Nodes go first: a label removed after its pods or ComputeDomain were
  # read would otherwise look like a label missing under a running pod.
  nodes=$(kubectl get nodes -l resource.nvidia.com/computeDomain -o json)
  pods=$(kubectl get pods -n "${CD_NS}" -l "${CD_SELECTOR}" -o json)
  claims=$(kubectl get resourceclaims -n "${CD_NS}" -o json)
  cds=$(kubectl get computedomains -A -o json)

  # Channels of a running pod do not change, so read each pod log once.
  while read -r uid name; do
    [[ -z "${uid}" || -s "${cache}/${uid}" ]] && continue
    line=$(kubectl logs -n "${CD_NS}" "${name}" 2>/dev/null | grep -m1 '^IMEX_CHANNELS:') || continue
    { grep -o 'channel[0-9]\+' <<< "${line}" || true; } | sed 's/channel//' | jq -s -c . > "${cache}/${uid}"
  done < <(jq -r '.items[] | select(.metadata.deletionTimestamp == null and .status.phase == "Running")
    | "\(.metadata.uid) \(.metadata.name)"' <<< "${pods}")
  chans=$(for f in "${cache}"/*; do
    [[ -s "${f}" ]] && printf '{"%s": %s}\n' "$(basename "${f}")" "$(cat "${f}")"
  done | jq -s 'add // {}')

  jq -r -n \
    --argjson pods "${pods}" --argjson claims "${claims}" --argjson cds "${cds}" --argjson nodes "${nodes}" --argjson chans "${chans}" \
    --arg ns "${CD_NS}" --argjson stuck "${TEST_CD_STUCK_TIMEOUT}" \
    --argjson reservations "$(cd_mode_reservations && echo true || echo false)" \
    --argjson static "$(cd_mode_static && echo true || echo false)" '
    def age: now - (sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601);
    ([$cds.items[] | select(.metadata.namespace == $ns) | {key: .metadata.name, value: .}] | from_entries) as $testcds
    # The channel the controller assigned to the claim of each pod.
    | ([$claims.items[] | . as $c | ($c.status.devices // [])[] | select(.device == "channel" and .data != null)
        | .data as $d | ($c.status.reservedFor // [])[] | {key: .uid, value: $d}] | from_entries) as $alloc
    | [$pods.items[] | {
        uid: .metadata.uid, name: .metadata.name, node: (.spec.nodeName // ""),
        workload: .metadata.labels["cd-workload"],
        template: .metadata.labels["cd-template"],
        deleting: .metadata.deletionTimestamp, phase: .status.phase, created: .metadata.creationTimestamp,
        running: (.metadata.deletionTimestamp == null and .status.phase == "Running"),
        channels: $chans[.metadata.uid], alloc: $alloc[.metadata.uid]}] as $all
    | ($all | map(select(.running and .channels != null))) as $run
    | ($cds.items[] | select(.metadata.deletionTimestamp != null and (.metadata.deletionTimestamp | age) > $stuck)
        | "cd-stuck-deleting \(.metadata.namespace)/\(.metadata.name) deleting since \(.metadata.deletionTimestamp), finalizers \(.metadata.finalizers)"),
      ($all[] | select(.deleting != null and (.deleting | age) > $stuck)
        | "pod-stuck-terminating \(.uid) \(.name) terminating since \(.deleting)"),
      ($all[] | select(.phase == "Failed") | "pod-failed \(.uid) \(.name) on \(.node) failed"),
      ($all[] | select(.deleting == null and .phase == "Pending" and $testcds[.workload] != null and (.created | age) > $stuck)
        | "pod-stuck-pending \(.uid) \(.name) pending since \(.created)"),
      ($run[] | select(.channels | length != 1)
        | "channel-count \(.uid) \(.name) on \(.node) sees channels \(.channels), want one"),
      ($run[] | select(.channels | length == 1) | . as $p
        | (if $reservations then $p.alloc.channel // null else 0 end) as $want
        | select($want != null and $want != $p.channels[0])
        | "channel-mismatch \(.uid) \(.name) on \(.node) got channel \(.channels[0]), want \($want)"),
      ($run | group_by(.node)[] | (map(.workload) | unique) as $w
        | select($static | not) | select($w | length > 1)
        | "node-shared \(.[0].node)/\($w | join(",")) node \(.[0].node) runs ComputeDomains \($w | join(","))"),
      ($run | map(select(.channels | length == 1)) | group_by([.node, .channels[0]])[]
        | (map(.workload) | unique) as $w | select($w | length > 1)
        | "channel-shared \(.[0].node)/\(.[0].channels[0])/\($w | join(",")) channel \(.[0].channels[0]) on \(.[0].node) is used by \($w | join(","))"),
      ($run | map(select(.channels | length == 1)) | group_by([.node, .channels[0]])[]
        | (map(.template) | unique) as $t | select($t | length > 1)
        | "channel-shared-by-templates \(.[0].node)/\(.[0].channels[0]) channel \(.[0].channels[0]) on \(.[0].node) is used by templates \($t | join(","))"),
      (if $reservations then
         [$run[] | select(.alloc != null) | {k: "\(.alloc.imexDomain)/\(.alloc.channel)", t: "\(.workload)/\(.template)"}]
         | group_by(.k)[] | (map(.t) | unique) as $t | select($t | length > 1)
         | "clique-channel-overlap \(.[0].k)/\($t | join(",")) channel \(.[0].k) is used by \($t | join(","))"
       else empty end),
      (if $static then empty else
         ([$nodes.items[] | {key: .metadata.name, value: .metadata.labels["resource.nvidia.com/computeDomain"]}] | from_entries) as $owners
         # Deleting a ComputeDomain removes its node labels under running pods.
         | $run[] | . as $p | select($testcds[$p.workload].metadata.deletionTimestamp == null)
         | ($testcds[$p.workload].metadata.uid // null) as $uid
         | select($uid != null and ($owners[$p.node] // "") != $uid)
         | "node-label-mismatch \($p.node)/\($uid) node \($p.node) runs \($p.name) but its IMEX daemon is for \"\($owners[$p.node] // "")\""
       end)'

  if [[ -n "${CD_DAEMON_BASELINE:-}" && -s "${CD_DAEMON_BASELINE}" ]]; then
    local now node
    now=$(cd_static_daemons)
    while read -r node uid; do
      grep -qx "${node} ${uid}" <<< "${now}" || \
        echo "static-daemon-restarted ${node}/${uid} static IMEX daemon ${uid} of node ${node} is gone"
    done < "${CD_DAEMON_BASELINE}"
  fi
}


cd_assert_invariants() {
  run cd_check_invariants
  assert_success
  assert_output ""
}


# cd_find_leaks prints objects left for deleted ComputeDomains, and test
# objects left in the test namespace.
cd_find_leaks() {
  local live
  live=$({ cd_get computedomains -A; cd_get computedomaindaemonsets; } | \
    jq -s -c '[.[].items[].metadata.uid]')
  { kubectl get pods,resourceclaims,resourceclaimtemplates -n "${CD_NS}" -o name
    cd_get computedomains -n "${CD_NS}" | jq -r '.items[] | "computedomain/\(.metadata.name)"'
  } | grep "/${CD_PREFIX}-" || true
  kubectl get daemonsets -n "${CD_DRIVER_NS}" -l resource.nvidia.com/computeDomain -o json | \
    jq -r --argjson live "${live}" '.items[]
      | select(.metadata.labels["resource.nvidia.com/computeDomain"] as $u | $live | any(. == $u) | not)
      | "daemonset/\(.metadata.name)"'
  kubectl get daemonsets -n "${CD_DRIVER_NS}" -l resource.nvidia.com/computeDomainDaemonSet -o json | \
    jq -r --argjson live "${live}" '.items[]
      | select(.metadata.labels["resource.nvidia.com/computeDomainDaemonSet"] as $u | $live | any(. == $u) | not)
      | "daemonset/\(.metadata.name)"'
  cd_get computedomaincliques -A | \
    jq -r --argjson live "${live}" '.items[]
      | select(.metadata.name | split(".")[0] as $u | $live | any(. == $u) | not)
      | "computedomainclique/\(.metadata.name)"'
  kubectl get nodes -l resource.nvidia.com/computeDomain -o json | \
    jq -r --argjson live "${live}" '.items[]
      | .metadata.labels["resource.nvidia.com/computeDomain"] as $u
      | select($live | any(. == $u) | not) | "node/\(.metadata.name) label \($u)"'
}


cd_wait_no_leaks() {
  local deadline=$((SECONDS + ${1:-${TEST_CD_CLEANUP_TIMEOUT}}))
  local leaks
  while leaks=$(cd_find_leaks) && [[ -n "${leaks}" ]]; do
    if ((SECONDS >= deadline)); then
      echo "${leaks}"
      return 1
    fi
    sleep 5
  done
}


# cd_wait_component_ready waits for the pods of a driver component, on node
# $2 if given, to be ready and not terminating.
cd_wait_component_ready() {
  local selector="dra-driver-nvidia-gpu-component=$1" node="${2:-}"
  local args=("-n" "${CD_DRIVER_NS}" "-l" "${selector}")
  [[ -n "${node}" ]] && args+=("--field-selector" "spec.nodeName=${node}")
  local deadline=$((SECONDS + 300))
  until kubectl get pods "${args[@]}" -o json | jq -e '.items | length > 0 and all(
      .metadata.deletionTimestamp == null and
      any(.status.conditions[]?; .type == "Ready" and .status == "True"))' > /dev/null; do
    ((SECONDS < deadline)) || return 1
    sleep 2
  done
}


cd_restart_controller() {
  kubectl delete pods -n "${CD_DRIVER_NS}" -l dra-driver-nvidia-gpu-component=controller
  cd_wait_component_ready controller
}


cd_restart_plugin() {
  kubectl delete pods -n "${CD_DRIVER_NS}" -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    --field-selector "spec.nodeName=$1"
  cd_wait_component_ready kubelet-plugin "$1"
}


cd_k8s_minor() {
  kubectl version -o json | jq -r '.serverVersion.minor' | tr -dc '0-9'
}


# cd_churn_workers prints the number of concurrent churn workloads. Static
# daemons share nodes, so they get more.
cd_churn_workers() {
  local nodes
  nodes=$(wc -w <<< "${CD_NODES}" | tr -d ' ')
  if [[ -n "${TEST_CD_CHURN_WORKERS}" ]]; then
    echo "${TEST_CD_CHURN_WORKERS}"
  elif cd_mode_static; then
    echo $((2 * nodes))
  else
    echo "${nodes}"
  fi
}


# cd_churn_shape sets pods, templates and placement of a random workload.
cd_churn_shape() {
  local sizes=() size clique
  IFS=',' read -r -a sizes <<< "${TEST_CD_CHURN_NODES_PER_CD}"
  size="${sizes[RANDOM % ${#sizes[@]}]}"
  clique=$(cd_clique_size)
  ((size > clique)) && size="${clique}"
  pods="${size}" templates=1 placement=any
  ((size > 1)) && placement=spread
  if cd_mode_reservations && ((RANDOM % 4 == 0)); then
    templates=2
    if ((size == 1)); then
      pods=2 placement=colocate
    fi
  elif cd_mode_reservations && ((size == 1 && RANDOM % 3 == 0)); then
    # Pods of a template share its channel on a node.
    pods=2 placement=colocate
  fi
}


# cd_churn_worker creates, runs and deletes random workloads until epoch $2,
# and appends one line per workload to $3.
cd_churn_worker() {
  local id="$1" end="$2" out="$3"
  local n=0 name pods templates placement t0 force cd_first
  set +e
  RANDOM=$((TEST_CD_CHURN_SEED + id))
  while (($(date +%s) < end)); do
    n=$((n + 1))
    name="${CD_PREFIX}-c${id}-${n}"
    cd_churn_shape
    t0=$(date +%s)
    if ! cd_apply_workload "${name}" "${pods}" "${templates}" "${placement}" > /dev/null; then
      echo "error ${name} apply failed" >> "${out}"
      sleep 5
      continue
    fi
    if ((RANDOM % 100 < 10)); then
      sleep $((RANDOM % 5))
      echo "aborted ${name}" >> "${out}"
    elif cd_wait_workload "${name}" > /dev/null 2>&1; then
      echo "ready ${name} $(($(date +%s) - t0)) pods=${pods} templates=${templates} placement=${placement}" >> "${out}"
      sleep $((TEST_CD_CHURN_MIN_LIFETIME + RANDOM % (TEST_CD_CHURN_MAX_LIFETIME - TEST_CD_CHURN_MIN_LIFETIME + 1)))
    else
      echo "error ${name} not running after ${TEST_CD_READY_TIMEOUT}s: $(cd_workload_summary "${name}")" >> "${out}"
    fi
    force=false cd_first=false
    ((RANDOM % 100 < TEST_CD_FORCE_DELETE_PERCENT)) && force=true
    ((RANDOM % 2)) && cd_first=true
    t0=$(date +%s)
    cd_delete_workload "${name}" "${force}" "${cd_first}" > /dev/null 2>&1
    if cd_wait_workload_gone "${name}" 2> /dev/null; then
      echo "cleanup ${name} $(($(date +%s) - t0)) force=${force} cd_first=${cd_first}" >> "${out}"
    else
      echo "error ${name} left after ${TEST_CD_CLEANUP_TIMEOUT}s: $(cd_workload_objects "${name}" | paste -sd' ')" >> "${out}"
    fi
  done
}


# cd_chaos restarts the controller or the kubelet plugin of a random node.
cd_chaos() {
  local targets=() target nodes=()
  IFS=',' read -r -a targets <<< "${TEST_CD_CHAOS}"
  target="${targets[RANDOM % ${#targets[@]}]}"
  case "${target}" in
    controller)
      echo "chaos $(date -u +%H:%M:%S) restart controller"
      cd_restart_controller
      ;;
    plugin)
      read -r -a nodes <<< "${CD_NODES}"
      local node="${nodes[RANDOM % ${#nodes[@]}]}"
      echo "chaos $(date -u +%H:%M:%S) restart kubelet plugin on ${node}"
      cd_restart_plugin "${node}"
      ;;
    *)
      echo "error unknown chaos target ${target}"
      return 1
      ;;
  esac
}


# cd_percentiles prints count, p50, p95 and max of the numbers on stdin.
cd_percentiles() {
  sort -n | awk '{a[NR] = $1}
    END {
      if (NR == 0) {print "n=0"; exit}
      p50 = int(NR * 0.50) + 1; if (p50 > NR) p50 = NR
      p95 = int(NR * 0.95) + 1; if (p95 > NR) p95 = NR
      printf "n=%d p50=%ss p95=%ss max=%ss\n", NR, a[p50], a[p95], a[NR]
    }'
}
