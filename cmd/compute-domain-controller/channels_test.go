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
	"encoding/json"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	draclient "k8s.io/dynamic-resource-allocation/client"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/flags"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
)

const workloadTemplatePath = "../../templates/compute-domain-workload-claim-template.tmpl.yaml"

func testComputeDomain(uid string, count int, channels ...int) *nvapi.ComputeDomain {
	return &nvapi.ComputeDomain{
		ObjectMeta: metav1.ObjectMeta{Name: "cd-" + uid, Namespace: "ns-" + uid, UID: types.UID(uid)},
		Spec: nvapi.ComputeDomainSpec{
			Channel: &nvapi.ComputeDomainChannelSpec{
				ResourceClaimTemplate: nvapi.ComputeDomainResourceClaimTemplate{Name: "job", Count: count},
			},
		},
		Status: nvapi.ComputeDomainStatus{Channels: channels},
	}
}

func testReservationManager(mode imex.Mode, maxChannels int, cds ...*nvapi.ComputeDomain) (*ChannelReservationManager, *[]*nvapi.ComputeDomain) {
	var updates []*nvapi.ComputeDomain
	config := &ManagerConfig{
		imexConfig:      imex.Config{Mode: mode, Isolation: imex.IsolationIMEXDomain},
		imexMaxChannels: maxChannels,
	}
	list := func() ([]*nvapi.ComputeDomain, error) { return cds, nil }
	update := func(_ context.Context, cd *nvapi.ComputeDomain) (*nvapi.ComputeDomain, error) {
		updates = append(updates, cd)
		return cd, nil
	}
	return NewChannelReservationManager(config, list, update), &updates
}

func TestWorkloadResourceClaimTemplateNames(t *testing.T) {
	assert.Equal(t, "job", workloadResourceClaimTemplateName(testComputeDomain("a", 1), 0), "a single template keeps the user-provided name")
	assert.Equal(t, "job", workloadResourceClaimTemplateName(testComputeDomain("a", 0), 0), "objects stored before the count field read as one template")
	assert.Equal(t, "job-0", workloadResourceClaimTemplateName(testComputeDomain("a", 3), 0))
	assert.Equal(t, "job-2", workloadResourceClaimTemplateName(testComputeDomain("a", 3), 2))
}

func TestReserveDriverManaged(t *testing.T) {
	// Another ComputeDomain holding the same channels does not matter: each
	// driver-managed ComputeDomain has its own IMEX domain.
	other := testComputeDomain("other", 2, 0, 1)
	m, updates := testReservationManager(imex.ModeDriverManaged, 8, other)

	cd, err := m.Reserve(context.Background(), testComputeDomain("a", 3), false)

	require.NoError(t, err)
	assert.Equal(t, []int{0, 1, 2}, cd.Status.Channels)
	require.Len(t, *updates, 1, "the reservation must be persisted in status")
}

func TestReserveHostManaged(t *testing.T) {
	t.Run("skips channels reserved by other ComputeDomains", func(t *testing.T) {
		m, _ := testReservationManager(imex.ModeHostManaged, 8, testComputeDomain("other", 2, 0, 2))

		cd, err := m.Reserve(context.Background(), testComputeDomain("a", 2), false)

		require.NoError(t, err)
		assert.Equal(t, []int{1, 3}, cd.Status.Channels)
	})

	t.Run("skips channels reserved in this process but not yet in the informer", func(t *testing.T) {
		m, _ := testReservationManager(imex.ModeHostManaged, 8)

		first, err := m.Reserve(context.Background(), testComputeDomain("a", 2), false)
		require.NoError(t, err)
		second, err := m.Reserve(context.Background(), testComputeDomain("b", 1), false)
		require.NoError(t, err)

		assert.Equal(t, []int{1, 2}, first.Status.Channels)
		assert.Equal(t, []int{3}, second.Status.Channels)
	})

	t.Run("released channels are reused", func(t *testing.T) {
		m, _ := testReservationManager(imex.ModeHostManaged, 8)
		_, err := m.Reserve(context.Background(), testComputeDomain("a", 1), false)
		require.NoError(t, err)

		m.Release("a")
		cd, err := m.Reserve(context.Background(), testComputeDomain("b", 1), false)

		require.NoError(t, err)
		assert.Equal(t, []int{1}, cd.Status.Channels)
	})

	t.Run("fails when channels are exhausted", func(t *testing.T) {
		m, updates := testReservationManager(imex.ModeHostManaged, 3, testComputeDomain("other", 2, 0, 1))

		_, err := m.Reserve(context.Background(), testComputeDomain("a", 2), false)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "only 1 of the 2 requested IMEX channels are unreserved")
		assert.Empty(t, *updates)
	})
}

func TestReserveHostManagedLegacy(t *testing.T) {
	// Legacy ComputeDomains share channel 0, which new ones never get.
	legacy := testComputeDomain("legacy-a", 1, 0)
	m, _ := testReservationManager(imex.ModeHostManaged, 8, legacy)

	b, err := m.Reserve(context.Background(), testComputeDomain("legacy-b", 1), true)
	require.NoError(t, err)
	assert.Equal(t, []int{0}, b.Status.Channels)

	_, err = m.Reserve(context.Background(), legacy, false)
	require.NoError(t, err, "a shared channel is no overlap")

	c, err := m.Reserve(context.Background(), testComputeDomain("new", 1), false)
	require.NoError(t, err)
	assert.Equal(t, []int{1}, c.Status.Channels)
}

func TestReserveDetectsOutdatedCRD(t *testing.T) {
	m, _ := testReservationManager(imex.ModeDriverManaged, 8)
	m.updateStatus = func(_ context.Context, cd *nvapi.ComputeDomain) (*nvapi.ComputeDomain, error) {
		pruned := cd.DeepCopy()
		pruned.Status.Channels = nil
		return pruned, nil
	}

	_, err := m.Reserve(context.Background(), testComputeDomain("a", 1), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "apply the ComputeDomain CRD")
	assert.Empty(t, m.pending)
}

func TestReserveKeepsExistingReservation(t *testing.T) {
	// Survives controller restarts: the reservation is read back from status.
	m, updates := testReservationManager(imex.ModeHostManaged, 8)
	cd := testComputeDomain("a", 2, 5, 7)

	got, err := m.Reserve(context.Background(), cd, false)

	require.NoError(t, err)
	assert.Equal(t, []int{5, 7}, got.Status.Channels)
	assert.Empty(t, *updates)
}

func TestReserveRejectsInvalidReservation(t *testing.T) {
	tests := map[string]struct {
		mode    imex.Mode
		cd      *nvapi.ComputeDomain
		others  []*nvapi.ComputeDomain
		wantErr string
	}{
		"count mismatch": {
			mode: imex.ModeDriverManaged, cd: testComputeDomain("a", 2, 0), wantErr: "expected 2 channels",
		},
		"out of range": {
			mode: imex.ModeDriverManaged, cd: testComputeDomain("a", 1, 8), wantErr: "out of range",
		},
		"duplicate": {
			mode: imex.ModeDriverManaged, cd: testComputeDomain("a", 2, 1, 1), wantErr: "reserved twice",
		},
		"overlap with another host-managed ComputeDomain": {
			mode: imex.ModeHostManaged, cd: testComputeDomain("a", 1, 3), others: []*nvapi.ComputeDomain{testComputeDomain("b", 1, 3)},
			wantErr: "also reserved by ComputeDomain b",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			m, _ := testReservationManager(tc.mode, 8, tc.others...)

			_, err := m.Reserve(context.Background(), tc.cd, false)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}

	t.Run("driver-managed ComputeDomains reuse channels", func(t *testing.T) {
		m, _ := testReservationManager(imex.ModeDriverManaged, 8, testComputeDomain("b", 1, 0))

		_, err := m.Reserve(context.Background(), testComputeDomain("a", 1, 0), false)

		require.NoError(t, err)
	})
}

func TestReserveRejectsTooManyChannels(t *testing.T) {
	m, _ := testReservationManager(imex.ModeDriverManaged, 2)

	_, err := m.Reserve(context.Background(), testComputeDomain("a", 3), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "at most 2 IMEX channels")
}

func channelClaim(t *testing.T, domainID string, allocated bool) resourceapi.ResourceClaim {
	t.Helper()
	config := nvapi.DefaultComputeDomainChannelConfig()
	config.DomainID = domainID
	raw, err := json.Marshal(config)
	require.NoError(t, err)
	deviceConfig := resourceapi.DeviceConfiguration{
		Opaque: &resourceapi.OpaqueDeviceConfiguration{Driver: DriverName, Parameters: runtime.RawExtension{Raw: raw}},
	}
	claim := resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim-" + domainID, Namespace: "default"}}
	if allocated {
		claim.Status.Allocation = &resourceapi.AllocationResult{
			Devices: resourceapi.DeviceAllocationResult{
				Config: []resourceapi.DeviceAllocationConfiguration{{Source: resourceapi.AllocationConfigSourceClaim, DeviceConfiguration: deviceConfig}},
			},
		}
	} else {
		claim.Spec.Devices.Config = []resourceapi.DeviceClaimConfiguration{{DeviceConfiguration: deviceConfig}}
	}
	return claim
}

func TestClaimReferencesComputeDomain(t *testing.T) {
	spec := channelClaim(t, "a", false)
	allocated := channelClaim(t, "a", true)
	other := channelClaim(t, "b", true)
	garbage := resourceapi.ResourceClaim{}
	garbage.Spec.Devices.Config = []resourceapi.DeviceClaimConfiguration{{DeviceConfiguration: resourceapi.DeviceConfiguration{
		Opaque: &resourceapi.OpaqueDeviceConfiguration{Driver: DriverName, Parameters: runtime.RawExtension{Raw: []byte("not json")}},
	}}}

	assert.True(t, claimReferencesComputeDomain(&spec, "a"))
	assert.True(t, claimReferencesComputeDomain(&allocated, "a"))
	assert.False(t, claimReferencesComputeDomain(&other, "a"))
	assert.False(t, claimReferencesComputeDomain(&garbage, "a"), "an undecodable config must not block unrelated ComputeDomains")
}

func testReservationManagerWithClient(t *testing.T, objects ...runtime.Object) (*ChannelReservationManager, *fake.Clientset) {
	t.Helper()
	client := fake.NewSimpleClientset(objects...)
	m, _ := testReservationManager(imex.ModeHostManaged, 8)
	m.config.clientsets = flags.ClientSets{Resource: draclient.New(client), Core: client}
	return m, client
}

func TestAssertNoClaimsReferenceComputeDomain(t *testing.T) {
	claim := channelClaim(t, "a", true)
	m, _ := testReservationManagerWithClient(t, &claim)

	err := m.AssertNoClaimsReferenceComputeDomain(context.Background(), "default", "a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "waiting for ResourceClaim default/claim-a")

	require.NoError(t, m.AssertNoClaimsReferenceComputeDomain(context.Background(), "default", "b"))
}

func renderWorkloadTemplate(t *testing.T, data *ResourceClaimTemplateTemplateData) *resourceapi.ResourceClaimTemplate {
	t.Helper()
	data.Namespace = "ns"
	data.Name = "job-0"
	data.Finalizer = computeDomainFinalizer
	data.ComputeDomainLabelKey = computeDomainLabelKey
	data.ComputeDomainLabelValue = "a"
	data.TargetLabelKey = computeDomainResourceClaimTemplateTargetLabelKey
	data.TargetLabelValue = computeDomainResourceClaimTemplateTargetWorkload
	data.DriverName = DriverName
	data.ChannelConfig = nvapi.DefaultComputeDomainChannelConfig()
	data.ChannelConfig.DomainID = "a"
	rct, err := renderResourceClaimTemplate(workloadTemplatePath, data)
	require.NoError(t, err)
	return rct
}

func TestRenderWorkloadResourceClaimTemplate(t *testing.T) {
	t.Run("legacy template requests the default channel", func(t *testing.T) {
		rct := renderWorkloadTemplate(t, &ResourceClaimTemplateTemplateData{DeviceClassName: computeDomainDefaultChannelDeviceClass})

		request := rct.Spec.Spec.Devices.Requests[0].Exactly
		assert.Equal(t, computeDomainDefaultChannelDeviceClass, request.DeviceClassName)
		assert.Empty(t, request.Selectors)
		assert.Empty(t, request.Tolerations)
		assert.False(t, resourceClaimTemplatePinnedToChannel(rct, 0))
	})

	t.Run("pinned template", func(t *testing.T) {
		rct := renderWorkloadTemplate(t, &ResourceClaimTemplateTemplateData{
			DeviceClassName: computeDomainChannelDeviceClass,
			ChannelSelector: channelSelectorExpression(7),
		})

		request := rct.Spec.Spec.Devices.Requests[0].Exactly
		assert.Equal(t, computeDomainChannelDeviceClass, request.DeviceClassName)
		assert.True(t, resourceClaimTemplatePinnedToChannel(rct, 7))
		assert.False(t, resourceClaimTemplatePinnedToChannel(rct, 0))
		assert.Empty(t, request.Tolerations)
	})
}

func TestWorkloadTemplateUpToDate(t *testing.T) {
	legacy := renderWorkloadTemplate(t, &ResourceClaimTemplateTemplateData{DeviceClassName: computeDomainDefaultChannelDeviceClass})
	pinned := renderWorkloadTemplate(t, &ResourceClaimTemplateTemplateData{
		DeviceClassName: computeDomainChannelDeviceClass,
		ChannelSelector: channelSelectorExpression(0),
	})
	tolerated := renderWorkloadTemplate(t, &ResourceClaimTemplateTemplateData{
		DeviceClassName:      computeDomainChannelDeviceClass,
		ChannelSelector:      channelSelectorExpression(0),
		OwnerTolerationKey:   imex.ChannelOwnerTaintKey,
		OwnerTolerationValue: "a",
	})
	cd := testComputeDomain("a", 1, 0)
	newManager := func(mode imex.Mode) *WorkloadResourceClaimTemplateManager {
		return &WorkloadResourceClaimTemplateManager{BaseResourceClaimTemplateManager: &BaseResourceClaimTemplateManager{
			config: &ManagerConfig{imexConfig: imex.Config{Mode: mode, Isolation: imex.IsolationIMEXDomain}},
		}}
	}

	assert.True(t, isLegacyWorkloadTemplate(legacy))
	assert.False(t, isLegacyWorkloadTemplate(pinned))

	driver := newManager(imex.ModeDriverManaged)
	assert.False(t, driver.upToDate(legacy, cd, 0), "driver-managed legacy templates are replaced")
	assert.False(t, driver.upToDate(pinned, cd, 0), "the owner toleration is missing")
	assert.True(t, driver.upToDate(tolerated, cd, 0))
	assert.False(t, driver.upToDate(tolerated, cd, 1))

	host := newManager(imex.ModeHostManaged)
	assert.True(t, host.upToDate(legacy, cd, legacySharedChannel), "host-managed legacy templates keep the shared channel")
	assert.False(t, host.upToDate(legacy, cd, 1))
}
