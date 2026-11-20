/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package common

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

// procDevicesPath and usingAltProcDevices are set at init time from the
// ALT_PROC_DEVICES_PATH env var. When set, the system is running against mock
// NVML on CPU-only nodes where /proc/devices lacks NVIDIA entries, and
// operations that depend on real kernel state should be skipped.
var (
	procDevicesPath     = "/proc/devices"
	usingAltProcDevices bool
)

// procDriverPath is the /proc/driver directory that NVIDIA capability files are
// read from, set at init time from the ALT_PROC_DRIVER_PATH env var. Mock NVML
// nodes have no kernel module serving /proc/driver/nvidia, and runc refuses
// mounts targeting /proc, so the capability tree is staged somewhere else.
var procDriverPath = "/proc/driver"

func init() {
	if v := os.Getenv("ALT_PROC_DEVICES_PATH"); v != "" {
		procDevicesPath = v
		usingAltProcDevices = true
	}
	if v := os.Getenv("ALT_PROC_DRIVER_PATH"); v != "" {
		procDriverPath = v
	}
}

const (
	devNvidiaCapsPath    = "/dev/nvidia-caps"
	nvidiaCapsDeviceName = "nvidia-caps"
)

// UsingAltProcDevices reports whether an alternative /proc/devices path is
// configured via ALT_PROC_DEVICES_PATH or ConfigureProcDevicesPath.
func UsingAltProcDevices() bool {
	return usingAltProcDevices
}

// ConfigureProcDevicesPath overrides the /proc/devices path at runtime.
// Use in tests to set different paths without relying on environment variables.
func ConfigureProcDevicesPath(path string) {
	procDevicesPath = path
	usingAltProcDevices = path != "/proc/devices"
}

// ResetProcDevicesPath restores the default /proc/devices path.
// Use in test cleanup to avoid leaking state between tests.
func ResetProcDevicesPath() {
	procDevicesPath = "/proc/devices"
	usingAltProcDevices = false
	if v := os.Getenv("ALT_PROC_DEVICES_PATH"); v != "" {
		procDevicesPath = v
		usingAltProcDevices = true
	}
}

// MigCapsPaths returns the GI and CI access capability files of a MIG device,
// under /proc/driver/nvidia/capabilities unless ALT_PROC_DRIVER_PATH overrides
// /proc/driver.
func MigCapsPaths(parentMinor, giID, ciID int) (giPath, ciPath string) {
	giDir := filepath.Join(procDriverPath, "nvidia", "capabilities",
		fmt.Sprintf("gpu%d", parentMinor), "mig", fmt.Sprintf("gi%d", giID))
	return filepath.Join(giDir, "access"), filepath.Join(giDir, fmt.Sprintf("ci%d", ciID), "access")
}

// ConfigureProcDriverPath overrides the /proc/driver path at runtime.
// Use in tests to set different paths without relying on environment variables.
func ConfigureProcDriverPath(path string) {
	procDriverPath = path
}

// ResetProcDriverPath restores the default /proc/driver path.
// Use in test cleanup to avoid leaking state between tests.
func ResetProcDriverPath() {
	procDriverPath = "/proc/driver"
	if v := os.Getenv("ALT_PROC_DRIVER_PATH"); v != "" {
		procDriverPath = v
	}
}

type NVcapDeviceInfo struct {
	Major  int
	Minor  int
	Mode   int
	Modify int
	Path   string
}

// Construct and return a CDI `deviceNodes` entry. Adding this to a CDI
// container specification at the high level has the purpose of granting cgroup
// access to the containerized application for being able to access (open) a
// certain character device node as identified by `i.path`. The special device
// type "c" below specifically instructs the container runtime to create (mknod)
// the character device (for the container, accessible from within the
// container, not visible on the host), and to grant the cgroup privilege to the
// container to open that device. References:
// https://github.com/opencontainers/runtime-spec/blob/main/config-linux.md#allowed-device-list
// https://www.kernel.org/doc/Documentation/cgroup-v1/devices.txt
func (i *NVcapDeviceInfo) CDICharDevNode() *cdispec.DeviceNode {
	return &cdispec.DeviceNode{
		Path:     i.Path,
		Type:     "c",
		FileMode: ptr.To(os.FileMode(i.Mode)),
		Major:    int64(i.Major),
		Minor:    int64(i.Minor),
	}
}

// A note: MIG minors are predictable, and can be looked up in
// `/proc/driver/nvidia-caps/mig-minors`:
//
// $ cat /proc/driver/nvidia-caps/mig-minors
// ...
// gpu0/gi0/access 3
// gpu0/gi0/ci0/access 4
// gpu0/gi0/ci1/access 5
// gpu0/gi0/ci2/access 6
// ...
// gpu3/gi3/ci3/access 439
// gpu3/gi3/ci4/access 440
// gpu3/gi3/ci5/access 441
// ...
// gpu6/gi11/ci5/access 918
// gpu6/gi11/ci6/access 919
// gpu6/gi11/ci7/access 920

// Parse a capabilities file under /proc/driver/nvidia/capabilities and return
// an NVcapDeviceInfo struct with its `Path` set to
// `/dev/nvidia-caps/nvidia-cap<MINOR>`.
//
// Examples for capability file paths:
//   - /proc/driver/nvidia/capabilities/fabric-imex-mgmt
//   - /proc/driver/nvidia/capabilities/gpu0/mig/gi5/access
func ParseNVCapDeviceInfo(nvcapsFilePath string) (*NVcapDeviceInfo, error) {

	file, err := os.Open(nvcapsFilePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	info := &NVcapDeviceInfo{}
	major, err := GetDeviceMajor(nvidiaCapsDeviceName)
	if err != nil {
		return nil, fmt.Errorf("error getting device major: %w", err)
	}

	klog.V(7).Infof("Got major for %s: %d", nvidiaCapsDeviceName, major)
	info.Major = major

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		switch key {
		case "DeviceFileMinor":
			_, _ = fmt.Sscanf(value, "%d", &info.Minor)
		case "DeviceFileMode":
			_, _ = fmt.Sscanf(value, "%d", &info.Mode)
		case "DeviceFileModify":
			_, _ = fmt.Sscanf(value, "%d", &info.Modify)
		}
	}
	info.Path = fmt.Sprintf("%s/nvidia-cap%d", devNvidiaCapsPath, info.Minor)

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading '%s': %w", nvcapsFilePath, err)
	}

	return info, nil
}

// GetDeviceMajor searches for one "<integer> <name>" occurrence in the
// "Character devices" section of the /proc/devices file, and returns the
// integer.
func GetDeviceMajor(name string) (int, error) {

	re := regexp.MustCompile(
		// The `(?s)` flag makes `.` match newlines. The greedy modifier in
		// `.*?` ensures to pick the first match after "Character devices".
		// Extract the number as capture group (the first and only group).
		"(?s)Character devices:.*?" +
			"([0-9]+) " + regexp.QuoteMeta(name) +
			// Require `name` to be newline-terminated (to not match on a device
			// that has `name` as prefix).
			"\n.*Block devices:",
	)

	data, err := os.ReadFile(procDevicesPath)
	if err != nil {
		return -1, fmt.Errorf("error reading '%s': %w", procDevicesPath, err)
	}

	// Expect precisely one match: first element is the total match, second
	// element corresponds to first capture group within that match (i.e., the
	// number of interest).
	matches := re.FindStringSubmatch(string(data))
	if len(matches) != 2 {
		return -1, fmt.Errorf("error parsing '%s': unexpected regex match: %v", procDevicesPath, matches)
	}

	// Convert capture group content to integer. Perform upper bound check:
	// value must fit into 32-bit integer (it's then also guaranteed to fit into
	// a 32-bit unsigned integer, which is the type that must be passed to
	// unix.Mkdev()).
	major, err := strconv.ParseInt(matches[1], 10, 32)
	if err != nil {
		return -1, fmt.Errorf("error parsing device major %s from '%s': %w", matches[1], procDevicesPath, err)
	}

	// ParseInt() always returns an integer of explicit type `int64`. We have
	// performed an upper bound check so it's safe to convert this to `int`
	// (which is documented as "int is a signed integer type that is at least 32
	// bits in size", so in theory it could be smaller than int64).
	return int(major), nil
}
