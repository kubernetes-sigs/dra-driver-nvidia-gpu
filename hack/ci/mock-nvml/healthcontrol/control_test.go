/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package healthcontrol

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseHealthControlDefaultsToFullGPU(t *testing.T) {
	doc, err := ParseHealthControl([]byte(`{"events":[{"gpu":0,"xid":43}]}`))
	require.NoError(t, err)
	require.Len(t, doc.Events, 1)
	require.Equal(t, FullGPUInstanceID, doc.Events[0].GPUInstanceID)
	require.Equal(t, FullGPUInstanceID, doc.Events[0].ComputeInstanceID)
	require.Equal(t, uint64(43), doc.Events[0].XID)
	require.Equal(t, RecoveryActionNone, doc.RecoveryAction(0))
}

func TestParseHealthControlKeepsExplicitInstanceIDs(t *testing.T) {
	doc, err := ParseHealthControl([]byte(`{
		"events":[{"id":"mig","gpu":1,"xid":79,"gpuInstanceId":3,"computeInstanceId":0}]
	}`))
	require.NoError(t, err)
	require.Equal(t, "mig", doc.Events[0].ID)
	require.Equal(t, uint32(3), doc.Events[0].GPUInstanceID)
	require.Equal(t, uint32(0), doc.Events[0].ComputeInstanceID)
}

func TestParseRecoveryActionNames(t *testing.T) {
	cases := map[string]uint32{
		"NONE":                          RecoveryActionNone,
		"GPU_RECOVERY_ACTION_NONE":      RecoveryActionNone,
		"GPU_RESET":                     RecoveryActionGPUReset,
		"GPU_RECOVERY_ACTION_GPU_RESET": RecoveryActionGPUReset,
		"  node_reboot ":                RecoveryActionNodeReboot,
		"RECOVER_IMEX_DOMAIN":           RecoveryActionRecoverIMEXDomain,
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseRecoveryAction(name)
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}

	_, err := ParseRecoveryAction("not-an-action")
	require.Error(t, err)
}

func TestHealthStoreClaimOnceAndRecoveryTransition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health-control.json")
	store := NewHealthStore(path)

	event, ok := store.Claim()
	require.False(t, ok)
	require.Zero(t, event.XID)
	require.Equal(t, RecoveryActionNone, store.RecoveryAction(0))

	writeHealthControl(t, path, `{
		"events":[
			{"id":"first","gpu":0,"xid":43},
			{"id":"second","gpu":1,"xid":79,"gpuInstanceId":3,"computeInstanceId":1}
		],
		"recoveryActions":[{"gpu":1,"action":"GPU_RESET"}]
	}`)

	first, ok := store.Claim()
	require.True(t, ok)
	require.Equal(t, "first", first.ID)
	require.Equal(t, uint64(43), first.XID)
	require.Equal(t, FullGPUInstanceID, first.GPUInstanceID)

	second, ok := store.Claim()
	require.True(t, ok)
	require.Equal(t, "second", second.ID)
	require.Equal(t, uint32(3), second.GPUInstanceID)
	require.Equal(t, uint32(1), second.ComputeInstanceID)
	require.Equal(t, RecoveryActionGPUReset, store.RecoveryAction(1))
	require.Equal(t, RecoveryActionNone, store.RecoveryAction(0))

	_, ok = store.Claim()
	require.False(t, ok)

	// Rewriting the same events must not deliver them again, but a recovery
	// action change is visible on the next read.
	writeHealthControl(t, path, `{
		"events":[
			{"id":"first","gpu":0,"xid":43},
			{"id":"second","gpu":1,"xid":79,"gpuInstanceId":3,"computeInstanceId":1}
		],
		"recoveryActions":[{"gpu":1,"action":"GPU_RECOVERY_ACTION_NONE"}]
	}`)
	_, ok = store.Claim()
	require.False(t, ok)
	require.Equal(t, RecoveryActionNone, store.RecoveryAction(1))

	writeHealthControl(t, path, `{"events":[{"id":"third","gpu":2,"xid":13}]}`)
	third, ok := store.Claim()
	require.True(t, ok)
	require.Equal(t, "third", third.ID)
}

func TestHealthStoreKeepsLastValidDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health-control.json")
	store := NewHealthStore(path)
	writeHealthControl(t, path, `{"recoveryActions":[{"gpu":0,"action":"GPU_RESET"}]}`)
	require.Equal(t, RecoveryActionGPUReset, store.RecoveryAction(0))

	writeHealthControl(t, path, `{"recoveryActions":[{"gpu":0,"action":"nope"}]}`)
	require.Equal(t, RecoveryActionGPUReset, store.RecoveryAction(0))
}

func TestHealthControlPath(t *testing.T) {
	t.Setenv("MOCK_NVML_HEALTH_CONTROL", "/var/lib/nvml-mock/driver/health-control.json")
	require.Equal(t, "/var/lib/nvml-mock/driver/health-control.json", HealthControlPath())

	t.Setenv("MOCK_NVML_HEALTH_CONTROL", "")
	require.Equal(t, defaultHealthControlPath, HealthControlPath())
}

func writeHealthControl(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
}
