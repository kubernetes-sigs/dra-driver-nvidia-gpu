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

# Cloud Build / Prow: package the Helm chart and push it to
# oci://${IMG_PREFIX}/charts. Chart semver is IMG_TAG with a leading "v" removed.

set -euo pipefail

REPO_ROOT=$(git rev-parse --show-toplevel)
cd "$REPO_ROOT"

if [[ -z ${IMG_PREFIX:-} ]]; then
	echo "IMG_PREFIX is not set" >&2
	exit 1
fi

if [[ -z ${IMG_TAG:-} ]]; then
	if git describe --exact-match --tags HEAD >/dev/null 2>&1; then
		IMG_TAG=$(git describe --exact-match --tags HEAD)
	else
		IMG_TAG=$(make --no-print-directory -f "${REPO_ROOT}/versions.mk" print-VERSION_W_COMMIT)
	fi
fi
echo "Using IMG_TAG=${IMG_TAG}"

CHART_VERSION="${IMG_TAG#v}"
echo "Using CHART_VERSION=${CHART_VERSION} (IMG_TAG without leading v)"

DRIVER_NAME=$(make --no-print-directory -f "${REPO_ROOT}/versions.mk" print-DRIVER_NAME)
HELM="${HELM:-helm}"
COSIGN="${COSIGN:-cosign}"
COSIGN_VERSION="${COSIGN_VERSION:-v3.0.6}"
JQ_VERSION="${JQ_VERSION:-jq-1.7.1}"
CHART_PROVENANCE="${CHART_PROVENANCE:-false}"
CHART_SBOM="${CHART_SBOM:-false}"
DIST_DIR="${REPO_ROOT}/dist"

chart_attestations_enabled() {
	[[ "${CHART_PROVENANCE}" == "true" || "${CHART_SBOM}" == "true" ]]
}

ensure_cosign() {
	if command -v "${COSIGN}" >/dev/null 2>&1; then
		return
	fi

	if [[ "${COSIGN}" != "cosign" ]]; then
		echo "COSIGN=${COSIGN} is not available" >&2
		exit 1
	fi

	echo "Installing cosign ${COSIGN_VERSION}..."
	curl -sSfL --retry 8 --retry-all-errors --connect-timeout 10 --retry-delay 5 \
		-o /usr/local/bin/cosign \
		"https://github.com/sigstore/cosign/releases/download/${COSIGN_VERSION}/cosign-linux-amd64"
	chmod +x /usr/local/bin/cosign
}

ensure_jq() {
	if command -v jq >/dev/null 2>&1; then
		return
	fi

	echo "Installing jq ${JQ_VERSION}..."
	curl -sSfL --retry 8 --retry-all-errors --connect-timeout 10 --retry-delay 5 \
		-o /usr/local/bin/jq \
		"https://github.com/jqlang/jq/releases/download/${JQ_VERSION}/jq-linux-amd64"
	chmod +x /usr/local/bin/jq
}

cosign_attest_chart() {
	local predicate=$1
	local predicate_type=$2
	local label=$3
	local cmd=("${COSIGN}" attest --yes --predicate "${predicate}" --type "${predicate_type}")

	if [[ -n ${COSIGN_KEY:-} ]]; then
		cmd+=(--key "${COSIGN_KEY}")
	fi
	if [[ -n ${COSIGN_IDENTITY_TOKEN:-} ]]; then
		cmd+=(--identity-token "${COSIGN_IDENTITY_TOKEN}")
	fi
	if [[ -n ${COSIGN_OIDC_PROVIDER:-} ]]; then
		cmd+=(--oidc-provider "${COSIGN_OIDC_PROVIDER}")
	fi

	cmd+=("${CHART_REF}")

	echo "Attesting ${label} for ${CHART_REF}"
	"${cmd[@]}"
}

if ! command -v helm >/dev/null 2>&1; then
	echo "Installing Helm 3..."
	curl -sSfLO --retry 8 --retry-all-errors --connect-timeout 10 --retry-delay 5 \
		https://get.helm.sh/helm-v3.18.6-linux-amd64.tar.gz
	tar -zxvf helm-v3*linux-amd64.tar.gz
	mv linux-amd64/helm /usr/local/bin/helm
fi

if [[ -z ${GIT_COMMIT:-} ]]; then
	GIT_COMMIT=$(git rev-parse HEAD)
fi
echo "Using GIT_COMMIT=${GIT_COMMIT}"

if chart_attestations_enabled; then
	ensure_jq
	ensure_cosign
fi

REGISTRY_HOST="${IMG_PREFIX%%/*}"
if command -v gcloud >/dev/null 2>&1; then
	case "${REGISTRY_HOST}" in
	*.pkg.dev | gcr.io | *.gcr.io)
		gcloud auth configure-docker "${REGISTRY_HOST}" --quiet
		;;
	esac
fi

mkdir -p "${DIST_DIR}"
rm -f "${DIST_DIR}/${DRIVER_NAME}-"*.tgz

# Staging image registry swap for non-release builds only. Tagged (release) builds
# keep registry.k8s.io/dra-driver-nvidia in values.yaml for promoted charts.
VALUES="${REPO_ROOT}/deployments/helm/${DRIVER_NAME}/values.yaml"
if git describe --exact-match --tags HEAD >/dev/null 2>&1; then
	echo "Tagged release build: skipping staging registry rewrite in values.yaml"
else
	sed -i 's|registry.k8s.io/dra-driver-nvidia|us-central1-docker.pkg.dev/k8s-staging-images/dra-driver-nvidia|g' "${VALUES}"
	git diff || echo "ignore git diff exit code"
fi

"${HELM}" package "deployments/helm/${DRIVER_NAME}" \
	--version "${CHART_VERSION}" \
	--app-version "${CHART_VERSION}" \
	--destination "${DIST_DIR}"

CHART_TGZ="${DIST_DIR}/${DRIVER_NAME}-${CHART_VERSION}.tgz"
echo "Pushing ${CHART_TGZ} -> oci://${IMG_PREFIX}/charts"
if ! PUSH_OUTPUT=$("${HELM}" push "${CHART_TGZ}" "oci://${IMG_PREFIX}/charts" 2>&1); then
	printf '%s\n' "${PUSH_OUTPUT}"
	exit 1
fi
printf '%s\n' "${PUSH_OUTPUT}"

if chart_attestations_enabled; then
	CHART_DIGEST=${CHART_DIGEST:-$(printf '%s\n' "${PUSH_OUTPUT}" | awk '/Digest:/ { print $2; exit }')}
	if [[ -z "${CHART_DIGEST}" ]]; then
		echo "could not determine chart digest from helm push output" >&2
		exit 1
	fi

	CHART_REF="${IMG_PREFIX}/charts/${DRIVER_NAME}@${CHART_DIGEST}"
	echo "Using CHART_REF=${CHART_REF}"

	CHART_NAME="${DRIVER_NAME}" \
	CHART_VERSION="${CHART_VERSION}" \
	GIT_COMMIT="${GIT_COMMIT}" \
	PULL_BASE_REF="${PULL_BASE_REF:-}" \
	BUILD_ID="${BUILD_ID:-}" \
	PROJECT_ID="${PROJECT_ID:-}" \
	bash "${REPO_ROOT}/hack/generate-helm-chart-attestation-predicates.sh" \
		"${CHART_TGZ}" \
		"${CHART_REF}" \
		"${DIST_DIR}"

	CHART_PREDICATE_PREFIX="${DIST_DIR}/${DRIVER_NAME}-${CHART_VERSION}"
	if [[ "${CHART_PROVENANCE}" == "true" ]]; then
		cosign_attest_chart "${CHART_PREDICATE_PREFIX}.slsa-provenance.json" slsaprovenance1 "SLSA provenance"
	fi
	if [[ "${CHART_SBOM}" == "true" ]]; then
		cosign_attest_chart "${CHART_PREDICATE_PREFIX}.sbom.spdx.json" spdxjson "SPDX SBOM"
	fi
fi
