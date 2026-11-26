package main

import (
	"errors"
	"fmt"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"k8s.io/klog/v2"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

type ClockManager struct {
	nvdevlib *deviceLib
}

func NewClockManager(devicelib *deviceLib) *ClockManager {
	return &ClockManager{
		nvdevlib: devicelib,
	}
}

func (c *ClockManager) SetClock(uuids []string, config configapi.GpuClocks) error {
	if config.SMLockedMHz == nil {
		return fmt.Errorf("SMLockedMHz configuration is missing (nil)")
	}
	clockSpeed := uint32(*config.SMLockedMHz)
	// Track completed locks so a later GPU failure can undo earlier changes.
	lockedGPUUUIDs := make([]string, 0, len(uuids))
	for _, uuid := range uuids {
		device, ret := c.nvdevlib.DeviceGetHandleByUUID(uuid)
		if ret != nvml.SUCCESS {
			if len(lockedGPUUUIDs) > 0 {
				err := c.ResetGpuClockSpeed(lockedGPUUUIDs)
				if err != nil {
					return fmt.Errorf("couldn't get device for uuid %s: %v (rollback also failed: %w)", uuid, ret, err)
				}
			}
			return fmt.Errorf("couldn't get device for uuid %s: %v", uuid, ret)
		}
		ret = device.SetGpuLockedClocks(clockSpeed, clockSpeed)
		if ret != nvml.SUCCESS {
			if len(lockedGPUUUIDs) > 0 {
				err := c.ResetGpuClockSpeed(lockedGPUUUIDs)
				if err != nil {
					return fmt.Errorf("failed to set clock speed for device %s: %v (rollback also failed: %w)", uuid, ret, err)
				}
			}
			return fmt.Errorf("failed to set clock speed for device %s: %v", uuid, ret)
		}
		lockedGPUUUIDs = append(lockedGPUUUIDs, uuid)
	}
	return nil
}

func (c *ClockManager) ResetGpuClockSpeed(uuids []string) error {
	var errs []error

	for _, uuid := range uuids {
		device, ret := c.nvdevlib.DeviceGetHandleByUUID(uuid)
		if ret != nvml.SUCCESS {
			err := fmt.Errorf("failed to get NVML device handle for UUID %s: %v", uuid, ret)
			klog.Warningf("%v", err)
			errs = append(errs, err)
			continue // Keep trying the rest of the GPUs!
		}

		ret = device.ResetGpuLockedClocks()
		if ret != nvml.SUCCESS {
			err := fmt.Errorf("failed to reset locked GPU clocks for device UUID %s: %v", uuid, ret)
			klog.Warningf("%v", err)
			errs = append(errs, err) // Keep trying the rest of the GPUs!
		}
	}

	return errors.Join(errs...)
}
