#!/usr/bin/env bash
# Copyright The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# e2e-test.sh -- outer orchestrator for the GCP + nvkind DRA e2e.
#
# Runs on the Prow pod (or a laptop). Acquires a Boskos GPU project,
# provisions a GCE VM, brings up a DRA-enabled kind cluster, installs
# GPU Operator + DRA driver, runs the Ginkgo suite, tears everything down.
#
# Prow-provided env: JOB_NAME, BUILD_ID, ARTIFACTS.
# Prow YAML-provided env: BOSKOS_HOST, BOSKOS_RESOURCE_TYPE, GCE_ZONE,
# GCE_MACHINE_TYPE, GCE_ACCELERATOR, GCE_IMAGE_FAMILY, GCE_IMAGE_PROJECT,
# K8S_VERSION, GPU_OPERATOR_VERSION.
# Setting GCP_PROJECT skips Boskos (local dev).

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
LIB_DIR="${SCRIPT_DIR}/lib"

# Prefer the shared lib from the test-infra checkout when TESTINFRA_DIR is
# set (Prow); fall back to the in-repo duplicate for local runs.
if [ -n "${TESTINFRA_DIR:-}" ] && [ -d "${TESTINFRA_DIR}/experiment/gcp-nvkind/lib" ]; then
  LIB_DIR="${TESTINFRA_DIR}/experiment/gcp-nvkind/lib"
fi

# shellcheck source=lib/boskos.sh
source "${LIB_DIR}/boskos.sh"
# shellcheck source=lib/gce.sh
source "${LIB_DIR}/gce.sh"

: "${JOB_NAME:=dra-gcp-nvkind-local}"
: "${BUILD_ID:=$(date +%s)}"
: "${ARTIFACTS:=/tmp/dra-gcp-nvkind-artifacts}"
: "${BOSKOS_HOST:=http://boskos.test-pods.svc.cluster.local}"
: "${BOSKOS_RESOURCE_TYPE:=gpu-project}"
: "${GCE_ZONE:=us-central1-b}"
: "${K8S_VERSION:=v1.37.0}"
: "${GPU_OPERATOR_VERSION:=v26.3.1}"

mkdir -p "${ARTIFACTS}"
export JOB_NAME BUILD_ID ARTIFACTS

if [ -n "${GCP_PROJECT:-}" ]; then
  echo "Local mode: skipping Boskos, using GCP_PROJECT=${GCP_PROJECT}"
else
  boskos::acquire
  export GCP_PROJECT="${BOSKOS_PROJECT}"
  boskos::heartbeat_start
fi

VM_NAME="dra-ci-${BUILD_ID}"
VM_NAME=${VM_NAME:0:63}   # GCE VM names are <=63 chars.
export VM_NAME GCP_PROJECT GCE_ZONE

cleanup() {
  local rc=$?
  set +e
  echo "cleanup rc=${rc}"
  if [ "${rc}" -ne 0 ] && [ -n "${VM_NAME:-}" ]; then
    gce::ssh "NVKIND_CLUSTER_NAME=dra-ci bash /tmp/dra-src/hack/ci/gcp-nvkind/collect-artifacts.sh" \
      > "${ARTIFACTS}/cluster-debug.txt" 2>&1 || true
  fi
  gce::delete || true
  boskos::release
  exit "${rc}"
}
trap cleanup EXIT

# 1. Create VM, wait for driver.
gce::create
gce::wait_for_driver

# 2. Ship repo to VM. Vendor is included (Dockerfile uses -mod=vendor).
WORK=$(mktemp -d)
# COPYFILE_DISABLE and the ._* exclude keep macOS tar from adding AppleDouble
# files, which Helm then fails to parse as CRDs on local runs from a Mac.
COPYFILE_DISABLE=1 tar --exclude='._*' --exclude='.git' --exclude='_output' --exclude='dist' --exclude='.claude' --exclude='site' \
  -czf "${WORK}/dra-src.tgz" -C "${REPO_ROOT}" .
gce::scp_to "${WORK}/dra-src.tgz" "/tmp/"
gce::ssh 'rm -rf /tmp/dra-src && mkdir -p /tmp/dra-src && tar -xzf /tmp/dra-src.tgz -C /tmp/dra-src'
rm -rf "${WORK}"

# 3. Install toolchain + nvkind cluster.
gce::ssh "NVKIND_CLUSTER_NAME=dra-ci \
          NVKIND_K8S_VERSION='${K8S_VERSION}' \
          NVKIND_CONFIG_PATH=/tmp/dra-src/hack/ci/gcp-nvkind/lib/nvkind-config.yaml.tmpl \
          NVKIND_DELETE_EXISTING=true \
          bash /tmp/dra-src/hack/ci/gcp-nvkind/lib/setup-nvkind-node.sh"

# 4. GPU Operator (minimal mode).
gce::ssh "NVKIND_CLUSTER_NAME=dra-ci \
          GPU_OPERATOR_VERSION='${GPU_OPERATOR_VERSION}' \
          bash /tmp/dra-src/hack/ci/gcp-nvkind/install-gpu-operator.sh"

# 5. Build + load + install DRA driver from source.
gce::ssh "NVKIND_CLUSTER_NAME=dra-ci \
          DRA_SRC_DIR=/tmp/dra-src \
          bash /tmp/dra-src/hack/ci/gcp-nvkind/install-dra-driver.sh"

# 6. Ginkgo suite.
gce::ssh "cd /tmp/dra-src && ARTIFACTS=/tmp/ginkgo-artifacts \
          PATH=/usr/local/go/bin:\$HOME/go/bin:\$PATH \
          make test-e2e"
gce::scp_from /tmp/ginkgo-artifacts "${ARTIFACTS}/ginkgo-artifacts" || true

# 7. Artifacts on success.
gce::ssh "NVKIND_CLUSTER_NAME=dra-ci ARTIFACTS=/tmp/cluster-artifacts \
          bash /tmp/dra-src/hack/ci/gcp-nvkind/collect-artifacts.sh" \
  > "${ARTIFACTS}/cluster-debug.txt" 2>&1 || true
gce::scp_from /tmp/cluster-artifacts "${ARTIFACTS}/" || true

echo "=== DONE ==="
