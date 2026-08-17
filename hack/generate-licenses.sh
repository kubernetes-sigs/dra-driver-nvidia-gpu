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

set -euo pipefail

REPO_ROOT=$(git rev-parse --show-toplevel)
MODULE_PATH=$(sed -n 's/^module[[:space:]]\+\(.*\)$/\1/p' "${REPO_ROOT}/go.mod")

if [[ -z "${MODULE_PATH}" ]]; then
    echo "Failed to determine module path from go.mod" >&2
    exit 1
fi

GO_LICENSES_BIN=${GO_LICENSES_BIN:-}
if [[ -z "${GO_LICENSES_BIN}" ]] && command -v go-licenses >/dev/null 2>&1; then
    GO_LICENSES_BIN=$(command -v go-licenses)
fi
if [[ -z "${GO_LICENSES_BIN}" ]]; then
    GOBIN_PATH=$(go env GOBIN)
    GOPATH_PATH=$(go env GOPATH)
    if [[ -n "${GOBIN_PATH}" && -x "${GOBIN_PATH}/go-licenses" ]]; then
        GO_LICENSES_BIN="${GOBIN_PATH}/go-licenses"
    elif [[ -n "${GOPATH_PATH}" && -x "${GOPATH_PATH}/bin/go-licenses" ]]; then
        GO_LICENSES_BIN="${GOPATH_PATH}/bin/go-licenses"
    fi
fi
if [[ -z "${GO_LICENSES_BIN}" ]]; then
    echo "go-licenses is required but was not found in PATH, GOBIN, or GOPATH/bin" >&2
    echo "Install it with: go install github.com/google/go-licenses/v2@latest" >&2
    exit 1
fi

OUTPUT_DIR=${1:-"${REPO_ROOT}/LICENSES"}
PACKAGE_PATTERN=${2:-./...}

SUMMARY_FILE="${OUTPUT_DIR}/summary.csv"
ERRORS_FILE="${OUTPUT_DIR}/errors.log"
ARTIFACTS_DIR="${OUTPUT_DIR}/artifacts"

rm -rf "${OUTPUT_DIR}"
mkdir -p "${OUTPUT_DIR}"

pushd "${REPO_ROOT}" >/dev/null

# Force module resolution from go.mod/go.sum instead of the vendored tree.
export GOFLAGS="${GOFLAGS:-} -mod=mod"
export CGO_ENABLED="${CGO_ENABLED:-1}"

if [[ "${CGO_ENABLED}" != "1" ]]; then
    echo "CGO_ENABLED must be 1 for go-licenses to load CGO-backed packages in this module" >&2
    exit 1
fi

echo "Generating license report for ${MODULE_PATH} (${PACKAGE_PATTERN})"

if ! "${GO_LICENSES_BIN}" report "${PACKAGE_PATTERN}" >"${SUMMARY_FILE}" 2>"${ERRORS_FILE}"; then
    if grep -q "build constraints exclude all Go files" "${ERRORS_FILE}"; then
        cat >&2 <<'EOF'
go-licenses report failed because at least one dependency package was excluded by build constraints.
This module depends on CGO-backed packages, so the container must provide:
  - CGO_ENABLED=1
  - a working C compiler such as gcc or cc
  - the matching target GOOS/GOARCH for the packages being analyzed
See LICENSES/errors.log for the exact package that failed.
EOF
    fi
    exit 1
fi

"${GO_LICENSES_BIN}" save "${PACKAGE_PATTERN}" --save_path="${ARTIFACTS_DIR}" >>"${ERRORS_FILE}" 2>&1

popd >/dev/null

echo "Wrote ${SUMMARY_FILE}"
echo "Wrote ${ARTIFACTS_DIR}"
echo "Wrote ${ERRORS_FILE}"
