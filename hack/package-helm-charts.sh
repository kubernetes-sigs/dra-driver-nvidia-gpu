#!/usr/bin/env bash

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

# Packages the Helm chart for release or for Prow / Cloud Build jobs that push
# OCI charts to staging/promotion registries.

set -euo pipefail

REPO_ROOT=$(git rev-parse --show-toplevel)
cd "${REPO_ROOT}"

# if arg1 is set, it will be used as the version number
if [ -z "${1:-}" ]; then
  VERSION=$(make --no-print-directory -f "${REPO_ROOT}/versions.mk" print-VERSION)
else
  VERSION=$1
fi
# Remove any v prefix, if exists.
VERSION="${VERSION#v}"


# Note(JP): the goal below is for VERSION to always be
# strictly semver-compliant (parseable with a semver
# parser). That enables best compatibility with the Helm
# ecosystem. For example, that means that no `v` prefix
# should be used here.

# Create release assets to be uploaded
helm package "${REPO_ROOT}/deployments/helm/dra-driver-nvidia-gpu/" --version "${VERSION}" --app-version "${VERSION}"
