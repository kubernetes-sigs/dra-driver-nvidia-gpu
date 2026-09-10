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
set -euo pipefail

REPO_ROOT=$(git rev-parse --show-toplevel)
cd "$REPO_ROOT"

if [[ -z ${IMG_PREFIX:-} ]]; then
	echo "IMG_PREFIX is not set"
	exit 1
fi

if [[ -z ${IMG_TAG:-} ]]; then
	# Match versions.mk VERSION_W_COMMIT (e.g. v0.4.0-dev-f2eaddd6) for release
	# commits, use the exact git tag as the image tag.
	if git describe --exact-match --tags HEAD >/dev/null 2>&1; then
		IMG_TAG=$(git describe --exact-match --tags HEAD)
	else
		IMG_TAG=$(make --no-print-directory -f "${REPO_ROOT}/versions.mk" print-VERSION_W_COMMIT)
	fi
fi
echo "Using IMG_TAG=${IMG_TAG}"

if [[ -z ${GIT_COMMIT:-} ]]; then
	GIT_COMMIT=$(git rev-parse HEAD)
fi
echo "Using GIT_COMMIT=${GIT_COMMIT}"

IMG_PROVENANCE="${IMG_PROVENANCE:-false}"
IMG_SBOM="${IMG_SBOM:-false}"

export CI=true
export DOCKER_CLI_EXPERIMENTAL=enabled

# Register gcloud as a Docker credential helper.
# Required for "docker buildx build --push".
gcloud auth configure-docker us-central1-docker.pkg.dev --quiet

bash "${REPO_ROOT}/hack/init-buildx.sh"

make -f deployments/container/Makefile build \
	BUILD_MULTI_ARCH_IMAGES=true \
	PUSH_ON_BUILD=true \
	REGISTRY="${IMG_PREFIX}" \
	VERSION="${IMG_TAG}" \
	GIT_COMMIT="${GIT_COMMIT}" \
	PROVENANCE="${IMG_PROVENANCE}" \
	SBOM="${IMG_SBOM}"
