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


set -o nounset
set -o pipefail

CRD_URL="https://raw.githubusercontent.com/kubernetes-sigs/dra-driver-nvidia-gpu/main/deployments/helm/dra-driver-nvidia-gpu/crds/resource.nvidia.com_computedomains.yaml"


THIS_DIR_PATH=$(dirname "$(realpath $0)")
source "${THIS_DIR_PATH}/helpers.sh"

# For debugging: state of the world
kubectl get computedomains.resource.nvidia.com
kubectl get pods -n dra-driver-nvidia-gpu
helm list -A


# Facilitate containerized and also direct invocation
# of this script, without depending on HOME being set.
HELM_CACHE_HOME=$(mktemp -d -t helm-XXXXX)
export HELM_CACHE_HOME

# Facilitate direct (manual) invocation of this script.
TEST_CLEANUP_CHART_REPO="${TEST_CHART_REPO:-oci://gcr.io/k8s-staging-nvidia/charts/dra-driver-nvidia-gpu}"
TEST_CLEANUP_CHART_VERSION="${TEST_CHART_VERSION:-26.4.0-dev-f9de1ef3-chart}"
TEST_NVIDIA_DRIVER_ROOT="${TEST_NVIDIA_DRIVER_ROOT:-/run/nvidia/driver}"

# If a previous run leaves e.g. the controller behind in CrashLoopBackOff then
# the next installation with --wait won't succeed.
set -x
timeout -v 15 helm uninstall dra-driver-nvidia-gpu-batssuite -n dra-driver-nvidia-gpu

# When the CRD has been left behind deleted by a partially performed
# test then the deletions below cannot succeed. Apply a CRD version that
# likely helps deletion.
kubectl apply -f "${CRD_URL}"

# Workload deletion below requires a DRA driver to be present, to actually clean
# up. Install _a_ version temporarily, towards best-effort. Install
# to-be-tested-version for now, latest published staging chart might be smarter though. Again,
# this command may fail and in best-effort fashion this cleanup script proceeds.
set +x
iupgrade_wait "${TEST_CLEANUP_CHART_REPO}" "${TEST_CLEANUP_CHART_VERSION}" NOARGS
set -x

# Attempt cleanup. One goal is to never hang forever, hence the `timeout`
# wrappers. The following commands are OK to fail -- the `errexit` shell option
# is deliberately not set here. Repeated invocation of this cleanup script may
# break circular dependencies.
timeout -v 10 kubectl delete mpijobs.kubeflow.org --all
timeout -v 10 kubectl delete jobs --all --namespace=default
timeout -v 10 kubectl delete jobs --all --namespace=batssuite
timeout -v 10 kubectl delete pods --all --namespace=default
timeout -v 10 kubectl delete pods --all --namespace=batssuite
timeout -v 10 kubectl delete computedomain --all --namespace=default
timeout -v 10 kubectl delete computedomain --all --namespace=batssuite
timeout -v 10 kubectl delete resourceclaimtemplates --all --namespace=default
timeout -v 10 kubectl delete resourceclaimtemplates --all --namespace=batssuite
timeout -v 10 kubectl delete resourceclaim --all --namespace=default
timeout -v 10 kubectl delete resourceclaim --all --namespace=batssuite
timeout -v 2 kubectl delete resourceclaim batssuite-rc-bad-opaque-config --force 2> /dev/null

kubectl wait --for=delete pods -l 'env=batssuite,test=stress-shared' \
    --timeout=60s \
    || echo "wait-for-delete failed"

kubectl delete resourceclaimtemplate -l env=batssuite
kubectl delete resourceclaim -l env=batssuite

# The controller removes the finalizers of these, so wait before uninstalling.
timeout -v 300 kubectl delete computedomains -A --all --ignore-not-found

# Static modes delete their ComputeDomainDaemonSet when done. One left by an
# aborted run keeps its finalizer, as the chart above runs without the gate.
# Its static daemons are removed below.
for _CDDS in $(kubectl get computedomaindaemonsets -o name 2>/dev/null); do
  timeout -v 30 kubectl patch "${_CDDS}" --type=merge -p '{"metadata":{"finalizers":null}}'
done
timeout -v 30 kubectl delete computedomaindaemonsets --all --ignore-not-found

# Daemon objects in the driver namespace are not garbage collected, so remove
# those of ComputeDomains and ComputeDomainDaemonSets that are gone, also
# their finalizers, which only a controller with the right gate removes.
_LIVE=$( (kubectl get computedomains -A -o json; kubectl get computedomaindaemonsets -o json) 2>/dev/null | \
    jq -sc '[.[].items[].metadata.uid]')
_DEAD() {
  jq -r --argjson live "${_LIVE}" --arg key "$1" '.items[]
    | select(.metadata.labels[$key] as $u | $live | any(. == $u) | not)
    | "\(.kind)/\(.metadata.name)"'
}
for _KEY in resource.nvidia.com/computeDomain resource.nvidia.com/computeDomainDaemonSet; do
  for _OBJ in $(kubectl get daemonsets,resourceclaimtemplates -n dra-driver-nvidia-gpu -l "${_KEY}" -o json | _DEAD "${_KEY}"); do
    echo "removing ${_OBJ}, its owner is gone"
    timeout -v 30 kubectl patch -n dra-driver-nvidia-gpu "${_OBJ}" --type=merge -p '{"metadata":{"finalizers":null}}'
    timeout -v 60 kubectl delete -n dra-driver-nvidia-gpu "${_OBJ}" --ignore-not-found
  done
done
_KEY=resource.nvidia.com/computeDomain
for _OBJ in $(kubectl get devicetaintrules -l "${_KEY}" -o json 2>/dev/null | _DEAD "${_KEY}"); do
  timeout -v 30 kubectl delete "${_OBJ}" --ignore-not-found
done
for _NODE in $(kubectl get nodes -l "${_KEY}" -o json | _DEAD "${_KEY}"); do
  timeout -v 30 kubectl label "${_NODE}" "${_KEY}-"
done
for _CLIQUE in $(kubectl get computedomaincliques -A -o json 2>/dev/null | \
    jq -r --argjson live "${_LIVE}" '.items[]
      | select(.metadata.name | split(".")[0] as $u | $live | any(. == $u) | not)
      | "\(.metadata.namespace)/\(.metadata.name)"'); do
  timeout -v 30 kubectl patch computedomainclique -n "${_CLIQUE%%/*}" "${_CLIQUE#*/}" --type=merge -p '{"metadata":{"finalizers":null}}'
  timeout -v 30 kubectl delete computedomainclique -n "${_CLIQUE%%/*}" "${_CLIQUE#*/}" --ignore-not-found
done

# Delete any previous remainder of `clean-state-dirs-all-nodes.sh` invocation.
kubectl delete pods privpod-rm-plugindirs 2> /dev/null

# Uninstall chart (it was set up only to facilitate resource deletion).
helm uninstall dra-driver-nvidia-gpu-batssuite --wait -n dra-driver-nvidia-gpu
kubectl wait \
    --for=delete pods -A \
    -l app.kubernetes.io/name=dra-driver-nvidia-gpu \
    --timeout=15s \
        || echo "wait-for-delete failed"

# The next `helm install` must freshly install CRDs. Delete CRD now to achieve
# that. This might time out in case workload wasn't cleaned up properly. If that
# happens, the next test suite invocation will have failures like "create not
# allowed while custom resource definition is terminating".
# Objects whose finalizers the controller did not remove would keep a CRD,
# and with it the field owner of the `kubectl apply` above, which makes the
# next `helm install` fail with a conflict on .spec.versions.
for _KIND in computedomains computedomaincliques computedomaindaemonsets; do
  for _OBJ in $(kubectl get "${_KIND}.resource.nvidia.com" -A -o json 2>/dev/null | \
      jq -r '.items[] | "\(.metadata.namespace // "")/\(.metadata.name)"'); do
    _NS="${_OBJ%%/*}"
    timeout -v 30 kubectl patch "${_KIND}.resource.nvidia.com" "${_OBJ#*/}" \
        ${_NS:+-n "${_NS}"} --type=merge -p '{"metadata":{"finalizers":null}}'
  done
done
timeout -v 120 kubectl delete crds --ignore-not-found \
    computedomains.resource.nvidia.com \
    computedomaincliques.resource.nvidia.com \
    computedomaindaemonsets.resource.nvidia.com || echo "CRD deletion failed"

# Remove kubelet plugin state directories from all nodes (critical part of
# cleanup, fail hard if this does not succeed).
set -e
bash tests/bats/clean-state-dirs-all-nodes.sh

# Remove any stray MIG devices. Disable MIG mode unless this suite is running
# on GPUs where MIG mode cannot be toggled.
if [ "${TEST_MIG_MODE_TOGGLE_SUPPORTED:-true}" = "false" ]; then
  nvmm all sh -c 'nvidia-smi mig -dci; nvidia-smi mig -dgi' \
    || echo "nvmm MIG device cleanup skipped (non-fatal)"
else
  # Skip on A100 cloud VMs — disabling MIG mode can put the GPU in an
  # unrecoverable state (#883). Non-fatal for GPUs that don't support MIG.
  nvmm all sh -c '
    gpu_name=$(nvidia-smi --query-gpu=name --format=csv,noheader | head -1)
    case "$gpu_name" in
      *A100*) echo "Skipping MIG cleanup on A100 (#883)"; exit 0 ;;
    esac
    nvidia-smi mig -dci; nvidia-smi mig -dgi; nvidia-smi -mig 0
  ' || echo "nvmm MIG cleanup skipped (non-fatal)"
fi

set +x
echo "cleanup: done"
