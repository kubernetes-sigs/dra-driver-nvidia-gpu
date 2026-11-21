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

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CHART_PATH="${REPO_ROOT}/deployments/helm/dra-driver-nvidia-gpu"
common=(vfio-only-test "${CHART_PATH}" --namespace vfio-only-test
    --set gpuResourcesEnabledOverride=true --set vfioOnly=true)

rendered=$(helm template "${common[@]}" --set featureGates.PassthroughSupport=true \
    --set resources.computeDomains.enabled=false)
if [[ $(sed -n '/name: VFIO_ONLY/{n;p;}' <<<"${rendered}" | grep -c 'value: "true"') -ne 2 ]]; then
    echo "expected VFIO_ONLY=true in both the init container and GPU plugin" >&2
    exit 1
fi

expect_failure() {
    local expected=$1
    shift
    local output
    if output=$(helm template "${common[@]}" "$@" 2>&1); then
        echo "expected Helm to reject invalid VFIO-only configuration" >&2
        exit 1
    fi
    if [[ "${output}" != *"${expected}"* ]]; then
        echo "expected: ${expected}; got: ${output}" >&2
        exit 1
    fi
}

expect_failure "vfioOnly requires featureGates.PassthroughSupport=true" \
    --set resources.computeDomains.enabled=false
expect_failure "vfioOnly requires resources.computeDomains.enabled=false" \
    --set featureGates.PassthroughSupport=true
expect_failure "vfioOnly is incompatible with featureGates.FabricManagerPartitioning" \
    --set featureGates.PassthroughSupport=true --set resources.computeDomains.enabled=false \
    --set featureGates.FabricManagerPartitioning=true

# Intercept only the init script's symlink creation so this test never writes /driver-root.
test_bin=$(mktemp -d)
trap 'rm -rf "${test_bin}"' EXIT
printf '#!/bin/sh\nexit 0\n' > "${test_bin}/ln"
chmod +x "${test_bin}/ln"
output=$(PATH="${test_bin}:${PATH}" VFIO_ONLY=true NVIDIA_DRIVER_ROOT=/ \
    bash "${REPO_ROOT}/hack/kubelet-plugin-prestart.sh")
if [[ "${output}" != *"skipping NVIDIA driver validation"* ]]; then
    echo "expected VFIO-only init to skip NVIDIA driver validation" >&2
    exit 1
fi
