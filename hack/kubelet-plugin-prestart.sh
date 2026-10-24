#!/usr/bin/env bash

# Main intent: help users to self-troubleshoot when the GPU driver is not set up
# properly before installing this DRA driver. In that case, the log of the init
# container running this script is meant to yield an actionable error message.
# For now, rely on k8s to implement a high-level retry with back-off.

if [ -z "$NVIDIA_DRIVER_ROOT" ]; then
    # Not set, or set to empty string (not distinguishable).
    # Normalize to "/" (treated as such elsewhere).
    export NVIDIA_DRIVER_ROOT="/"
fi

# Remove trailing slash (if existing) and get last path element.
_driver_root_path="/driver-root-parent/$(basename "${NVIDIA_DRIVER_ROOT%/}")"

# Create in-container path /driver-root as a symlink. Expectation: link may be
# broken initially (e.g., if the GPU operator isn't deployed yet. The link heals
# once the driver becomes mounted (e.g., once GPU operator provides the driver
# on the host at /run/nvidia/driver).
echo "create symlink: /driver-root -> ${_driver_root_path}"
ln -s "${_driver_root_path}" /driver-root

emit_common_err () {
    printf '%b' \
        "Check failed. Has the NVIDIA GPU driver been set up? " \
        "It is expected to be installed under " \
        "NVIDIA_DRIVER_ROOT (currently set to '${NVIDIA_DRIVER_ROOT}') " \
        "in the host filesystem. If that path appears to be unexpected: " \
        "review the DRA driver's 'nvidiaDriverRoot' Helm chart variable. " \
        "Otherwise, review if the GPU driver has " \
        "actually been installed under that path.\n"
}

# Check if all GPUs on the node are bound to the vfio-pci driver (prepared
# for passthrough to e.g. KubeVirt/Kata workloads). Plain nvidia-smi fails on
# such nodes even with a healthy NVIDIA driver: NVML doesn't see
# vfio-pci-bound devices. We look for PCI devices with NVIDIA vendor ID
# (0x10de) and display class (0x03xx: VGA/3D controllers) in the host sysfs;
# this container is privileged, so it sees the host tree. Zero visible GPUs
# also counts as "all bound to vfio-pci" here: it keeps the weaker check for
# GPU-less test setups, as before. The sysfs devices dir can be overridden
# via $1 (used by tests).
all_gpus_on_vfio_pci () {
    _pci_devices="${1:-/sys/bus/pci/devices}"
    [ -d "${_pci_devices}" ] || return 0

    for _dev in "${_pci_devices}"/*; do
        [ -f "${_dev}/vendor" ] || continue
        [ "$(< "${_dev}/vendor")" = "0x10de" ] || continue
        case "$(< "${_dev}/class")" in
            0x03*) ;;
            *) continue ;;
        esac
        if [ "$(basename "$(readlink "${_dev}/driver")")" != "vfio-pci" ]; then
            return 1
        fi
    done

    return 0
}

validate_and_exit_on_success () {
    echo -n "$(date -u +"%Y-%m-%dT%H:%M:%SZ")  /driver-root (${NVIDIA_DRIVER_ROOT} on host): "

    # Search specific set of directories (not recursively: not required, and
    # /driver-root may be a big tree). Limit to first result (multiple results
    # are a bit of a pathological state, but continue with validation logic).
    # Suppress find stderr: some search directories are expected to be "not
    # found".

    NV_PATH=$( \
        find \
            /driver-root/opt/bin \
            /driver-root/usr/bin \
            /driver-root/usr/sbin \
            /driver-root/bin \
            /driver-root/sbin \
        -maxdepth 1 -type f -name "nvidia-smi" 2> /dev/null | head -n1
    )

    # Follow symlinks (-L), because `libnvidia-ml.so.1` is typically a link.
    # maxdepth 1 also protects against any potential symlink loop (we're
    # suppressing find's stderr, so we'd never see messages like 'Too many
    # levels of symbolic links').
    NV_LIB_PATH=$( \
        find -L \
            /driver-root/usr/lib64 \
            /driver-root/usr/lib/x86_64-linux-gnu \
            /driver-root/usr/lib/aarch64-linux-gnu \
            /driver-root/lib64 \
            /driver-root/lib/x86_64-linux-gnu \
            /driver-root/lib/aarch64-linux-gnu \
        -maxdepth 1 -type f -name "libnvidia-ml.so.1" 2> /dev/null | head -n1
    )

    if [ -z "${NV_PATH}" ]; then
        echo -n "nvidia-smi: not found, "
    else
        echo -n "nvidia-smi: '${NV_PATH}', "
    fi

    if [ -z "${NV_LIB_PATH}" ]; then
        echo -n "libnvidia-ml.so.1: not found, "
    else
        echo -n "libnvidia-ml.so.1: '${NV_LIB_PATH}', "
    fi

    # Log top-level entries in /driver-root (this may be valuable debug info).
    echo "current contents: [$(ls -1xAw0 /driver-root 2>/dev/null)]."

    if [ -n "${NV_PATH}" ] && [ -n "${NV_LIB_PATH}" ]; then
        # Run with clean environment (only LD_PRELOAD; nvidia-smi has only this
        # dependency). Emit message before invocation (nvidia-smi may be slow or
        # hang).
        # At this point, we only want to check if the nvidia driver is installed
        # correctly. On nodes where all GPUs are on the vfio-pci driver, run
        # 'nvidia-smi --version' instead: NVML can't reach vfio-bound GPUs, so
        # plain nvidia-smi would fail there with a healthy driver. Everywhere
        # else plain nvidia-smi is the stronger check: it also proves the
        # driver actually talks to the GPUs.
        NV_SMI_ARGS=""
        if all_gpus_on_vfio_pci; then
            echo "all GPUs on the node (if any) are on vfio-pci: skip GPU init"
            NV_SMI_ARGS="--version"
        fi
        echo "invoke: env -i LD_PRELOAD=${NV_LIB_PATH} ${NV_PATH} ${NV_SMI_ARGS}"

        # Always show stderr, maybe hide or filter stdout?
        env -i LD_PRELOAD="${NV_LIB_PATH}" "${NV_PATH}" ${NV_SMI_ARGS}
        RCODE="$?"

        # For checking GPU driver health: rely on nvidia-smi's exit code. Rely
        # on code 0 signaling that the driver is properly set up. See section
        # 'RETURN VALUE' in the nvidia-smi man page for meaning of error codes.
        if [ ${RCODE} -eq 0 ]; then
            echo "nvidia-smi returned with code 0: success, leave"

            # nvidia-smi may have just created the /dev/nvidia* device nodes
            # They live in this container's private /dev and die with the
            # init container; consumers like CDI device-node injection need
            # them in the host's /dev, mounted at /host-dev here. Copy them
            # over. Best-effort: if no nodes exist (e.g. all GPUs on
            # vfio-pci) there's simply nothing to copy.
            if [ -d /host-dev ] && ls /dev/nvidia* > /dev/null 2>&1; then
                if cp -a /dev/nvidia* /host-dev/ 2>/dev/null; then
                    echo "replicated GPU device nodes into host /dev (mounted at /host-dev)"
                else
                    echo "warning: failed to replicate GPU device nodes into host /dev"
                fi
            fi

            # Exit script indicating success (leave init container).
            exit 0
        fi
        echo "exit code: ${RCODE}"
    fi

    # Reduce log volume: log hints only every Nth attempt.
    if [ $((_ATTEMPT % 6)) -ne 0 ]; then
        return
    fi

    # nvidia-smi binaries not found, or execution failed. First, provide generic
    # error message. Then, try to provide actionable hints for common problems.
    echo
    emit_common_err

    # For host-provided driver not at / provide feedback for two special cases.
    if [ "${NVIDIA_DRIVER_ROOT}" != "/" ]; then
        if [ -z "$( ls -A /driver-root )" ]; then
            echo "Hint: Directory $NVIDIA_DRIVER_ROOT on the host is empty"
        else
            # Not empty, but at least one of the binaries not found: this is a
            # rather pathological state.
            if [ -z "${NV_PATH}" ] || [ -z "${NV_LIB_PATH}" ]; then
                echo "Hint: Directory $NVIDIA_DRIVER_ROOT is not empty but at least one of the binaries wasn't found."
            fi
        fi
    fi

    # Common mistake: driver container, but forgot `--set nvidiaDriverRoot`
    if [ "${NVIDIA_DRIVER_ROOT}" == "/" ] && [ -f /driver-root/run/nvidia/driver/usr/bin/nvidia-smi ]; then
        printf '%b' \
        "Hint: '/run/nvidia/driver/usr/bin/nvidia-smi' exists on the host, you " \
        "may want to re-install the DRA driver Helm chart with " \
        "--set nvidiaDriverRoot=/run/nvidia/driver\n"
    fi

    if [ "${NVIDIA_DRIVER_ROOT}" == "/run/nvidia/driver" ]; then
        printf '%b' \
            "Hint: NVIDIA_DRIVER_ROOT is set to '/run/nvidia/driver' " \
            "which typically means that the NVIDIA GPU Operator " \
            "manages the GPU driver. Make sure that the GPU Operator " \
            "is deployed and healthy.\n"
    fi
    echo
}

# DS pods may get deleted (terminated with SIGTERM) and re-created when the GPU
# Operator driver container creates a mount at /run/nvidia. Make that explicit.
log_sigterm() {
  echo "$(date -u +"%Y-%m-%dT%H:%M:%S.%3NZ"): received SIGTERM"
  exit 0
}
trap 'log_sigterm' SIGTERM


# Design goal: long-running init container that retries at constant frequency,
# and leaves only upon success (with code 0).
_WAIT_S=10
_ATTEMPT=0

while true
do
    validate_and_exit_on_success
    sleep ${_WAIT_S}
    _ATTEMPT=$((_ATTEMPT+1))
done
