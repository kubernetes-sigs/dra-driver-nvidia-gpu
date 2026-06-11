#!/bin/bash
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

# Use a name that upon cluster inspection reveals that this
# Helm chart release was installed/managed by this test suite.
export TEST_HELM_RELEASE_NAME="dra-driver-nvidia-gpu-batssuite"


# Extend PATH for the `nvmm` utility. NVMM_PATH allows overriding the
# directory containing `nvmm` — set it to `tests/bats/lib/lambda` on
# Lambda CI (no GPU Operator) so the no-op stub is used instead.
SELF_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
export PATH="${NVMM_PATH:-${SELF_DIR}/lib}:${PATH}"


_common_setup() {
  load '/bats-libraries/bats-support/load.bash'
  load '/bats-libraries/bats-assert/load.bash'
  load '/bats-libraries/bats-file/load.bash'
}


# A helper arg for `iupgrade_wait` w/o additional install args.
export NOARGS=()

# Install or upgrade, and wait for pods to be READY.
# 1st arg: helm chart repo
# 2nd arg: helm chart version
# 3rd arg: array with additional args (provide `NOARGS` if none)
iupgrade_wait() {
  # E.g. `nvidia/dra-driver-nvidia-gpu` or
  # `oci://gcr.io/k8s-staging-nvidia/charts/dra-driver-nvidia-gpu`
  local REPO="$1"

  # E.g. `25.3.1` or `25.8.0-dev-f2eaddd6-chart`
  local VERSION="$2"

  # Expect array as third argument.
  local -n ADDITIONAL_INSTALL_ARGS=$3

  # When running against mock NVML, pass altProcDevices so the compute-domain
  # plugin can find nvidia-caps-imex-channels in the mock /proc/devices file.
  local _mock_args=()
  if [ -n "${TEST_ALT_PROC_DEVICES:-}" ]; then
    _mock_args+=("--set" "altProcDevices=${TEST_ALT_PROC_DEVICES}")
  fi

  log "iupgrade_wait: start"
  timeout -v 120 helm upgrade --install "${TEST_HELM_RELEASE_NAME}" \
    "${REPO}" \
    --version="${VERSION}" \
    --wait \
    --timeout=1m5s \
    --create-namespace \
    --namespace dra-driver-nvidia-gpu \
    --set gpuResourcesEnabledOverride=true \
    --set nvidiaDriverRoot="${TEST_NVIDIA_DRIVER_ROOT}" "${_mock_args[@]}" "${ADDITIONAL_INSTALL_ARGS[@]}"

  # Valueable output to have in the logs in case things went pearshaped.
  kubectl get pods -n dra-driver-nvidia-gpu

  # Some part of this waiting work is done by helm as of `--wait` with
  # `--timeout`. Note that the below in itself would not be sufficient: in case
  # of an upgrade we need to isolate the _new_ pods and not accidentally observe
  # the currently disappearing pods. Also note that despite the `--wait` above,
  # the kubelet plugins may still be in `PodInitializing` or `Init:0/1` after
  # the Helm command returned. My conclusion is that helm waits for the
  # controller to be READY, but not for the plugin pods to be READY. An old
  # plugin pod may also still be present in `Completed` state. The label
  # selector below may (rarely) pick it up, and of course that one will never
  # transition to READY. Fix that by only waiting for pods that are not in
  # Terminating/Completed state (that do not have a deletionTimestamp). That's
  # not natively supported by `kubectl wait`, hence this must be something of
  # the shape
  # `kubectl get pods ... | xargs -I{} kubectl wait --for=condition=Ready pod/{} `
  sleep 1
  kubectl wait --for=condition=READY pods -A -l dra-driver-nvidia-gpu-component=kubelet-plugin --timeout=15s

  # Again, log current state.
  kubectl get pods -n dra-driver-nvidia-gpu

  # That one should be obvious now, but make that guarantee explicit for
  # consuming tests. Skip when compute domains are disabled (no controller deployment).
  if [ "${DISABLE_COMPUTE_DOMAINS:-}" != "true" ]; then
    kubectl wait --for=condition=READY pods -A -l dra-driver-nvidia-gpu-component=controller --timeout=10s
  fi
  # maybe: check version on labels (to confirm that we set labels correctly)
  log "iupgrade_wait: done"
}


log_objects() {
  # Never fail, but show output in case a test fails, to facilitate debugging.
  # Could this be part of setup()? If setup succeeds and when a test fails:
  # does this show the output of setup? Then we could do this.
  log "LOG_OBJECTS START"
  echo "claims:"; kubectl get resourceclaims --ignore-not-found || true
  echo "CDs:   "; kubectl get computedomain --ignore-not-found || true
  echo "pods:  "; kubectl get pods -o wide --ignore-not-found || true
  kubectl get pods -o wide -n dra-driver-nvidia-gpu || true
  log "LOG_OBJECTS END"
}


# Events accumulate over time, so for certainty it's best to use a unique pod
# name. Right now, this inspects an entire line which includes REASON, MESSAGE,
# and OBJECT, so choose the needle (grepped for) precisely enough.
# Example: wait_for_pod_event pod/testpod-ls09x FailedPrepareDynamicResources 60
wait_for_pod_event() {
  # Expect this to have the pod/ prefix
  local POD_NAME="$1"
  local REASON="$2"
  local TIMEOUT="$3"

  local START=$SECONDS
  while true; do
    if kubectl events --for "${POD_NAME}" | grep -q "${REASON}"; then
      echo "Event detected: ${REASON} (for ${POD_NAME})"
      return 0
    fi
    if (( SECONDS - START > TIMEOUT )); then
      echo "Timeout (${TIMEOUT} s) waiting for '${REASON}' in events for ${POD_NAME}"
      return 1
    fi
    sleep 2
  done
}

# The number of expected GPU resource slices per node depends on the Kubernetes
# version. Wait for each node to announce at least one of them.
wait_for_all_gpu_resource_slices() {
  local timeout=$1
  local start_time=$SECONDS
  local driver_name="gpu.nvidia.com"
  local expected_count=$2
  expected_count="$(get_node_count)"

  log "Waiting up to ${timeout} s for at least ${expected_count} unique node(s) to show at least one '${driver_name}' ResourceSlice"
  while (( SECONDS - start_time < timeout )); do
    # 1. Fetch DRIVER and NODE columns for all slices.
    # 2. filter for GPU DRA driver and print the nodeName ($2)
    # 3. sort -u removes duplicate node names (since one node can have multiple GPU slices).
    # 4. wc -l counts the unique nodes.
    # 5. awk strips any extra whitespace from wc output.
    current_node_count=$(kubectl get resourceslices.resource.k8s.io -A \
      -o custom-columns=DRIVER:.spec.driver,NODE:.spec.nodeName --no-headers 2>/dev/null \
      | awk -v d="${driver_name}" '$1 == d { print $2 }' \
      | sort -u \
      | wc -l \
      | awk '{print $1}')

    if [[ "$current_node_count" -ge "$expected_count" ]]; then
      log "Success: Found ${current_node_count} unique node(s) with ResourceSlices for '${driver_name}'"
      return 0
    fi

    sleep 0.5
  done

  log "wait for resource slices: deadline reached"
  return 1
}

get_all_cd_daemon_logs_for_cd_name() {
  CD_NAME="$1"
  CD_UID=$(kubectl describe computedomains.resource.nvidia.com "${CD_NAME}" | grep UID | awk '{print $2}')
  CD_LABEL_KV="resource.nvidia.com/computeDomain=${CD_UID}"
  echo "CD daemon logs for CD: $CD_UID"
  kubectl logs \
    -n dra-driver-nvidia-gpu \
    -l "${CD_LABEL_KV}" \
    --tail=-1 \
    --prefix \
    --all-containers
}


# Compare attribute sets. Specifically, compare two sets of words where the
# reference must be provided as array, and the other set must be provided as a
# string with one word per line.
# Usage: compare_attributes "$actual_attrs" reference_array[@]
assert_attrs_equal() {
  local actual="$1"
  shift
  local reference=("$@")

  local expected=$(printf '%s\n' "${reference[@]}" | sort)
  local actual_sorted=$(echo "$actual" | sort)

  if [[ "$expected" != "$actual_sorted" ]]; then
    echo -e "Attribute mismatch detected!"
    echo -e "\nExpected:\n--\n$expected\n--\n"
    echo -e "\nActual:\n--\n$actual_sorted\n--\n"

    echo -e "\nMissing required attributes (in reference but not in actual):"
    comm -23 <(echo "$expected") <(echo "$actual_sorted")
    echo -e "\nUnexpected attributes (in actual but not in reference):"
    comm -13 <(echo "$expected") <(echo "$actual_sorted")
    return 1
  fi

  return 0
}


# Get device attributes for a device in a GPU resource slice.
# Emit attributes to stdout.
# Log to stderr.
# If node name is provided, query first slice from that node.
# If no node name is provided, query first slice overall.
# Usage: get_device_attrs_from_any_gpu_slice "gpu"
#        get_device_attrs_from_any_gpu_slice "mig"
#        get_device_attrs_from_any_gpu_slice "mig" "<nodename>"
get_device_attrs_from_any_gpu_slice() {
  local device_type="$1"
  local node_name="$2"
  local spath="${BATS_TEST_TMPDIR}/gpu_resource_slice_content"
  local slicenames

  # For debugging, show current set of slices.
  kubectl get resourceslice >&2

  # Get names of resource slices: either for all gpu plugin resource slices, or
  # all for all gpu slices announced by a specific node.
  if [ -n "$node_name" ]; then
    slicenames="$(kubectl get resourceslices.resource.k8s.io | grep 'gpu.nvidia.com' | grep "$node_name" | awk '{print $1}')"
  else
    slicenames="$(kubectl get resourceslices.resource.k8s.io | grep 'gpu.nvidia.com' | awk '{print $1}')"
  fi

  # Identify a suitable (device-announcing) resource slice. This is relevant
  # because on Kubernetes 1.35+ there is one special resource slice per node
  # that only contains counters, and no devices. We must make sure that we
  # do not use that for device attribute extraction.
  local slicename
  for slicename in $slicenames; do
    kubectl get resourceslices.resource.k8s.io -o yaml "${slicename}" > "${spath}"
    if grep -q "devices:" "${spath}"; then
      log "identified suitable resource slice: $slicename"
      break
    fi
    log "do not use slice $slicename (does not define devices)"
  done

  log "wrote resource slice content to: ${spath}"

  # Log contents, for https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/issues/902
  cat "${spath}" >&2

  # For the first device in that slice (of given type), extract the set of
  # device attribute _keys_. Emit those keys, one per line. --raw-output strips
  # quotes.
  yq --raw-output "[.spec.devices[] | select(.attributes.type.string == \"${device_type}\")] | .[0] | .attributes | keys | .[]" "${spath}"
}


# Get Kubernetes node count, emit to stdout. Fail function
# if kubectl fails.
get_node_count() {
  local nodes

  if ! nodes=$(kubectl get nodes --no-headers 2>/dev/null); then
    return 1
  fi

  if [[ -z "$nodes" ]]; then
    echo 0
  else
    echo "$nodes" | wc -l | awk '{print $1}'
  fi
}


show_kubelet_plugin_error_logs() {
  echo -e "\nKUBELET PLUGIN ERROR LOGS START"
  (
    kubectl logs \
    -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    -n dra-driver-nvidia-gpu \
    --all-containers \
    --prefix --tail=-1 | grep -E -e "^(E|W)[0-9]{4}" -e "error"
  ) || true
  echo -e "KUBELET PLUGIN ERROR LOGS END\n\n"
}


show_kubelet_plugin_log_tails() {
  echo -e "\nKUBELET PLUGIN LOG TAILS START"
  (
    kubectl logs \
    -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    -n dra-driver-nvidia-gpu \
    --all-containers \
    --prefix --tail=400
  ) || true
  echo -e "KUBELET PLUGIN LOG TAILS END\n\n"
}


show_gpu_plugin_log_tails() {
  echo -e "\nKUBELET GPU PLUGIN LOGS TAILS(400) START"
  (
    kubectl logs \
    -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    -n dra-driver-nvidia-gpu \
    --container gpus \
    --prefix --tail=400
  ) || true
  echo -e "KUBELET GPU PLUGIN LOG TAILS(400) END\n\n"
}


# Intended use case: one pod in Running or ContainerCreating state; then this
# function returns the specific name of that pod. Specifically, ignore pods that
# were just deleted or are terminating (this is important during the small time
# window of restarting the controller, say in response to a deployment podspec
# template mutation).
get_current_controller_pod_name() {
  kubectl get pod \
    -l dra-driver-nvidia-gpu-component=controller \
    -n dra-driver-nvidia-gpu \
      | grep -iv "NAME" \
      | grep -iv 'completed' \
      | grep -iv 'terminating' \
      | awk '{print $1}'
}


get_one_kubelet_plugin_pod_name() {
  kubectl get pod \
    -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    -n dra-driver-nvidia-gpu \
      | grep -iv "NAME" \
      | grep -i 'running' \
      | head -n1 \
      | awk '{print $1}'
}


apply_check_delete_workload_imex_chan_inject() {
  kubectl apply -f demo/specs/imex/channel-injection.yaml
  kubectl wait --for=condition=READY pods imex-channel-injection --timeout=100s
  run kubectl logs imex-channel-injection
  assert_output --partial "channel0"

  # Wait for deletion to complete; this is critical before moving on to the next
  # test (as long as we don't wipe state entirely between tests).
  kubectl delete -f demo/specs/imex/channel-injection.yaml
  kubectl wait --for=delete pods imex-channel-injection --timeout=10s
}


mig_confirm_disabled_on_all_nodes() {
  # Confirm that MIG mode is disabled for all GPUs; in all nodes.
  run nvmm all sh -c 'nvidia-smi --query-gpu=index,mig.mode.current --format=csv'
  refute_output --partial "Enabled"
}


# Enable MIG mode on the selected node on GPU 0, and create a MIG device. Pick
# the first listed 1g profile (available on all MIG-capable GPUs).
mig_create_1g0_on_node() {
  local nodename="$1"
  local mprofile=$(nvmm "$nodename" nvidia-smi mig -lgip -i 0 | grep -m 1 -oE '1g\.[1-9]+gb')
  echo "selected MIG profile: $mprofile"
  nvmm "$nodename" nvidia-smi -i 0 -mig 1
  nvmm "$nodename" nvidia-smi mig -cgi "$mprofile" -C
  log "created mig dev"
}


# On all nodes, attempt ot destroy all MIG devices and disable MIG mode for all
# physical GPUs. Fail the consuming test if any GPU in any of the nodes still
# has MIG mode enabled. This can serve as 1) an explicit assertion about current
# state when entering a test, and 2) a convenient cleanup routine during test
# development, and 3) a regular cleanup when leaving a test.
mig_ensure_teardown_on_all_nodes() {
  nvmm all sh -c 'nvidia-smi mig -dci; nvidia-smi mig -dgi; nvidia-smi -mig 0'
  mig_confirm_disabled_on_all_nodes
}


restart_kubelet_on_node() {
  local NODEIP="$1"
  echo "sytemctl restart kubelet.service on ${NODEIP}"
  # Assume that current user has password-less sudo privileges
  ssh "${USER}@${NODEIP}" 'sudo systemctl restart kubelet.service'
}


restart_kubelet_all_nodes() {
  for nodeip in $(kubectl get nodes -o jsonpath='{range .items[*]}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}'); do
    restart_kubelet_on_node "$nodeip"
  done
  #wait
  echo "restart kubelets: done"
}


kplog () {
  if [[ -z "$1" || -z "$2" ]]; then
    echo "Usage: kplog [gpus|compute-domains] <node-hint-for-grep> [args]"
    return 1
  fi
  local nodehint="$2"
  local cont="$1"
  shift
  shift # Remove first argument, leaving remaining args in $@

  local node=$(kubectl get nodes | grep "$nodehint" | awk '{print $1}')
  echo "identified node: $node"

  local pod
  pod=$(kubectl get pod -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    --field-selector spec.nodeName="$node" \
    --no-headers -o custom-columns=":metadata.name")

  if [ -z "$pod" ]; then
    echo " get pod -n dra-driver-nvidia-gpu -l dra-driver-nvidia-gpu-component=kubelet-plugin: no pod found on node $node"
    return 1
  fi

  echo "Executing on pod $pod (node: $node)..."
  kubectl logs -n dra-driver-nvidia-gpu "$pod" -c "$cont" "$@"
}


_log_ts_no_newline() {
    echo -n "$(date -u +'%Y-%m-%dT%H:%M:%S.%3NZ ')" >&2
}


# For measuring duration with sub-second precision.
export _T0=$(awk '{print $1}' /proc/uptime)
log() {
  _TNOW=$(awk '{print $1}' /proc/uptime)
  _DUR=$(echo "$_TNOW - $_T0" | bc)
  _log_ts_no_newline
  printf "[%6.1fs] $1\n" "$_DUR" >&2
}
