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

# setup-mock-gpu.sh -- Install mock NVML on a CPU-only Linux host.
#
# Transforms a bare-metal machine into one where nvidia-smi reports fake GPUs.
# Designed for CI (Prow, GitHub Actions) on CPU-only nodes.
#
# Usage:
#   GPU_PROFILE=gb200 GPU_COUNT=8 ./setup-mock-gpu.sh
#
# Required:
#   - Root/sudo access (for mknod, library installation)
#   - Go 1.25+ and gcc (or Docker for cross-build)
#   - git clone of github.com/nvidia/k8s-test-infra at K8S_TEST_INFRA_DIR
#
# Environment:
#   GPU_PROFILE       GPU profile: a100, h100, b200, gb200, l40s, t4 (default: gb200)
#   GPU_COUNT         Number of GPUs to emulate, max 8 (default: 8)
#   K8S_TEST_INFRA_DIR  Path to k8s-test-infra checkout (default: auto-detect via GOPATH)
#   DRIVER_ROOT       Where to install mock driver (default: /var/lib/nvml-mock/driver)
#   MOCK_SYSFS_ROOT   Where to render mock PCI/NUMA sysfs (default: $DRIVER_ROOT/sys)
#   SKIP_NVIDIA_SMI   Set to "true" to skip nvidia-smi binary extraction (default: false)
set -o errexit
set -o nounset
set -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=hack/ci/mock-nvml/common.sh
source "${SCRIPT_DIR}/common.sh"
SKIP_NVIDIA_SMI="${SKIP_NVIDIA_SMI:-false}"

echo "=== Mock GPU Setup ==="
echo "Profile:    ${GPU_PROFILE}"
echo "GPU count:  ${GPU_COUNT}"
echo "Driver ver: ${DRIVER_VERSION}"
echo "Test-infra: ${K8S_TEST_INFRA_DIR}"
echo "Driver root: ${DRIVER_ROOT}"

# --- Validate inputs ---
if [ "${GPU_COUNT}" -gt 8 ]; then
  echo "ERROR: GPU_COUNT cannot exceed 8 (mock NVML maximum)" >&2
  exit 1
fi

if [ ! -d "${K8S_TEST_INFRA_DIR}/pkg/gpu/mocknvml" ]; then
  echo "ERROR: k8s-test-infra not found at ${K8S_TEST_INFRA_DIR}" >&2
  echo "Clone it: git clone https://github.com/nvidia/k8s-test-infra.git \$(go env GOPATH)/src/github.com/nvidia/k8s-test-infra" >&2
  exit 1
fi

PROFILE_YAML="${K8S_TEST_INFRA_DIR}/pkg/gpu/mocknvml/configs/mock-nvml-config-${GPU_PROFILE}.yaml"
if [ ! -f "${PROFILE_YAML}" ]; then
  echo "ERROR: Profile config not found: ${PROFILE_YAML}" >&2
  echo "Available profiles: $(ls "${K8S_TEST_INFRA_DIR}/pkg/gpu/mocknvml/configs/" | sed 's/mock-nvml-config-//;s/\.yaml//' | tr '\n' ' ')" >&2
  exit 1
fi

render_mock_pci_numa_sysfs() {
  local config_path="$1"
  local sysfs_root="${MOCK_SYSFS_ROOT:-${DRIVER_ROOT}/sys}"

  echo ""
  echo "--- Rendering mock PCI NUMA sysfs ---"
  echo "Sysfs root: ${sysfs_root}"

  sudo mkdir -p "${sysfs_root}/bus/pci/devices"

  local max_numa=-1
  local index=0
  local bdf
  local numa
  local root_complex
  while read -r bdf; do
    [ -n "${bdf}" ] || continue
    numa=$((index / 2))
    root_complex=$(printf "pci0000:%02x" $((numa * 64)))

    sudo mkdir -p "${sysfs_root}/devices/${root_complex}/${bdf}"
    printf "%s\n" "${numa}" | sudo tee "${sysfs_root}/devices/${root_complex}/${bdf}/numa_node" > /dev/null

    sudo rm -f "${sysfs_root}/bus/pci/devices/${bdf}"
    sudo ln -s "../../../devices/${root_complex}/${bdf}" "${sysfs_root}/bus/pci/devices/${bdf}"
    echo "  ${bdf} -> NUMA node ${numa} (${root_complex})"

    max_numa="${numa}"
    index=$((index + 1))
  done < <(awk '/^[[:space:]]+bus_id:/ { gsub(/"/, "", $2); print tolower($2) }' "${config_path}")

  if [ "${max_numa}" -lt 0 ]; then
    echo "WARNING: no PCI bus IDs found in ${config_path}; mock NUMA sysfs not rendered"
    return 0
  fi

  render_mock_numa_slit "${sysfs_root}" "${max_numa}"
}

render_mock_numa_slit() {
  local sysfs_root="$1"
  local max_numa="$2"

  # Render enough NUMA nodes for every physical node to have a same-socket
  # partner. The DRA helper reads nodeX/distance and picks nodes at the minimum
  # non-self SLIT distance, filtered by physical_package_id when available.
  local node_count=$(( (max_numa / 2 + 1) * 2 ))
  local node_dir
  local cpu_dir
  local row
  local distance
  local i
  local j

  sudo mkdir -p "${sysfs_root}/devices/system/node" "${sysfs_root}/devices/system/cpu"
  if [ "${node_count}" -eq 1 ]; then
    printf "0\n" | sudo tee "${sysfs_root}/devices/system/node/online" > /dev/null
  else
    printf "0-%d\n" $((node_count - 1)) | sudo tee "${sysfs_root}/devices/system/node/online" > /dev/null
  fi

  for i in $(seq 0 $((node_count - 1))); do
    node_dir="${sysfs_root}/devices/system/node/node${i}"
    cpu_dir="${sysfs_root}/devices/system/cpu/cpu${i}/topology"
    sudo mkdir -p "${node_dir}" "${cpu_dir}"

    printf "%d\n" "${i}" | sudo tee "${node_dir}/cpulist" > /dev/null
    printf "%d\n" $((i / 2)) | sudo tee "${cpu_dir}/physical_package_id" > /dev/null

    row=""
    for j in $(seq 0 $((node_count - 1))); do
      if [ "${i}" -eq "${j}" ]; then
        distance=10
      elif [ $((i / 2)) -eq $((j / 2)) ]; then
        distance=12
      else
        distance=20
      fi
      row="${row}${row:+ }${distance}"
    done
    printf "%s\n" "${row}" | sudo tee "${node_dir}/distance" > /dev/null
  done

  echo "Rendered NUMA SLIT with ${node_count} node(s); same-socket distance=12, cross-socket distance=20"
}

# --- Step 1: Build mock NVML library ---
echo ""
echo "--- Step 1: Building mock NVML library ---"
MOCKNVML_DIR="${K8S_TEST_INFRA_DIR}/pkg/gpu/mocknvml"
BUILT_LIB="${MOCKNVML_DIR}/libnvidia-ml.so.${DRIVER_VERSION}"

# XID events and NVML_FI_DEV_GET_GPU_RECOVERY_ACTION are patched in before the
# library is built. Drop any earlier artifact so this checkout is recompiled.
echo "--- Applying GPU health-control support ---"
bash "${SCRIPT_DIR}/apply-health-control.sh" "${MOCKNVML_DIR}"
rm -f "${MOCKNVML_DIR}"/libnvidia-ml.so.*

# Check if already built with correct version
if [ -f "${BUILT_LIB}" ]; then
  echo "Mock NVML library already built: ${BUILT_LIB}"
else
  # Build the library. The Makefile version may differ from DRIVER_VERSION;
  # we rename after build.
  pushd "${MOCKNVML_DIR}" > /dev/null
  make clean
  if command -v go > /dev/null 2>&1 && [ "$(uname)" = "Linux" ]; then
    echo "Building natively..."
    make
  else
    echo "Building via Docker (non-Linux or no Go)..."
    make docker-build
  fi

  # Rename if Makefile version differs from target
  MAKEFILE_LIB=$(ls libnvidia-ml.so.*.*.* 2>/dev/null | head -1)
  if [ -n "${MAKEFILE_LIB}" ] && [ "${MAKEFILE_LIB}" != "libnvidia-ml.so.${DRIVER_VERSION}" ]; then
    cp "${MAKEFILE_LIB}" "libnvidia-ml.so.${DRIVER_VERSION}"
  fi
  popd > /dev/null
  echo "Built: ${BUILT_LIB}"
fi

# --- Step 2: Create directory structure ---
echo ""
echo "--- Step 2: Creating directory structure ---"
sudo mkdir -p "${DRIVER_ROOT}/usr/lib64" "${DRIVER_ROOT}/usr/bin" "${DRIVER_ROOT}/config"
sudo mkdir -p "${DRIVER_ROOT}/dev" "${DEV_ROOT}"

# --- Step 3: Install mock NVML library ---
echo ""
echo "--- Step 3: Installing mock NVML library ---"
sudo cp "${BUILT_LIB}" "${DRIVER_ROOT}/usr/lib64/libnvidia-ml.so.${DRIVER_VERSION}"
sudo ln -sf "libnvidia-ml.so.${DRIVER_VERSION}" "${DRIVER_ROOT}/usr/lib64/libnvidia-ml.so.1"
sudo ln -sf "libnvidia-ml.so.1" "${DRIVER_ROOT}/usr/lib64/libnvidia-ml.so"

echo "Installed: ${DRIVER_ROOT}/usr/lib64/libnvidia-ml.so.${DRIVER_VERSION}"
ls -la "${DRIVER_ROOT}/usr/lib64/libnvidia-ml.so"*

# --- Step 4: Install GPU profile config ---
echo ""
echo "--- Step 4: Installing GPU profile config ---"
sudo cp "${PROFILE_YAML}" "${DRIVER_ROOT}/config/config.yaml"

# Inject num_devices (guard against duplicate key) and patch version strings.
if grep -q "num_devices:" "${DRIVER_ROOT}/config/config.yaml"; then
  sudo sed -i "s|num_devices:.*|num_devices: ${GPU_COUNT}|" "${DRIVER_ROOT}/config/config.yaml"
else
  sudo sed -i "/^system:/a\\  num_devices: ${GPU_COUNT}" "${DRIVER_ROOT}/config/config.yaml"
fi
sudo sed -i "s|driver_version:.*|driver_version: \"${DRIVER_VERSION}\"|" "${DRIVER_ROOT}/config/config.yaml"
sudo sed -i "s|nvml_version:.*|nvml_version: \"12.${DRIVER_VERSION}\"|" "${DRIVER_ROOT}/config/config.yaml"

echo "Installed config for ${GPU_PROFILE} with ${GPU_COUNT} devices, driver ${DRIVER_VERSION}"

# --- Step 4b: Render PCI sysfs with NUMA SLIT data ---
render_mock_pci_numa_sysfs "${DRIVER_ROOT}/config/config.yaml"

# --- Step 5: Extract and patch nvidia-smi ---
if [ "${SKIP_NVIDIA_SMI}" = "true" ]; then
  echo ""
  echo "--- Step 5: Skipping nvidia-smi (SKIP_NVIDIA_SMI=true) ---"
else
  echo ""
  echo "--- Step 5: Extracting and patching nvidia-smi ---"

  if [ -f "${DRIVER_ROOT}/usr/bin/nvidia-smi" ]; then
    echo "nvidia-smi already installed, skipping extraction"
  else
    # Install patchelf if needed
    if ! command -v patchelf > /dev/null 2>&1; then
      sudo apt-get update -qq
      sudo apt-get install -y -qq patchelf
    fi

    # Determine architecture for NVIDIA repo
    ARCH=$(dpkg --print-architecture 2>/dev/null || echo "amd64")
    case "${ARCH}" in
      arm64) NVIDIA_REPO_ARCH="sbsa" ;;
      *)     NVIDIA_REPO_ARCH="x86_64" ;;
    esac

    # Add NVIDIA repo and download nvidia-utils package
    curl -fsSL "https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2204/${NVIDIA_REPO_ARCH}/3bf863cc.pub" | \
      sudo gpg --batch --yes --dearmor -o /usr/share/keyrings/nvidia.gpg
    echo "deb [signed-by=/usr/share/keyrings/nvidia.gpg] https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2204/${NVIDIA_REPO_ARCH} /" | \
      sudo tee /etc/apt/sources.list.d/nvidia-mock.list > /dev/null
    sudo apt-get update -qq

    # Download (not install) nvidia-utils to extract nvidia-smi binary.
    # We use nvidia-utils-550 because newer meta-packages (570+) don't include
    # the nvidia-smi binary directly. The binary version (550.x) differs from
    # DRIVER_VERSION but this is fine: nvidia-smi reads the driver version from
    # the NVML library (our mock), not from its own binary version.
    WORKDIR=$(mktemp -d)
    pushd "${WORKDIR}" > /dev/null
    apt-get download nvidia-utils-550 2>/dev/null
    dpkg-deb -x nvidia-utils-550*.deb "${WORKDIR}/extract"
    sudo cp "${WORKDIR}/extract/usr/bin/nvidia-smi" "${DRIVER_ROOT}/usr/bin/nvidia-smi"
    sudo chmod +x "${DRIVER_ROOT}/usr/bin/nvidia-smi"
    popd > /dev/null
    rm -rf "${WORKDIR}"

    # Patch RPATH so nvidia-smi finds libnvidia-ml.so.1 at ../lib64 relative to itself.
    sudo patchelf --set-rpath '$ORIGIN/../lib64' "${DRIVER_ROOT}/usr/bin/nvidia-smi"

    # Clean up NVIDIA repo (don't leave it dangling)
    sudo rm -f /etc/apt/sources.list.d/nvidia-mock.list /usr/share/keyrings/nvidia.gpg

    echo "Installed nvidia-smi with RPATH=\$ORIGIN/../lib64"
  fi
fi

# --- Step 6: Create character device nodes ---
echo ""
echo "--- Step 6: Creating character device nodes ---"
for i in $(seq 0 $((GPU_COUNT - 1))); do
  sudo mknod -m 666 "${DEV_ROOT}/nvidia${i}" c 195 "${i}" 2>/dev/null || true
done
sudo mknod -m 666 "${DEV_ROOT}/nvidiactl" c 195 255 2>/dev/null || true
sudo mknod -m 666 "${DEV_ROOT}/nvidia-uvm" c 510 0 2>/dev/null || true
sudo mknod -m 666 "${DEV_ROOT}/nvidia-uvm-tools" c 510 1 2>/dev/null || true

# Also create device nodes inside driver root for CDI discovery.
# The DRA driver's getDevRoot() checks for <driverRoot>/dev/ and the
# nvidia-container-toolkit CharDeviceDiscoverer resolves paths from there.
for i in $(seq 0 $((GPU_COUNT - 1))); do
  sudo mknod -m 666 "${DRIVER_ROOT}/dev/nvidia${i}" c 195 "${i}" 2>/dev/null || true
done
sudo mknod -m 666 "${DRIVER_ROOT}/dev/nvidiactl" c 195 255 2>/dev/null || true
sudo mknod -m 666 "${DRIVER_ROOT}/dev/nvidia-uvm" c 510 0 2>/dev/null || true
sudo mknod -m 666 "${DRIVER_ROOT}/dev/nvidia-uvm-tools" c 510 1 2>/dev/null || true

echo "Created device nodes at ${DEV_ROOT}/ and ${DRIVER_ROOT}/dev/"

# --- Step 7: Create /proc/driver/nvidia mock files ---
echo ""
echo "--- Step 7: Creating /proc/driver/nvidia mock files ---"
PROC_DIR="${DRIVER_ROOT}/proc/driver/nvidia"
sudo mkdir -p "${PROC_DIR}"

sudo tee "${PROC_DIR}/version" > /dev/null << EOF
NVRM version: NVIDIA UNIX x86_64 Kernel Module  ${DRIVER_VERSION}  Thu Feb 20 23:41:34 UTC 2026
GCC version:  gcc version 12.2.0 (Debian 12.2.0-14)
EOF

sudo tee "${PROC_DIR}/params" > /dev/null << 'EOF'
EnableMSI: 1
NVreg_RegistryDwords:
NVreg_DeviceFileGID: 0
NVreg_DeviceFileMode: 438
NVreg_DeviceFileUID: 0
NVreg_ModifyDeviceFiles: 1
NVreg_PreserveVideoMemoryAllocations: 0
NVreg_EnableResizableBar: 0
EOF

# --- Step 7b: Create mock IMEX channel support ---
# The compute-domain-kubelet-plugin parses /proc/devices looking for
# "nvidia-caps-imex-channels" to get its device major number, then reads
# /proc/driver/nvidia/capabilities/fabric-imex-mgmt for the fabric management
# device info. We create both.
echo ""
echo "--- Step 7b: Creating mock IMEX channel support ---"
MOCK_IMEX="${MOCK_ROOT}/imex"
sudo mkdir -p "${MOCK_IMEX}"

# Pick a major number that doesn't conflict with real devices.
# 235 is typically unused on Linux.
IMEX_MAJOR=235

# Create a fake /proc/devices that includes nvidia-caps-imex-channels.
# We take the real /proc/devices content and append our mock entries.
# This file will be bind-mounted over /proc/devices inside Kind nodes.
{
  # Copy the real /proc/devices but strip the trailing "Block devices:" section
  sed '/^Block devices:/,$d' /proc/devices
  # Add our mock nvidia-caps entries
  echo "${IMEX_MAJOR} nvidia-caps-imex-channels"
  echo "236 nvidia-caps"
  echo ""
  # Re-add the Block devices section
  sed -n '/^Block devices:/,$p' /proc/devices
} | sudo tee "${MOCK_IMEX}/proc-devices" > /dev/null

# Create mock fabric-imex-mgmt capability file
CAPS_DIR="${PROC_DIR}/capabilities"
sudo mkdir -p "${CAPS_DIR}"
sudo tee "${CAPS_DIR}/fabric-imex-mgmt" > /dev/null << EOF
DeviceFileMinor: 512
DeviceFileMode: 438
DeviceFileModify: 0
EOF

# Create mock IMEX channel device nodes. The compute-domain CDI spec injects
# /dev/nvidia-caps-imex-channels/channel<N> into workload pods. These must
# exist as character devices so the CDI injection succeeds and workload pods
# can list them. The 2048 count matches getImexChannelCount() in
# cmd/compute-domain-kubelet-plugin/nvlib.go.
IMEX_CHAN_DIR="${DEV_ROOT}/nvidia-caps-imex-channels"
IMEX_CHAN_DRV_DIR="${DRIVER_ROOT}/dev/nvidia-caps-imex-channels"
sudo mkdir -p "${IMEX_CHAN_DIR}" "${IMEX_CHAN_DRV_DIR}"
echo "Creating 2048 mock IMEX channel device nodes..."
for i in $(seq 0 2047); do
  sudo mknod -m 666 "${IMEX_CHAN_DIR}/channel${i}" c "${IMEX_MAJOR}" "${i}" 2>/dev/null || true
  sudo mknod -m 666 "${IMEX_CHAN_DRV_DIR}/channel${i}" c "${IMEX_MAJOR}" "${i}" 2>/dev/null || true
done
echo "Created 2048 IMEX channel device nodes at ${IMEX_CHAN_DIR}/ and ${IMEX_CHAN_DRV_DIR}/"

echo "Created mock /proc/devices with nvidia-caps-imex-channels (major=${IMEX_MAJOR})"
echo "Created mock fabric-imex-mgmt capability file"

# --- Step 8: Create GPU Operator compatibility symlink ---
echo ""
echo "--- Step 8: Creating GPU Operator compatibility symlink ---"
sudo mkdir -p /run/nvidia
sudo ln -sfn "${DRIVER_ROOT}" /run/nvidia/driver
echo "Symlinked /run/nvidia/driver -> ${DRIVER_ROOT}"

# --- Step 9: Generate CDI spec ---
echo ""
echo "--- Step 9: Generating CDI spec ---"
CDI_DIR="/var/run/cdi"
sudo mkdir -p "${CDI_DIR}"

# Write CDI header (uses variables, not hardcoded paths)
sudo tee "${CDI_DIR}/nvidia-mock.yaml" > /dev/null << CDI_HEADER
cdiVersion: "0.6.0"
kind: "nvidia.com/gpu"
containerEdits:
  deviceNodes:
    - path: /dev/nvidiactl
      hostPath: ${DEV_ROOT}/nvidiactl
    - path: /dev/nvidia-uvm
      hostPath: ${DEV_ROOT}/nvidia-uvm
    - path: /dev/nvidia-uvm-tools
      hostPath: ${DEV_ROOT}/nvidia-uvm-tools
  mounts:
    - hostPath: ${DRIVER_ROOT}/usr/lib64/libnvidia-ml.so.1
      containerPath: /usr/lib64/libnvidia-ml.so.1
      options: [ro, nosuid, nodev, bind]
    - hostPath: ${DRIVER_ROOT}/usr/bin/nvidia-smi
      containerPath: /usr/bin/nvidia-smi
      options: [ro, nosuid, nodev, bind]
  hooks:
    - hookName: createContainer
      path: /usr/bin/nvidia-cdi-hook
      args: [nvidia-cdi-hook, update-ldcache, --folder, /usr/lib64]
  env:
    - NVIDIA_VISIBLE_DEVICES=void
devices:
CDI_HEADER

# Per-GPU device entries + "all" aggregate
for i in $(seq 0 $((GPU_COUNT - 1))); do
  sudo tee -a "${CDI_DIR}/nvidia-mock.yaml" > /dev/null << DEVICE_EOF
  - name: "${i}"
    containerEdits:
      deviceNodes:
        - path: /dev/nvidia${i}
          hostPath: ${DEV_ROOT}/nvidia${i}
DEVICE_EOF
done
{
  echo '  - name: "all"'
  echo '    containerEdits:'
  echo '      deviceNodes:'
  for i in $(seq 0 $((GPU_COUNT - 1))); do
    echo "        - path: /dev/nvidia${i}"
    echo "          hostPath: ${DEV_ROOT}/nvidia${i}"
  done
} | sudo tee -a "${CDI_DIR}/nvidia-mock.yaml" > /dev/null

echo "CDI spec generated at ${CDI_DIR}/nvidia-mock.yaml (${GPU_COUNT} devices)"

echo ""
echo "=== Mock GPU setup complete ==="
echo ""
echo "Verify with:"
echo "  ${DRIVER_ROOT}/usr/bin/nvidia-smi"
echo "  ${DRIVER_ROOT}/usr/bin/nvidia-smi -L"
echo "  MOCK_NVML_DEBUG=1 ${DRIVER_ROOT}/usr/bin/nvidia-smi 2>&1"
