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
RELEASE_NAME=validating-admission-policy-test
NAMESPACE=validating-admission-policy-test

kubelet_plugin=$(helm template "${RELEASE_NAME}" "${CHART_PATH}" \
    --namespace "${NAMESPACE}" \
    --set gpuResourcesEnabledOverride=true \
    --show-only templates/kubeletplugin.yaml)
policy=$(helm template "${RELEASE_NAME}" "${CHART_PATH}" \
    --namespace "${NAMESPACE}" \
    --set gpuResourcesEnabledOverride=true \
    --show-only templates/validatingadmissionpolicy.yaml)

mapfile -t service_accounts < <(
    sed -nE 's/^[[:space:]]*serviceAccountName:[[:space:]]*([^[:space:]#]+).*$/\1/p' \
        <<<"${kubelet_plugin}"
)
if [[ ${#service_accounts[@]} -ne 1 ]]; then
    echo "expected one kubelet-plugin serviceAccountName, found ${#service_accounts[@]}" >&2
    exit 1
fi

expected="request.userInfo.username == \"system:serviceaccount:${NAMESPACE}:${service_accounts[0]}\""
if [[ "${policy}" != *"${expected}"* ]]; then
    echo "ValidatingAdmissionPolicy does not target the kubelet-plugin service account" >&2
    echo "expected: ${expected}" >&2
    exit 1
fi

profiles_disabled=$(helm template "${RELEASE_NAME}" "${CHART_PATH}" \
    --namespace "${NAMESPACE}" \
    --set gpuResourcesEnabledOverride=true)
# Gate-off rendering must not introduce profile configuration artifacts.
for unexpected in "DEFAULT_CONFIG" "/available-configs" "gpu-driver-config"; do
    if [[ "${profiles_disabled}" == *"${unexpected}"* ]]; then
        echo "per-node GPU config artifact ${unexpected} rendered with the feature gate disabled" >&2
        exit 1
    fi
done

profiles_enabled=$(helm template "${RELEASE_NAME}" "${CHART_PATH}" \
    --namespace "${NAMESPACE}" \
    --set gpuResourcesEnabledOverride=true \
    --set featureGates.PerNodeGPUConfig=true)
# Gate-on rendering must wire the profile ConfigMap into the GPU plugin.
for expected_profile_artifact in "DEFAULT_CONFIG" "/available-configs" "gpu-driver-config"; do
    if [[ "${profiles_enabled}" != *"${expected_profile_artifact}"* ]]; then
        echo "per-node GPU config artifact ${expected_profile_artifact} missing with the feature gate enabled" >&2
        exit 1
    fi
done

# An explicit fallback must name an available profile.
if helm template "${RELEASE_NAME}" "${CHART_PATH}" \
    --namespace "${NAMESPACE}" \
    --set gpuResourcesEnabledOverride=true \
    --set featureGates.PerNodeGPUConfig=true \
    --set gpuDriverConfig.default=missing >/dev/null 2>&1; then
    echo "expected rendering to fail when the default GPU profile is missing" >&2
    exit 1
fi
