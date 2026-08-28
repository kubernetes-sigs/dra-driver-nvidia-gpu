/*
 * Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/klog/v2"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/fabricmanager"
)

// reconcileStrandedPassthroughGPUs returns GPUs that are bound to a vfio driver
// without a prepared claim behind them to the nvidia driver, and releases any
// Fabric Manager partition that was left active for them.
//
// A GPU ends up in that state when a passthrough consumer's pod goes away
// without NodeUnprepareResources running -- a force delete is the usual way.
// The GPU keeps the vfio binding, and nothing ever puts it back, which has two
// consequences worth undoing at startup:
//
//   - NVML can only enumerate GPUs bound to the nvidia driver, so a stranded GPU
//     is invisible to discovery. It is announced as a VFIO device with no parent
//     GpuInfo, which means no gpuModuleID and therefore no partitionN attributes
//     -- a claim constraining on those can never select it again.
//   - Its FM partition stays active, so no overlapping partition can be
//     activated for as long as that lasts.
//
// This must run before device discovery so that NVML sees the reclaimed GPUs and
// everything downstream (module IDs, partition attributes, full-GPU
// announcement) is populated normally.
//
// Failure to reclaim one GPU is logged and skipped rather than returned: a
// single busy GPU must not stop the plugin from starting and serving the rest of
// the node.
func (s *DeviceState) reconcileStrandedPassthroughGPUs(ctx context.Context, currentBootID string) error {
	if s.vfioPciManager == nil {
		// Passthrough is disabled, so this driver never binds a GPU to vfio-pci.
		// Anything vfio-bound on this node belongs to someone else.
		return nil
	}

	held, err := s.pciBusIDsHeldByPreparedClaims(ctx, currentBootID)
	if err != nil {
		return fmt.Errorf("determining which GPUs prepared claims hold: %w", err)
	}

	pciDevices, err := s.nvdevlib.nvpci.GetGPUs()
	if err != nil {
		return fmt.Errorf("enumerating GPU PCI devices: %w", err)
	}

	var stranded []string
	for _, pci := range pciDevices {
		driver, err := getDriver(pciDevicesPath, pci.Address)
		if err != nil {
			// No driver bound, or the device went away. Nothing to reclaim.
			klog.V(6).Infof("Could not determine driver binding for GPU %s: %v", pci.Address, err)
			continue
		}
		if !isVfioDriver(driver) {
			continue
		}
		if _, live := held[fabricmanager.NormalizePCIBusID(pci.Address)]; live {
			klog.Infof("GPU %s is bound to %q for a still-prepared claim; leaving it as is", pci.Address, driver)
			continue
		}
		klog.Warningf("GPU %s is bound to %q but no prepared claim holds it; reclaiming it for the nvidia driver. "+
			"A GPU is left in this state when a passthrough consumer's pod is removed without NodeUnprepareResources "+
			"running, e.g. a force delete", pci.Address, driver)
		stranded = append(stranded, pci.Address)
	}

	if len(stranded) == 0 {
		return nil
	}

	reclaimed := make(map[string]struct{}, len(stranded))
	for _, addr := range stranded {
		if err := s.vfioPciManager.RebindToNvidiaByPCIBusID(ctx, addr); err != nil {
			klog.Errorf("Failed to reclaim stranded GPU %s for the nvidia driver: %v", addr, err)
			continue
		}
		klog.Infof("Reclaimed stranded GPU %s for the nvidia driver", addr)
		reclaimed[fabricmanager.NormalizePCIBusID(addr)] = struct{}{}
	}

	// Release fabric partitions only after the rebind, mirroring the teardown
	// ordering in Unprepare: FM must not be asked to tear down a partition whose
	// GPUs are still handed out for passthrough.
	s.deactivateFabricPartitionsForReclaimedGPUs(reclaimed)

	return nil
}

// pciBusIDsHeldByPreparedClaims returns the normalized PCI bus ids of the
// passthrough GPUs that the checkpoint says are still prepared, and therefore
// legitimately vfio-bound. Claims that only started preparing count too: their
// devices may already be bound.
func (s *DeviceState) pciBusIDsHeldByPreparedClaims(ctx context.Context, currentBootID string) (map[string]struct{}, error) {
	held := make(map[string]struct{})

	checkpoints, err := s.checkpointManager.ListCheckpoints()
	if err != nil {
		return nil, fmt.Errorf("listing checkpoints: %w", err)
	}
	if !slices.Contains(checkpoints, DriverPluginCheckpointFileBasename) {
		// Nothing was ever prepared on this node, so nothing is held.
		return held, nil
	}

	cp, err := s.getCheckpoint(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading checkpoint: %w", err)
	}

	// A differing boot ID invalidates every prepared claim in the checkpoint, so
	// none of them holds anything. Nothing survives a reboot vfio-bound either,
	// which makes this mostly belt and braces -- but reading stale claims as
	// live would suppress exactly the reclamation we are here to do.
	if storedBootID := cp.GetNodeBootID(); storedBootID != "" && storedBootID != currentBootID {
		klog.Infof("Checkpoint nodeBootID %q != current %q; treating all prepared claims as stale while reconciling passthrough bindings",
			storedBootID, currentBootID)
		return held, nil
	}

	for uid, claim := range cp.V2.PreparedClaims {
		for _, group := range claim.PreparedDevices {
			for _, device := range group.Devices {
				if device.Vfio == nil || device.Vfio.Info == nil {
					continue
				}
				pci := fabricmanager.NormalizePCIBusID(device.Vfio.Info.PciBusID)
				if pci == "" {
					continue
				}
				held[pci] = struct{}{}
				klog.V(6).Infof("Claim %s still holds passthrough GPU %s", uid, pci)
			}
		}
	}

	return held, nil
}

// deactivateFabricPartitionsForReclaimedGPUs deactivates each active FM
// partition whose GPUs were *all* just reclaimed.
//
// Requiring full coverage keeps this tied to the stranding we actually observed
// instead of asserting ownership over all FM state: a partition that mixes
// reclaimed GPUs with GPUs this driver knows nothing about is left alone, since
// deactivating it could disrupt a consumer we cannot see.
func (s *DeviceState) deactivateFabricPartitionsForReclaimedGPUs(reclaimed map[string]struct{}) {
	if !s.fabricManagerPartitioningEnabled() || len(reclaimed) == 0 {
		return
	}

	active, err := s.fmManager.ActivePartitions()
	if err != nil {
		klog.Errorf("Could not list active Fabric Manager partitions; leaving partition state untouched: %v", err)
		return
	}

	for _, partition := range active {
		covered := 0
		for _, gpu := range partition.GPUs {
			if _, ok := reclaimed[fabricmanager.NormalizePCIBusID(gpu.PCIBusID)]; ok {
				covered++
			}
		}
		if covered == 0 {
			continue
		}
		if covered != len(partition.GPUs) {
			klog.Warningf("Fabric Manager partition %d is active and %d of its %d GPUs were reclaimed, but not all of them; "+
				"leaving it active because the remaining GPUs may be in use outside this driver",
				partition.ID, covered, len(partition.GPUs))
			continue
		}
		klog.Warningf("Fabric Manager partition %d is active but all of its GPUs were stranded; deactivating it", partition.ID)
		if err := s.fmManager.DeactivatePartition(partition.ID); err != nil {
			klog.Errorf("Failed to deactivate orphaned Fabric Manager partition %d: %v", partition.ID, err)
		}
	}
}

// isVfioDriver reports whether the given kernel driver name is a vfio-pci
// variant. Besides plain "vfio-pci" this covers the variant drivers used for
// specific GPU generations, such as "nvgrace_gpu_vfio_pci" on Grace-based
// systems.
func isVfioDriver(driver string) bool {
	return strings.Contains(driver, "vfio")
}
