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

# install-dra-driver.sh -- build + kind-load + helm install on the VM.
#
# The chart's default image ref (registry.k8s.io/dra-driver-nvidia/...) has
# no published v0.4.0-dev tag, so this builds from the checked-out source
# and side-loads the resulting image into the kind cluster.

set -o errexit
set -o nounset
set -o pipefail
set -o xtrace

# setup-nvkind-node.sh leaves tools under /usr/local/go and $HOME/go/bin.
export PATH="/usr/local/go/bin:${HOME}/go/bin:${PATH}"

: "${NVKIND_CLUSTER_NAME:=dra-ci}"
: "${DRA_SRC_DIR:=/tmp/dra-src}"

CTX="kind-${NVKIND_CLUSTER_NAME}"

cd "${DRA_SRC_DIR}"

# versions.mk uses `git rev-parse`; seed a .git if the tarball excluded it.
if ! git rev-parse HEAD >/dev/null 2>&1; then
  git init -q
  git config user.email ci@local
  git config user.name ci
  git add -A
  git commit -q -m ci --allow-empty
fi

IMG=$(make -f deployments/container/Makefile -s print-IMAGE)
sg docker -c "make -f deployments/container/Makefile build DOCKER_BUILD_OPTIONS=--load"
sg docker -c "kind load docker-image '${IMG}' --name '${NVKIND_CLUSTER_NAME}'"

helm --kube-context "${CTX}" upgrade -i dra-driver-nvidia-gpu \
  "${DRA_SRC_DIR}/deployments/helm/dra-driver-nvidia-gpu" \
  --create-namespace -n dra-driver-nvidia-gpu \
  --set nvidiaDriverRoot=/ \
  --set gpuResourcesEnabledOverride=true \
  --set featureGates.ConsumableShares=true \
  --set featureGates.MPSSupport=true \
  --set consumableShares=unlimited \
  --wait --timeout=5m

# Wait for the driver's ResourceSlice to appear.
for i in $(seq 1 30); do
  n=$(kubectl --context "${CTX}" get resourceslices --no-headers 2>/dev/null | wc -l)
  [ "${n}" -gt "0" ] && break
  sleep 5
done

kubectl --context "${CTX}" get resourceslices -o wide
kubectl --context "${CTX}" get pods -n dra-driver-nvidia-gpu -o wide
