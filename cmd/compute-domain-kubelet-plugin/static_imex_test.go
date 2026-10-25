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

package main

import (
	"context"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	pkgflags "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
	nvfake "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/clientset/versioned/fake"
	nvinformers "sigs.k8s.io/dra-driver-nvidia-gpu/pkg/nvidia.com/informers/externalversions"
)

const testCDDSUID = "cdds-uid"

func testCDDS() *configapi.ComputeDomainDaemonSet {
	return &configapi.ComputeDomainDaemonSet{ObjectMeta: metav1.ObjectMeta{Name: configapi.ComputeDomainDaemonSetName, UID: types.UID(testCDDSUID)}}
}

func testStaticClique(status string) *configapi.ComputeDomainClique {
	return &configapi.ComputeDomainClique{
		ObjectMeta: metav1.ObjectMeta{Name: testCDDSUID + ".clique-a", Namespace: "driver"},
		Daemons:    []*configapi.ComputeDomainDaemonInfo{{NodeName: "node-1", CliqueID: "clique-a", Status: status}},
	}
}

// staticTestState returns a DeviceState for static IMEX daemons on node-1 of
// clique-a, with channels 0..3 and channel 2 reserved for ComputeDomain cd-a.
func staticTestState(t *testing.T, cdds *configapi.ComputeDomainDaemonSet, objects ...runtime.Object) *DeviceState {
	t.Helper()
	config := &Config{
		flags:      &Flags{nodeName: "node-1", namespace: "driver", kubeletPluginsDirectoryPath: t.TempDir()},
		clientsets: pkgflags.ClientSets{Nvidia: nvfake.NewSimpleClientset(objects...)},
		imexConfig: imex.Config{Mode: imex.ModeDriverManaged, Scope: imex.ScopeCluster, Isolation: imex.IsolationIMEXChannel},
	}
	cddsInformer := nvinformers.NewSharedInformerFactory(nvfake.NewSimpleClientset(), 0).Resource().V1beta1().ComputeDomainDaemonSets().Informer()
	if cdds != nil {
		require.NoError(t, cddsInformer.GetStore().Add(cdds))
	}
	allocatable, infos := testChannels(4)
	return &DeviceState{
		config:            config,
		checkpointManager: &fakeCheckpointManager{checkpoint: checkpointWithClaims(nil)},
		computeDomainManager: &ComputeDomainManager{
			config:          config,
			informer:        testCDInformer(t),
			cddsInformer:    cddsInformer,
			cliqueID:        "clique-a",
			configFilesRoot: t.TempDir(),
		},
		cdi:         &CDIHandler{devRoot: t.TempDir()},
		nvdevlib:    &deviceLib{devRoot: t.TempDir(), nvCapImexChanDevInfos: infos},
		allocatable: allocatable,
	}
}

func TestApplyChannelConfigStaticDaemon(t *testing.T) {
	setChannelReservationsGate(t, true)
	result := allocationResult("request", DriverName, "channel-2", nil)
	apply := func(state *DeviceState) (*DeviceConfigState, error) {
		return state.applyComputeDomainChannelConfig(context.Background(), channelConfig("cd-a"), claimWithResults("claim-uid", result), []*resourceapi.DeviceRequestAllocationResult{&result})
	}

	t.Run("waits for the ComputeDomainDaemonSet", func(t *testing.T) {
		_, err := apply(staticTestState(t, nil))

		require.Error(t, err)
		assert.False(t, isPermanentError(err))
		assert.Contains(t, err.Error(), "create it before running ComputeDomain workloads")
	})

	t.Run("waits for the static IMEX daemon of the node", func(t *testing.T) {
		_, err := apply(staticTestState(t, testCDDS(), testStaticClique(configapi.ComputeDomainStatusNotReady)))

		require.Error(t, err)
		assert.False(t, isPermanentError(err))
		assert.Contains(t, err.Error(), "static IMEX daemon not ready")
	})

	t.Run("injects the reserved channel once the daemon is ready", func(t *testing.T) {
		configState, err := apply(staticTestState(t, testCDDS(), testStaticClique(configapi.ComputeDomainStatusReady)))

		require.NoError(t, err)
		require.Len(t, configState.containerEdits.DeviceNodes, 1)
		assert.Equal(t, "/dev/nvidia-caps-imex-channels/channel2", configState.containerEdits.DeviceNodes[0].Path)
	})
}

func TestAssertComputeDomainDaemonSetUID(t *testing.T) {
	t.Run("missing is retried", func(t *testing.T) {
		err := staticTestState(t, nil).computeDomainManager.AssertComputeDomainDaemonSetUID(testCDDSUID)
		require.Error(t, err)
		assert.False(t, isPermanentError(err))
	})

	t.Run("another UID is rejected", func(t *testing.T) {
		err := staticTestState(t, testCDDS()).computeDomainManager.AssertComputeDomainDaemonSetUID("old-uid")
		require.Error(t, err)
		assert.True(t, isPermanentError(err), "a daemon claim of a deleted ComputeDomainDaemonSet never becomes valid")
	})

	t.Run("current UID", func(t *testing.T) {
		require.NoError(t, staticTestState(t, testCDDS()).computeDomainManager.AssertComputeDomainDaemonSetUID(testCDDSUID))
	})
}

func TestStaticDaemonContainerEdits(t *testing.T) {
	m := staticTestState(t, testCDDS()).computeDomainManager
	settings, err := m.NewSettings(testCDDSUID)
	require.NoError(t, err)

	edits, err := settings.GetCDIContainerEditsCommon(context.Background())

	require.NoError(t, err)
	env := strings.Join(edits.Env, " ")
	assert.Contains(t, env, "COMPUTE_DOMAIN_DAEMONSET_UID="+testCDDSUID)
	assert.Contains(t, env, "CLIQUE_ID=clique-a")
	assert.NotContains(t, env, "COMPUTE_DOMAIN_UUID", "static IMEX daemons serve no ComputeDomain")
	require.Len(t, edits.Mounts, 1)
	assert.Equal(t, "/imexd", edits.Mounts[0].ContainerPath)
}

func TestComputeDomainPublishedDevicesStaticDaemon(t *testing.T) {
	setChannelReservationsGate(t, true)
	allocatable, _ := testChannels(2)
	allocatable["daemon-0"] = &AllocatableDevice{Daemon: &ComputeDomainDaemonInfo{ID: 0}}
	config := staticTestState(t, nil).config

	devices := computeDomainPublishedDevices(allocatable, config, channelDeviceState{owner: "ignored", cliqueID: "clique-a"})

	require.Equal(t, []string{imex.ChannelDeviceName, "channel-0", "daemon-0"}, deviceNames(devices), "static IMEX daemons are allocated daemon devices")
	channel := findDevice(t, devices, imex.ChannelDeviceName)
	assert.Equal(t, []string{imex.ChannelAdmittedCondition}, channel.BindingConditions, "the controller assigns the channel at admission")
	assert.Empty(t, channel.Taints, "ComputeDomains share nodes in a shared IMEX domain")
	assert.Equal(t, ptr.To("clique-a"), channel.Attributes[imex.ChannelCliqueAttribute].StringValue)
}

func TestStaticDaemonKeepsGPUCliqueLabelOnStop(t *testing.T) {
	static := staticTestState(t, nil).computeDomainManager
	static.config.flags.gpuCliqueLabelEnabled = true
	assert.False(t, static.removesGPUCliqueLabelOnStop(), "a plugin restart must not delete the static IMEX daemon")

	perComputeDomain := &ComputeDomainManager{config: driverManagedConfig()}
	perComputeDomain.config.flags.gpuCliqueLabelEnabled = true
	assert.True(t, perComputeDomain.removesGPUCliqueLabelOnStop())
}

func TestCliqueIDRefreshInterval(t *testing.T) {
	static := staticTestState(t, nil).computeDomainManager
	assert.Equal(t, staticDaemonCliqueRefreshInterval, static.cliqueIDRefreshInterval(), "static IMEX daemons follow partition changes sooner")

	perComputeDomain := &ComputeDomainManager{config: driverManagedConfig()}
	assert.Equal(t, gpuCliqueLabelRefreshInterval, perComputeDomain.cliqueIDRefreshInterval())
}
