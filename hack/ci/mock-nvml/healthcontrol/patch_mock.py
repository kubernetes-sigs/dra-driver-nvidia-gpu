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

# k8s-test-infra 18befbf replaced the direct PendingXidEvent call with
# pendingXidClaim. Health-control events are claimed first so a test can
# choose the GPU, GI, and CI. The failure-injector path below still runs
# when the control file has nothing to deliver.
PENDING_XID_CLAIM = "\thandle, xid, ok := pendingXidClaim()"
HEALTH_XID_CLAIM = """\tif handle, xid, gi, ci, ok := engine.GetEngine().ClaimHealthControlEvent(); ok {
\t\tdata.device.handle = (*C.struct_nvmlDevice_st)(handle)
\t\tdata.eventType = C.NVML_EVENT_TYPE_XID_CRITICAL_ERROR
\t\tdata.eventData = C.ulonglong(xid)
\t\tdata.gpuInstanceId = C.uint(gi)
\t\tdata.computeInstanceId = C.uint(ci)
\t\treturn true
\t}
\thandle, xid, ok := pendingXidClaim()"""

GET_FIELD_VALUE = """func (d *ConfigurableDevice) GetFieldValue(fieldID, scopeID uint32) (FieldValueType, uint64, nvml.Return) {
	if vt, val, ret, handled := d.getDeviceFieldValue(fieldID, scopeID); handled {"""
GET_FIELD_VALUE_HOOKED = """func (d *ConfigurableDevice) GetFieldValue(fieldID, scopeID uint32) (FieldValueType, uint64, nvml.Return) {
	if action, ok := d.healthRecoveryField(fieldID); ok {
		return FieldValueUint, uint64(action), nvml.SUCCESS
	}
	if vt, val, ret, handled := d.getDeviceFieldValue(fieldID, scopeID); handled {"""

FULL_GPU_INSTANCE = (
    "\tdata.gpuInstanceId = C.uint(0xFFFFFFFF)\n"
    "\tdata.computeInstanceId = C.uint(0xFFFFFFFF)\n"
)
ZERO_INSTANCE = "\tdata.gpuInstanceId = 0\n\tdata.computeInstanceId = 0\n"

# go-nvlib lowercases the bus ID from NVML before the driver looks the GPU up
# again. The GB200 profile stores uppercase hex (0000:0A:00.0). Real NVML
# parses the ID as hex, so the mock has to accept either case or every lookup
# fails and XIDs are classified as fatal.
PCI_BUS_ID_COMPARE = "if dev != nil && dev.PciBusID == pciBusId {"
PCI_BUS_ID_COMPARE_FOLDED = "if dev != nil && strings.EqualFold(dev.PciBusID, pciBusId) {"
STRINGS_IMPORT = '\t"strings"\n'


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
    if PENDING_XID_CLAIM not in text:
        die(f"pendingXidClaim call site not found in {events_path}")
    text = text.replace(PENDING_XID_CLAIM, HEALTH_XID_CLAIM, 1)
    if ZERO_INSTANCE not in text:
        die(f"hardcoded GI/CI assignment not found in {events_path}")
    text = text.replace(ZERO_INSTANCE, FULL_GPU_INSTANCE, 1)
    write_if_changed(events_path, text)


def patch_recovery_action_field(field_values_path):
    if not field_values_path.is_file():
        die(f"field value dispatch not found at {field_values_path}")
    text = field_values_path.read_text()
    if "healthRecoveryField" in text:
        print(f"recovery action field already dispatched in {field_values_path}")
        return
    if GET_FIELD_VALUE not in text:
        die(f"GetFieldValue dispatch not found in {field_values_path}")
    write_if_changed(field_values_path, text.replace(GET_FIELD_VALUE, GET_FIELD_VALUE_HOOKED, 1))


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


def patch_pci_bus_id_lookup(device_path):
    text = device_path.read_text()
    # 18befbf normalizes domain width and case in canonicalPCIBusID.
    if "canonicalPCIBusID(" in text or PCI_BUS_ID_COMPARE_FOLDED in text:
        print(f"PCI bus ID lookup already accepts either case in {device_path}")
        return
    if PCI_BUS_ID_COMPARE not in text:
        die(f"PCI bus ID comparison not found in {device_path}")
    if STRINGS_IMPORT not in text:
        slices_import = '\t"slices"\n'
        if slices_import not in text:
            die(f"cannot add strings import in {device_path}")
        text = text.replace(slices_import, slices_import + STRINGS_IMPORT, 1)
    text = text.replace(PCI_BUS_ID_COMPARE, PCI_BUS_ID_COMPARE_FOLDED, 1)
    write_if_changed(device_path, text)


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

    # The current mock already exports nvmlDeviceGetFieldValues. Installing the
    # overlay would be a second definition of that symbol. Recovery action is
    # hooked into GetFieldValue instead.
    dest = mocknvml_dir / "bridge" / "health_fieldvalues.go"
    if any(
        "func nvmlDeviceGetFieldValues(" in path.read_text()
        for path in (mocknvml_dir / "bridge").glob("*.go")
        if path != dest
    ):
        if dest.is_file():
            dest.unlink()
            print(f"removed {dest}; upstream already exports nvmlDeviceGetFieldValues")
        else:
            print("upstream already exports nvmlDeviceGetFieldValues")
        return
    fields = strip_build_ignore((healthcontrol_dir / "overlay" / "fieldvalues.go").read_text())
    if "//go:build ignore" in fields:
        die("failed to strip build ignore tag from field values")
    write_if_changed(dest, fields)


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
    patch_recovery_action_field(engine / "field_values.go")
    patch_pci_bus_id_lookup(engine / "device.go")
    print("GPU health-control mock patch applied")


if __name__ == "__main__":
    main()
