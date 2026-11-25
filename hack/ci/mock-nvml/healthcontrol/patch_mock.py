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

"""Patch the pinned k8s-test-infra mocknvml tree with GPU health-control support.

The pinned mock can emit one failure-injector XID, but that path hardcodes
GI/CI to 0 and has no NVML_FI_DEV_GET_GPU_RECOVERY_ACTION. This copies the
health-control sources into that tree and wires them into event wait and
field-value lookup. The pin already exports nvmlDeviceGetFieldValues and
compares PCI bus IDs through canonicalPCIBusID, so this script does not patch
those.
"""

import pathlib
import sys

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

# The pinned failure-injector path reports GI/CI 0, which names MIG instance 0
# rather than the whole GPU. A later mock reports the full-GPU sentinel itself,
# either as 0xFFFFFFFF or nvmlNoMIGInstanceID. Leave that assignment alone so a
# pin bump does not fail the job on a missing "= 0".
ZERO_INSTANCE = "\tdata.gpuInstanceId = 0\n\tdata.computeInstanceId = 0\n"
FULL_GPU_INSTANCE = (
    "\tdata.gpuInstanceId = C.uint(0xFFFFFFFF)\n"
    "\tdata.computeInstanceId = C.uint(0xFFFFFFFF)\n"
)
FULL_GPU_SENTINELS = (
    FULL_GPU_INSTANCE,
    "\tdata.gpuInstanceId = C.uint(nvmlNoMIGInstanceID)\n"
    "\tdata.computeInstanceId = C.uint(nvmlNoMIGInstanceID)\n",
)


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


def patch_events(events_path):
    text = events_path.read_text()
    if "ClaimHealthControlEvent" in text:
        print(f"event wait already claims health-control events in {events_path}")
        return
    if PENDING_XID_CLAIM not in text:
        die(f"pendingXidClaim call site not found in {events_path}")
    text = text.replace(PENDING_XID_CLAIM, HEALTH_XID_CLAIM, 1)
    if ZERO_INSTANCE in text:
        text = text.replace(ZERO_INSTANCE, FULL_GPU_INSTANCE, 1)
    elif any(sentinel in text for sentinel in FULL_GPU_SENTINELS):
        print(f"failure-injector XID already uses the full-GPU GI/CI sentinel in {events_path}")
    else:
        die(f"hardcoded GI/CI assignment not found in {events_path}")
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


def main():
    if len(sys.argv) != 3:
        die("usage: patch_mock.py <mocknvml-dir> <healthcontrol-dir>")
    mocknvml_dir = pathlib.Path(sys.argv[1])
    healthcontrol_dir = pathlib.Path(sys.argv[2])
    bridge = mocknvml_dir / "bridge"
    engine = mocknvml_dir / "engine"
    if not bridge.is_dir() or not engine.is_dir():
        die(f"mocknvml tree is incomplete: {mocknvml_dir}")

    copy_sources(healthcontrol_dir, mocknvml_dir)
    patch_events(bridge / "events.go")
    patch_recovery_action_field(engine / "field_values.go")
    print("GPU health-control mock patch applied")


if __name__ == "__main__":
    main()
