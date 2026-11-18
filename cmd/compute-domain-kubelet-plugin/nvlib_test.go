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

package main

import (
	"os"
	"path/filepath"
	"testing"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	"github.com/NVIDIA/go-nvml/pkg/nvml"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCheckHostIMEXReadyMissingBinary confirms that a driver root without
// nvidia-imex-ctl (e.g. a node where the optional nvidia-imex package was
// never installed) fails with an actionable error, without ever attempting
// to chroot/exec. The success path (chrooting in and parsing "READY") needs
// a real nvidia-imex-ctl binary and elevated privileges, so it is not
// covered by this unit test.
func TestCheckHostIMEXReadyMissingBinary(t *testing.T) {
	l := deviceLib{devRoot: t.TempDir()}

	err := l.checkHostIMEXReady(defaultIMEXHostSocketPath)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "nvidia-imex-ctl not found")
}

// No devices report an MNNVL clique in these node-local tests.
type noCliqueDevices struct {
	nvdev.Interface
}

func (noCliqueDevices) VisitDevices(func(int, nvdev.Device) error) error { return nil }

type nodeLocalNVML struct {
	nvml.Interface
}

func (nodeLocalNVML) InitWithFlags(uint32) nvml.Return { return nvml.SUCCESS }
func (nodeLocalNVML) Shutdown() nvml.Return            { return nvml.SUCCESS }

func TestGetCliqueIDNodeLocalRequiresFabric(t *testing.T) {
	for name, tc := range map[string]struct {
		enabled   bool
		fabric    bool
		broken    bool
		wantError string
	}{
		"gate disabled":   {},
		"no fabric":       {enabled: true, wantError: "requires a Fabric Manager-managed NVLink fabric"},
		"NVSwitch fabric": {enabled: true, fabric: true},
		"detection error": {enabled: true, broken: true, wantError: "failed to check if node has local fabric"},
	} {
		t.Run(name, func(t *testing.T) {
			setNodeLocalFabricIPCForTest(t, tc.enabled)
			hostRoot := t.TempDir()
			devicesDir := filepath.Join(hostRoot, "proc", "driver", "nvidia-nvswitch", "devices")
			if tc.fabric {
				require.NoError(t, os.MkdirAll(filepath.Join(devicesDir, "0000:01:00.0"), 0755))
			}
			if tc.broken {
				require.NoError(t, os.WriteFile(filepath.Join(hostRoot, "proc"), nil, 0600))
			}
			l := deviceLib{Interface: noCliqueDevices{}, nvmllib: nodeLocalNVML{}, devRoot: hostRoot}
			cliqueID, err := l.getCliqueID()
			assert.Empty(t, cliqueID)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
