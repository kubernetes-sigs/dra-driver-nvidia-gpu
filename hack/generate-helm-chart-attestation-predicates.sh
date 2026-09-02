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

# Generate predicate files for Helm chart OCI attestations.
#
# This emits:
# - SLSA provenance v1 predicate JSON for cosign --type slsaprovenance1
# - SPDX 2.3 JSON SBOM predicate for cosign --type spdxjson

set -euo pipefail

usage() {
	cat >&2 <<'EOF'
Usage: hack/generate-helm-chart-attestation-predicates.sh CHART_TGZ CHART_REF [OUT_DIR]

CHART_REF must be the OCI chart reference by digest, for example:
  us-central1-docker.pkg.dev/example/charts/dra-driver-nvidia-gpu@sha256:...
EOF
}

if [[ $# -lt 2 || $# -gt 3 ]]; then
	usage
	exit 2
fi

CHART_TGZ=$1
CHART_REF=$2
OUT_DIR=${3:-$(dirname "${CHART_TGZ}")}

if [[ ! -f "${CHART_TGZ}" ]]; then
	echo "chart package does not exist: ${CHART_TGZ}" >&2
	exit 1
fi

for tool in awk date jq sed sha256sum tar; do
	if ! command -v "${tool}" >/dev/null 2>&1; then
		echo "required tool not found: ${tool}" >&2
		exit 1
	fi
done

REPO_ROOT=${REPO_ROOT:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}
CHART_FILENAME=$(basename "${CHART_TGZ}")
CHART_BASENAME=${CHART_FILENAME%.tgz}
SLSA_PREDICATE="${OUT_DIR}/${CHART_BASENAME}.slsa-provenance.json"
SBOM_PREDICATE="${OUT_DIR}/${CHART_BASENAME}.sbom.spdx.json"

chart_yaml_path=$(tar -tzf "${CHART_TGZ}" | awk -F/ 'NF == 2 && $2 == "Chart.yaml" && found == 0 { print; found = 1 }')
if [[ -z "${chart_yaml_path}" ]]; then
	echo "could not find Chart.yaml in ${CHART_TGZ}" >&2
	exit 1
fi
chart_yaml=$(tar -xOzf "${CHART_TGZ}" "${chart_yaml_path}")

chart_yaml_value() {
	local key=$1
	printf '%s\n' "${chart_yaml}" \
		| sed -n "s/^${key}:[[:space:]]*//p" \
		| sed -e "s/[[:space:]]*#.*$//" -e "s/^['\"]//" -e "s/['\"]$//" \
		| awk 'NF && found == 0 { print; found = 1 }'
}

CHART_NAME=${CHART_NAME:-$(chart_yaml_value name)}
CHART_VERSION=${CHART_VERSION:-$(chart_yaml_value version)}

if [[ -z "${CHART_NAME}" ]]; then
	echo "could not determine chart name" >&2
	exit 1
fi

if [[ -z "${CHART_VERSION}" ]]; then
	echo "could not determine chart version" >&2
	exit 1
fi

CHART_SHA256=$(sha256sum "${CHART_TGZ}" | awk '{ print $1 }')
CREATED_ON=${SOURCE_DATE_EPOCH:+$(date -u -d "@${SOURCE_DATE_EPOCH}" '+%Y-%m-%dT%H:%M:%SZ')}
CREATED_ON=${CREATED_ON:-$(date -u '+%Y-%m-%dT%H:%M:%SZ')}

SOURCE_REPO_URL=${SOURCE_REPO_URL:-$(git -C "${REPO_ROOT}" config --get remote.origin.url 2>/dev/null || true)}
SOURCE_REPO_URL=${SOURCE_REPO_URL:-https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu}
case "${SOURCE_REPO_URL}" in
git@github.com:*)
	SOURCE_REPO_URL="https://github.com/${SOURCE_REPO_URL#git@github.com:}"
	;;
esac
SOURCE_REPO_URL=${SOURCE_REPO_URL%.git}
case "${SOURCE_REPO_URL}" in
git+*)
	SOURCE_URI=${SOURCE_REPO_URL}
	;;
*)
	SOURCE_URI="git+${SOURCE_REPO_URL}"
	;;
esac

if [[ -z ${SOURCE_REF:-} ]]; then
	if git -C "${REPO_ROOT}" describe --exact-match --tags HEAD >/dev/null 2>&1; then
		SOURCE_REF="refs/tags/$(git -C "${REPO_ROOT}" describe --exact-match --tags HEAD)"
	elif [[ -n ${PULL_BASE_REF:-} ]]; then
		SOURCE_REF="refs/heads/${PULL_BASE_REF}"
	else
		SOURCE_REF=$(git -C "${REPO_ROOT}" symbolic-ref -q --short HEAD 2>/dev/null || true)
		if [[ -n "${SOURCE_REF}" ]]; then
			SOURCE_REF="refs/heads/${SOURCE_REF}"
		fi
	fi
fi

SOURCE_URI_WITH_REF=${SOURCE_URI}
if [[ -n "${SOURCE_REF}" ]]; then
	SOURCE_URI_WITH_REF="${SOURCE_URI_WITH_REF}@${SOURCE_REF}"
fi

GIT_COMMIT=${GIT_COMMIT:-$(git -C "${REPO_ROOT}" rev-parse HEAD 2>/dev/null || true)}
PROJECT_ID=${PROJECT_ID:-}
BUILD_ID=${BUILD_ID:-}

if [[ -n "${PROJECT_ID}" && -n "${BUILD_ID}" ]]; then
	BUILDER_ID=${SLSA_BUILDER_ID:-"https://cloudbuild.googleapis.com/projects/${PROJECT_ID}"}
	INVOCATION_ID=${SLSA_INVOCATION_ID:-"https://cloudbuild.googleapis.com/v1/projects/${PROJECT_ID}/builds/${BUILD_ID}"}
elif [[ -n "${BUILD_ID}" ]]; then
	BUILDER_ID=${SLSA_BUILDER_ID:-"https://cloudbuild.googleapis.com/CloudBuild"}
	INVOCATION_ID=${SLSA_INVOCATION_ID:-"cloudbuild:${BUILD_ID}"}
else
	BUILDER_ID=${SLSA_BUILDER_ID:-"https://cloudbuild.googleapis.com/CloudBuild"}
	INVOCATION_ID=${SLSA_INVOCATION_ID:-}
fi

DOCUMENT_NAMESPACE="https://sigs.k8s.io/dra-driver-nvidia-gpu/spdx/${CHART_NAME}/${CHART_VERSION}/${CHART_SHA256}"

jq -n \
	--arg builder_id "${BUILDER_ID}" \
	--arg chart_name "${CHART_NAME}" \
	--arg chart_ref "${CHART_REF}" \
	--arg chart_sha256 "${CHART_SHA256}" \
	--arg chart_version "${CHART_VERSION}" \
	--arg created_on "${CREATED_ON}" \
	--arg git_commit "${GIT_COMMIT}" \
	--arg invocation_id "${INVOCATION_ID}" \
	--arg source_ref "${SOURCE_REF}" \
	--arg source_uri "${SOURCE_URI}" \
	--arg source_uri_with_ref "${SOURCE_URI_WITH_REF}" \
	'
	def compact_object: with_entries(select(.value != ""));

	{
		buildDefinition: {
			buildType: "https://cloudbuild.googleapis.com/CloudBuildYaml@v1",
			externalParameters: ({
				source: ({
					uri: $source_uri,
					ref: $source_ref,
					digest: (if $git_commit == "" then null else {gitCommit: $git_commit} end)
				} | compact_object),
				chart: {
					name: $chart_name,
					version: $chart_version,
					ref: $chart_ref,
					digest: {sha256: $chart_sha256}
				}
			} | compact_object),
			internalParameters: {},
			resolvedDependencies: (
				if $git_commit == "" then
					[]
				else
					[{uri: $source_uri_with_ref, digest: {gitCommit: $git_commit}}]
				end
			)
		},
		runDetails: {
			builder: {
				id: $builder_id
			},
			metadata: ({
				invocationId: $invocation_id,
				finishedOn: $created_on
			} | compact_object)
		}
	}
	' >"${SLSA_PREDICATE}"

jq -n \
	--arg chart_name "${CHART_NAME}" \
	--arg chart_ref "oci://${CHART_REF}" \
	--arg chart_sha256 "${CHART_SHA256}" \
	--arg chart_version "${CHART_VERSION}" \
	--arg created_on "${CREATED_ON}" \
	--arg document_namespace "${DOCUMENT_NAMESPACE}" \
	'
	{
		spdxVersion: "SPDX-2.3",
		dataLicense: "CC0-1.0",
		SPDXID: "SPDXRef-DOCUMENT",
		name: ($chart_name + "-" + $chart_version + " Helm chart SBOM"),
		documentNamespace: $document_namespace,
		documentDescribes: ["SPDXRef-Package-helm-chart"],
		creationInfo: {
			created: $created_on,
			creators: [
				"Organization: Kubernetes Authors",
				"Tool: hack/generate-helm-chart-attestation-predicates.sh"
			]
		},
		packages: [
			{
				name: $chart_name,
				SPDXID: "SPDXRef-Package-helm-chart",
				versionInfo: $chart_version,
				downloadLocation: $chart_ref,
				filesAnalyzed: false,
				checksums: [
					{algorithm: "SHA256", checksumValue: $chart_sha256}
				],
				licenseConcluded: "Apache-2.0",
				licenseDeclared: "Apache-2.0",
				copyrightText: "The Kubernetes Authors"
			}
		]
	}
	' >"${SBOM_PREDICATE}"

echo "Wrote ${SLSA_PREDICATE}"
echo "Wrote ${SBOM_PREDICATE}"
