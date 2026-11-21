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

# apply-health-control.sh -- Teach a k8s-test-infra mocknvml checkout how to
# emit configurable XID events and GPU recovery actions.
#
# Usage:
#   apply-health-control.sh /path/to/k8s-test-infra/pkg/gpu/mocknvml

set -o errexit
set -o nounset
set -o pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: apply-health-control.sh <mocknvml-dir>" >&2
  exit 1
fi

MOCKNVML_DIR="$1"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HEALTHCONTROL_DIR="${SCRIPT_DIR}/healthcontrol"

if ! command -v python3 > /dev/null 2>&1; then
  echo "ERROR: python3 is required to patch mock NVML" >&2
  exit 1
fi

python3 "${HEALTHCONTROL_DIR}/patch_mock.py" "${MOCKNVML_DIR}" "${HEALTHCONTROL_DIR}"
