# shellcheck disable=SC2148
# shellcheck disable=SC2329

setup_file () {
  load 'helpers.sh'
  _common_setup

  # Try to establish well-known state before entering tests. Any previous
  # partial cleanup or partial test might leave an exotic resource slice state
  # behind. Delete slices, and to do that reliably: delete DRA driver before, if
  # it exists.
  helm uninstall -n dra-driver-nvidia-gpu dra-driver-nvidia-gpu-batssuite --wait || true
  kubectl delete resourceslices.resource.k8s.io --all

  local _iargs=("--set" "logVerbosity=6" "--set" "featureGates.DynamicMIG=true")
  if [ "${DISABLE_COMPUTE_DOMAINS:-}" = "true" ]; then
    _iargs+=("--set" "resources.computeDomains.enabled=false")
  fi
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs
  run kubectl logs \
    -l dra-driver-nvidia-gpu-component=kubelet-plugin \
    -n dra-driver-nvidia-gpu \
    -c gpus \
    --prefix --tail=-1
  assert_output --partial "About to announce device gpu-0-mig-1g"

  # Wait until resource slices are announced. See
  # https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/issues/902 -- Maybe we should
  # fail the liveness probe until the first resource slice update is known to
  # have been performed? That may be too invasive. In any case, the resource
  # slice update controller running in the DRA plugin helper might enable us to
  # to something smart here. For the purpose of testing, deleting all resource
  # slices above and then waiting until resource slices pop up again: that seems
  # to be robust.
  wait_for_all_gpu_resource_slices 15
  confirm_expected_mig_mode_all_nodes
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
  kubectl describe pods | grep -A20 "Events:"
  echo -e "FAILURE HOOK END\n\n"
}


confirm_expected_mig_mode_all_nodes() {
  # Lambda CI has no GPU Operator and uses a no-op nvmm implementation.
  case "${TEST_CI:-}" in
    lambda)
      echo "MIG mode check is a no-op in Lambda CI"
      return
      ;;
  esac

  local expected="Disabled"
  if [ "${TEST_MIG_MODE_TOGGLE_SUPPORTED:-true}" = "false" ]; then
    expected="Enabled"
  fi

  run nvmm all sh -c 'nvidia-smi --query-gpu=mig.mode.current --format=csv,noheader'
  assert_success

  local mode_count=0
  while IFS= read -r line; do
    # Ignore nvmm's per-node header.
    [[ "$line" == "-- "* ]] && continue

    assert_equal "$line" "$expected" || return 1
    ((++mode_count))
  done <<< "$output"

  assert_not_equal "$mode_count" "0"
}


# bats test_tags=fastfeedback,dynmig,version-specific
@test "DynMIG: inspect device attributes in resource slice (gpu)" {
  if [ "${TEST_MIG_MODE_TOGGLE_SUPPORTED:-true}" = "false" ]; then
    local gpu_count
    gpu_count=$(kubectl get resourceslices.resource.k8s.io -o json | \
      jq '[.items[] | select(.spec.driver == "gpu.nvidia.com") | .spec.devices[]? | select(.attributes.type.string == "gpu")] | length')
    assert_equal "0" "$gpu_count"
    return
  fi

  local reference=(
    "architecture"
    "brand"
    "cudaComputeCapability"
    "cudaDriverVersion"
    "driverVersion"
    "productName"
    "resource.kubernetes.io/numaNode"
    "resource.kubernetes.io/pciBusID"
    "resource.kubernetes.io/pcieRoot"
    "type"
    "uuid"
    "addressingMode"
  )

  local attrs
  attrs=$(get_device_attrs_from_any_gpu_slice "gpu")
  log "attributes seen: $attrs"
  assert_attrs_equal "$attrs" "${reference[@]}"
}


# bats test_tags=fastfeedback,dynmig,version-specific
@test "DynMIG: inspect device attributes in resource slice (mig)" {
  local reference=(
    "architecture"
    "brand"
    "cudaComputeCapability"
    "cudaDriverVersion"
    "driverVersion"
    "productName"
    "resource.kubernetes.io/numaNode"
    "resource.kubernetes.io/pciBusID"
    "resource.kubernetes.io/pcieRoot"
    "type"
    "addressingMode"
    "parentUUID"
    "profile"
  )

  local attrs
  attrs=$(get_device_attrs_from_any_gpu_slice "mig")
  log "attributes seen: $attrs"
  assert_attrs_equal "$attrs" "${reference[@]}"
}


# bats test_tags=fastfeedback,dynmig
@test "DynMIG: 1 pod, 1 MIG" {
  confirm_expected_mig_mode_all_nodes
  kubectl apply -f tests/bats/specs/gpu-simple-mig.yaml
  kubectl wait --for=condition=READY pods pod-mig1g --timeout=10s
  run kubectl logs pod-mig1g

  # Confirm the following pattern:
  # GPU 0: NVIDIA GB200 (UUID: GPU-7277883e-ce1e-3b6e-6cc1-6d52e80cdb86)
  #   MIG 1g.24gb     Device  0: (UUID: MIG-5b696ac1-c323-589e-a082-e6045e980bf4)
  assert_output --partial "UUID: MIG-"
  assert_output --partial "UUID: GPU-"

  # Make sure the output contains two lines (first wc -l for debuggability)
  echo "${output}" | wc -l
  echo "${output}" | wc -l | grep 2

  kubectl delete -f tests/bats/specs/gpu-simple-mig.yaml
  kubectl wait --for=delete pods pod-mig1g --timeout=10s
  confirm_expected_mig_mode_all_nodes
}


# bats test_tags=fastfeedback,dynmig
@test "DynMIG: 1 pod, 2 containers (1 MIG each)" {
  confirm_expected_mig_mode_all_nodes

  local _specpath="tests/bats/specs/gpu-multiple-mig.yaml"
  local _podname="pod-2mig"

  kubectl apply -f "${_specpath}"
  kubectl wait --for=condition=READY pods "${_podname}" --timeout=10s

  run kubectl logs "${_podname}" -c ctr0
  assert_output --partial "UUID: MIG-"
  assert_output --partial "UUID: GPU-"
  echo "${output}"
  echo "${output}" | wc -l | grep 2
  mig1uuid="$(echo "${output}" | grep -oP 'MIG-\K[0-9a-f-]+')"

  run kubectl logs "${_podname}" -c ctr1
  assert_output --partial "UUID: MIG-"
  assert_output --partial "UUID: GPU-"
  echo "${output}"
  echo "${output}" | wc -l | grep 2
  mig2uuid="$(echo "${output}" | grep -oP 'MIG-\K[0-9a-f-]+')"

  assert_not_equal "$mig1uuid" "$mig2uuid"

  kubectl delete -f  "${_specpath}"
  kubectl wait --for=delete pods "${_podname}" --timeout=10s
  confirm_expected_mig_mode_all_nodes
}


# bats test_tags=fastfeedback,dynmig
@test "DynMIG: 1 pod, 1 MIG + TimeSlicing config" {
  local _iargs=("--set" "logVerbosity=6" "--set" "featureGates.DynamicMIG=true" "--set" "featureGates.TimeSlicingSettings=true")
  if [ "${DISABLE_COMPUTE_DOMAINS:-}" = "true" ]; then
    _iargs+=("--set" "resources.computeDomains.enabled=false")
  fi
  iupgrade_wait "${TEST_CHART_REPO}" "${TEST_CHART_VERSION}" _iargs

  confirm_expected_mig_mode_all_nodes
  kubectl apply -f tests/bats/specs/gpu-simple-mig-ts.yaml
  kubectl wait --for=condition=READY pods pod-mig1g --timeout=10s
  run kubectl logs pod-mig1g

  # Confirm the following pattern:
  # GPU 0: NVIDIA GB200 (UUID: GPU-7277883e-ce1e-3b6e-6cc1-6d52e80cdb86)
  #   MIG 1g.24gb     Device  0: (UUID: MIG-5b696ac1-c323-589e-a082-e6045e980bf4)
  assert_output --partial "UUID: MIG-"
  assert_output --partial "UUID: GPU-"

  # Make sure the output contains two lines (first wc -l for debuggability)
  echo "${output}" | wc -l
  echo "${output}" | wc -l | grep 2

  kubectl delete -f tests/bats/specs/gpu-simple-mig-ts.yaml
  kubectl wait --for=delete pods pod-mig1g --timeout=10s
  confirm_expected_mig_mode_all_nodes
}

# bats test_tags=fastfeedback,dynmig
@test "DynMIG: 1 pod, 1 smallest-slice MIG" {
  confirm_expected_mig_mode_all_nodes
  kubectl apply -f tests/bats/specs/gpu-smallest-slice-mig.yaml
  kubectl wait --for=condition=READY pods pod-mig-small --timeout=10s
  run kubectl logs pod-mig-small

  # Confirm the following pattern:
  # GPU 0: NVIDIA GB200 (UUID: GPU-7277883e-ce1e-3b6e-6cc1-6d52e80cdb86)
  #   MIG 1g.24gb     Device  0: (UUID: MIG-5b696ac1-c323-589e-a082-e6045e980bf4)
  assert_output --partial "UUID: MIG-"
  assert_output --partial "UUID: GPU-"

  # Make sure the output contains two lines (first wc -l for debuggability)
  echo "${output}" | wc -l
  echo "${output}" | wc -l | grep 2

  kubectl delete -f tests/bats/specs/gpu-smallest-slice-mig.yaml
  kubectl wait --for=delete pods pod-mig-small --timeout=10s
  confirm_expected_mig_mode_all_nodes
}

# bats test_tags=fastfeedback,dynmig
@test "DynMIG: 1 pod, 1 largest-slice MIG" {
  confirm_expected_mig_mode_all_nodes
  kubectl apply -f tests/bats/specs/gpu-largest-slice-mig.yaml
  kubectl wait --for=condition=READY pods pod-mig-large --timeout=10s
  run kubectl logs pod-mig-large

  # Confirm the following pattern:
  # GPU 0: NVIDIA GB200 (UUID: GPU-7277883e-ce1e-3b6e-6cc1-6d52e80cdb86)
  #   MIG 1g.24gb     Device  0: (UUID: MIG-5b696ac1-c323-589e-a082-e6045e980bf4)
  assert_output --partial "UUID: MIG-"
  assert_output --partial "UUID: GPU-"

  # Make sure the output contains two lines (first wc -l for debuggability)
  echo "${output}" | wc -l
  echo "${output}" | wc -l | grep 2

  kubectl delete -f tests/bats/specs/gpu-largest-slice-mig.yaml
  kubectl wait --for=delete pods pod-mig-large --timeout=10s
  confirm_expected_mig_mode_all_nodes
}
