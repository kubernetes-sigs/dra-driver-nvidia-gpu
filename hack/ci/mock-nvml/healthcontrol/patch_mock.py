#!/usr/bin/env python3
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

"""Patch a k8s-test-infra mocknvml tree with GPU health-control support.

The pinned mock can emit one failure-injector XID, but it hardcodes GI/CI to 0
and has no NVML_FI_DEV_GET_GPU_RECOVERY_ACTION. This copies the health-control
sources into that tree and wires them into event wait and field-value lookup.
"""

import pathlib
import re
import sys

FIELD_VALUE_TYPES = r"""/* DRA health-control field values. Layout matches nvmlFieldValue_t in
 * nvml.h so go-nvml can decode NVML_FI_DEV_GET_GPU_RECOVERY_ACTION.
 * nvmlValueType_t is already typedef'd to unsigned int above. */
typedef union nvmlValue_st
{
    double dVal;
    int siVal;
    unsigned int uiVal;
    unsigned long ulVal;
    unsigned long long ullVal;
    signed long long sllVal;
    unsigned short usVal;
} nvmlValue_t;

typedef struct nvmlFieldValue_st
{
    unsigned int fieldId;
    unsigned int scopeId;
    long long timestamp;
    long long latencyUsec;
    nvmlValueType_t valueType;
    nvmlReturn_t nvmlReturn;
    nvmlValue_t value;
} nvmlFieldValue_t;
"""

FIELD_VALUE_TYPEDEF = re.compile(
    r"^typedef struct nvmlFieldValue_st\s+nvmlFieldValue_t;\s*$",
    re.MULTILINE,
)

GET_FIELD_VALUES_STUB = """//export nvmlDeviceGetFieldValues
func nvmlDeviceGetFieldValues(device C.nvmlDevice_t, valuesCount C.int, values *C.nvmlFieldValue_t) C.nvmlReturn_t {
	return stubReturn("nvmlDeviceGetFieldValues")
}
"""

PENDING_XID_CALL = "\thandle, xid, ok := engine.GetEngine().PendingXidEvent()"
HEALTH_XID_CALL = """\tif handle, xid, gi, ci, ok := engine.GetEngine().ClaimHealthControlEvent(); ok {
\t\t// Health-control events carry the GPU, GI, and CI selected by the test.
\t\tdata.device.handle = (*C.struct_nvmlDevice_st)(unsafe.Pointer(handle))
\t\tdata.eventType = C.NVML_EVENT_TYPE_XID_CRITICAL_ERROR
\t\tdata.eventData = C.ulonglong(xid)
\t\tdata.gpuInstanceId = C.uint(gi)
\t\tdata.computeInstanceId = C.uint(ci)
\t\treturn true
\t}
\thandle, xid, ok := engine.GetEngine().PendingXidEvent()"""

FULL_GPU_INSTANCE = (
    "\tdata.gpuInstanceId = C.uint(0xFFFFFFFF)\n"
    "\tdata.computeInstanceId = C.uint(0xFFFFFFFF)\n"
)
ZERO_INSTANCE = "\tdata.gpuInstanceId = 0\n\tdata.computeInstanceId = 0\n"


def die(message):
    print(f"apply-health-control: {message}", file=sys.stderr)
    sys.exit(1)


def strip_build_ignore(text):
    lines = text.splitlines(keepends=True)
    output = []
    skipping_blank = False
    for line in lines:
        if line.startswith("//go:build ignore"):
            skipping_blank = True
            continue
        if skipping_blank and line.strip() == "":
            skipping_blank = False
            continue
        skipping_blank = False
        output.append(line)
    return "".join(output)


def write_if_changed(path, text):
    if path.is_file() and path.read_text() == text:
        print(f"unchanged {path}")
        return
    path.write_text(text)
    print(f"updated {path}")


def patch_header(header_path):
    text = header_path.read_text()
    if "typedef struct nvmlFieldValue_st {" in text or "typedef struct nvmlFieldValue_st\n{" in text:
        print(f"field value struct already present in {header_path}")
        return
    if "DRA health-control field values" in text:
        print(f"health-control field values already present in {header_path}")
        return
    if not FIELD_VALUE_TYPEDEF.search(text):
        die(f"incomplete nvmlFieldValue_t typedef not found in {header_path}")
    text = FIELD_VALUE_TYPEDEF.sub(FIELD_VALUE_TYPES.rstrip("\n"), text, count=1)
    write_if_changed(header_path, text)


def patch_events(events_path):
    text = events_path.read_text()
    if "ClaimHealthControlEvent" in text:
        print(f"event wait already claims health-control events in {events_path}")
        return
    if PENDING_XID_CALL not in text:
        die(f"PendingXidEvent call site not found in {events_path}")
    text = text.replace(PENDING_XID_CALL, HEALTH_XID_CALL, 1)
    if ZERO_INSTANCE not in text:
        die(f"hardcoded GI/CI assignment not found in {events_path}")
    text = text.replace(ZERO_INSTANCE, FULL_GPU_INSTANCE, 1)
    write_if_changed(events_path, text)


def remove_field_value_stub(stubs_path):
    text = stubs_path.read_text()
    if GET_FIELD_VALUES_STUB not in text:
        if "func nvmlDeviceGetFieldValues(" in text:
            die(
                f"{stubs_path} already defines nvmlDeviceGetFieldValues "
                "but it is not the expected stub"
            )
        print(f"GetFieldValues stub already removed from {stubs_path}")
        return
    write_if_changed(stubs_path, text.replace(GET_FIELD_VALUES_STUB + "\n", "", 1))


def copy_sources(healthcontrol_dir, mocknvml_dir):
    control = (healthcontrol_dir / "control.go").read_text()
    if "package healthcontrol\n" not in control:
        die("control.go is missing package healthcontrol")
    control = control.replace("package healthcontrol\n", "package engine\n", 1)
    control = control.replace(
        "// Package healthcontrol parses",
        "// health_control parses",
        1,
    )
    write_if_changed(mocknvml_dir / "engine" / "health_control.go", control)

    hooks = strip_build_ignore((healthcontrol_dir / "overlay" / "engine_hooks.go").read_text())
    if "//go:build ignore" in hooks:
        die("failed to strip build ignore tag from engine hooks")
    write_if_changed(mocknvml_dir / "engine" / "health_events.go", hooks)

    fields = strip_build_ignore((healthcontrol_dir / "overlay" / "fieldvalues.go").read_text())
    if "//go:build ignore" in fields:
        die("failed to strip build ignore tag from field values")
    write_if_changed(mocknvml_dir / "bridge" / "health_fieldvalues.go", fields)


def main():
    if len(sys.argv) != 3:
        die("usage: patch_mock.py <mocknvml-dir> <healthcontrol-dir>")
    mocknvml_dir = pathlib.Path(sys.argv[1])
    healthcontrol_dir = pathlib.Path(sys.argv[2])
    bridge = mocknvml_dir / "bridge"
    engine = mocknvml_dir / "engine"
    if not bridge.is_dir() or not engine.is_dir():
        die(f"mocknvml tree is incomplete: {mocknvml_dir}")

    patch_header(bridge / "nvml_types.h")
    remove_field_value_stub(bridge / "stubs_generated.go")
    copy_sources(healthcontrol_dir, mocknvml_dir)
    patch_events(bridge / "events.go")
    print("GPU health-control mock patch applied")


if __name__ == "__main__":
    main()
