# shellcheck disable=SC2148
# shellcheck disable=SC2329

# Seamless (zero-downtime) rolling updates of the kubelet plugin DaemonSet:
# SeamlessUpgrades feature gate + maxSurge=1/maxUnavailable=0. Verifies that
#
#  - each plugin instance uses pod-UID-suffixed registration and DRA sockets,
#  - during a rollout the old and the new instance run (and are registered)
#    concurrently and a claim can be prepared in that window,
#  - the two instances do not fight over the ResourceSlice (no writes during
#    the overlap window),
#  - the old instance's sockets are removed when it exits,
#  - a rollout to a bad image leaves the old instance serving, and rolling
#    back does not disturb it.
#
# The socket assertions look at the node's kubelet directories through the
# plugin container (which bind-mounts them). This test is device-independent
# and runs against mock NVML as well as real GPUs.

setup_file() {
  load 'helpers.sh'
  local _iargs=(
    "--set" "logVerbosity=6"
    "--set" "featureGates.SeamlessUpgrades=true"
    "--set" "kubeletPlugin.updateStrategy.rollingUpdate.maxSurge=1"
    "--set" "kubeletPlugin.updateStrategy.rollingUpdate.maxUnavailable=0"
  )
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
}


teardown_file() {
  load 'helpers.sh'
  # Restore the suite's default installation (gate off, default strategy) so
  # that subsequent test files see the state they expect.
  local _iargs=("--set" "logVerbosity=6")
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
  kubectl get pods -n dra-driver-nvidia-gpu -o wide
  show_kubelet_plugin_error_logs
  show_gpu_plugin_log_tails
  echo -e "FAILURE HOOK END\n\n"
}


# Name of the single Running, non-terminating gpu kubelet plugin pod on the
# node that the workload pod (arg 1) runs on -- or, w/o arg, on any node.
_running_plugin_pod() {
  kubectl get pods -n dra-driver-nvidia-gpu \
    -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.metadata.name}:{.metadata.deletionTimestamp}{"\n"}{end}' \
    | grep ':$' | cut -d: -f1 | head -n1
}

# List socket files in the kubelet registrar dir and the driver plugin dir, as
# seen from within a plugin pod (arg 1).
_list_sockets_via() {
  kubectl exec -n dra-driver-nvidia-gpu "$1" -c gpus -- \
    bash -c 'ls /var/lib/kubelet/plugins_registry/ /var/lib/kubelet/plugins/gpu.nvidia.com/ | grep sock' 2>/dev/null
}

_rs_rv() {
  kubectl get resourceslices -o jsonpath='{range .items[?(@.spec.driver=="gpu.nvidia.com")]}{.metadata.resourceVersion}{" "}{end}'
}


# bats test_tags=fastfeedback
@test "GPUs: seamless upgrades: per-pod-UID sockets" {
  local _kpod _uid
  _kpod=$(_running_plugin_pod)
  _uid=$(kubectl get pod -n dra-driver-nvidia-gpu "${_kpod}" -o jsonpath='{.metadata.uid}')
  log "plugin pod: ${_kpod} uid: ${_uid}"

  run _list_sockets_via "${_kpod}"
  assert_output --partial "gpu.nvidia.com-${_uid}-reg.sock"
  assert_output --partial "dra-${_uid}.sock"
  # The plain (non-suffixed) names must not be in use.
  refute_output --regexp "^gpu.nvidia.com-reg.sock$"
  refute_output --regexp "^dra.sock$"

  # The plugin's own log confirms the suffixed endpoints (also for the
  # healthcheck client, which must dial the same suffixed sockets).
  run kubectl logs -n dra-driver-nvidia-gpu "${_kpod}" -c gpus
  assert_output --partial "endpoint=\"/var/lib/kubelet/plugins/gpu.nvidia.com/dra-${_uid}.sock\""
  assert_output --partial "connecting to DRA socket path=unix:///var/lib/kubelet/plugins/gpu.nvidia.com/dra-${_uid}.sock"
}


# bats test_tags=fastfeedback
@test "GPUs: seamless upgrades: surge rollout, prepare during overlap, no ResourceSlice churn, old sockets removed" {
  local _old _olduid _new _newuid _rv0 _rv1

  # Baseline workload, kept running across the rollout.
  kubectl apply -f tests/bats/specs/gpu-simple-full.yaml
  kubectl wait --for=condition=READY pods pod-full-gpu --timeout=20s

  _old=$(_running_plugin_pod)
  _olduid=$(kubectl get pod -n dra-driver-nvidia-gpu "${_old}" -o jsonpath='{.metadata.uid}')
  log "old plugin pod: ${_old} (${_olduid})"

  # Widen the overlap window: make the old instance linger 30 s after SIGTERM
  # is due, so that we can reliably act while both instances are alive. This
  # DaemonSet template change itself is a (short-overlap) surge rollout.
  # (Strategic merge patch: target the gpus container by name -- its index in
  # the pod spec depends on whether compute domains are enabled.)
  kubectl patch ds -n dra-driver-nvidia-gpu dra-driver-nvidia-gpu-kubelet-plugin --type=strategic -p='{
    "spec":{"template":{"spec":{"terminationGracePeriodSeconds":90,
      "containers":[{"name":"gpus","lifecycle":{"preStop":{"exec":{"command":["sleep","30"]}}}}]}}}}'
  kubectl rollout status ds -n dra-driver-nvidia-gpu dra-driver-nvidia-gpu-kubelet-plugin --timeout=180s
  _old=$(_running_plugin_pod)
  _olduid=$(kubectl get pod -n dra-driver-nvidia-gpu "${_old}" -o jsonpath='{.metadata.uid}')
  log "old plugin pod (with preStop): ${_old} (${_olduid})"
  sleep 3
  _rv0=$(_rs_rv)
  log "ResourceSlice resourceVersion before rollout: ${_rv0}"

  # Trigger the rollout under test (pod template change).
  kubectl set env ds -n dra-driver-nvidia-gpu dra-driver-nvidia-gpu-kubelet-plugin -c gpus SEAMLESS_UPGRADES_TEST=1

  # Wait for the overlap window: new pod Ready while old pod is Terminating.
  local _i
  for _i in $(seq 1 90); do
    _new=$(kubectl get pods -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=kubelet-plugin \
      -o jsonpath='{range .items[?(@.status.containerStatuses[0].ready==true)]}{.metadata.name}:{.metadata.deletionTimestamp}{"\n"}{end}' \
      | grep ':$' | cut -d: -f1 | grep -v "^${_old}$" | head -n1 || true)
    if [ -n "${_new}" ] && [ -n "$(kubectl get pod -n dra-driver-nvidia-gpu "${_old}" -o jsonpath='{.metadata.deletionTimestamp}')" ]; then
      break
    fi
    sleep 1
  done
  [ -n "${_new}" ]
  _newuid=$(kubectl get pod -n dra-driver-nvidia-gpu "${_new}" -o jsonpath='{.metadata.uid}')
  log "overlap window: old=${_old} (Terminating) new=${_new} (${_newuid}, Ready)"

  # Both instances' sockets are present.
  run _list_sockets_via "${_new}"
  assert_output --partial "gpu.nvidia.com-${_olduid}-reg.sock"
  assert_output --partial "gpu.nvidia.com-${_newuid}-reg.sock"
  assert_output --partial "dra-${_olduid}.sock"
  assert_output --partial "dra-${_newuid}.sock"

  # A claim can be prepared while both instances are alive.
  sed 's/pod-full-gpu/pod-full-gpu-overlap/; s/rct-single-gpu-full/rct-single-gpu-full-overlap/' \
    tests/bats/specs/gpu-simple-full.yaml | kubectl apply -f -
  kubectl wait --for=condition=READY pods pod-full-gpu-overlap --timeout=30s
  # And the pre-rollout claim can be unprepared.
  kubectl delete pod pod-full-gpu --wait=true --timeout=60s

  # Old instance goes away; its sockets must be gone, the new one's remain.
  kubectl wait --for=delete pod -n dra-driver-nvidia-gpu "${_old}" --timeout=120s
  kubectl rollout status ds -n dra-driver-nvidia-gpu dra-driver-nvidia-gpu-kubelet-plugin --timeout=60s
  run _list_sockets_via "${_new}"
  refute_output --partial "${_olduid}"
  assert_output --partial "gpu.nvidia.com-${_newuid}-reg.sock"
  assert_output --partial "dra-${_newuid}.sock"

  # No ResourceSlice churn: the two publishers must have converged on identical
  # content without rewriting the slice (a rewrite would bump resourceVersion).
  _rv1=$(_rs_rv)
  log "ResourceSlice resourceVersion after rollout: ${_rv1}"
  [ "${_rv0}" = "${_rv1}" ]

  # Cleanup.
  kubectl delete pod pod-full-gpu-overlap --wait=true --timeout=60s
  kubectl delete resourceclaimtemplates rct-single-gpu-full rct-single-gpu-full-overlap --ignore-not-found
}


# bats test_tags=fastfeedback
@test "GPUs: seamless upgrades: bad image keeps old instance serving, rollback is non-disruptive" {
  local _good _rev

  _good=$(_running_plugin_pod)
  log "good plugin pod: ${_good}"
  _rev=$(helm history "${TEST_HELM_RELEASE_NAME}" -n dra-driver-nvidia-gpu -o json | jq -r ".[-1].revision")

  # Roll out an image that cannot be pulled. With maxUnavailable=0 the old
  # instance must keep running; the surge pod gets stuck.
  timeout -v 120 helm upgrade "${TEST_HELM_RELEASE_NAME}" "${TEST_CHART_REPO}" \
    --version="${TEST_CHART_VERSION}" --namespace dra-driver-nvidia-gpu --reuse-values \
    --set image.tag=does-not-exist-seamless-upgrades-test
  sleep 20
  kubectl get pods -n dra-driver-nvidia-gpu -o wide
  run kubectl get pod -n dra-driver-nvidia-gpu "${_good}" -o jsonpath='{.status.phase}:{.metadata.deletionTimestamp}'
  assert_output "Running:"
  run kubectl get ds -n dra-driver-nvidia-gpu dra-driver-nvidia-gpu-kubelet-plugin -o jsonpath='{.status.numberReady}'
  assert_output "1"

  # Workloads still get prepared by the surviving instance.
  kubectl apply -f tests/bats/specs/gpu-simple-full.yaml
  kubectl wait --for=condition=READY pods pod-full-gpu --timeout=30s

  # Rollback removes the stuck pod and does not restart the good one.
  helm rollback "${TEST_HELM_RELEASE_NAME}" "${_rev}" -n dra-driver-nvidia-gpu --wait --timeout=120s
  kubectl rollout status ds -n dra-driver-nvidia-gpu dra-driver-nvidia-gpu-kubelet-plugin --timeout=180s
  run kubectl get pod -n dra-driver-nvidia-gpu "${_good}" -o jsonpath='{.status.phase}:{.metadata.deletionTimestamp}'
  assert_output "Running:"
  run kubectl get pods -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=kubelet-plugin --field-selector=status.phase=Running -o name
  assert_line --index 0 "pod/${_good}"
  [ "$(echo "${output}" | wc -l)" -eq 1 ]

  kubectl delete pod pod-full-gpu --wait=true --timeout=60s
  kubectl delete resourceclaimtemplates rct-single-gpu-full --ignore-not-found
}
