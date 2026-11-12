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

package v1beta1

import (
	"fmt"
	"strconv"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

const (
	gpuSharingFeatureGateError = "%q is selected as the GPU sharing strategy, but the %q feature gate is not enabled"
	gpuSharingUnknownStrategy  = "unknown GPU sharing strategy: %v, supported GPU sharing strategies: %s, %s"
)

// validateSharingStrategy checks that the given strategy is known and its feature gate is enabled.
func validateSharingStrategy(s string) error {
	if s == TimeSlicingStrategy {
		if featuregates.Enabled(featuregates.TimeSlicingSettings) {
			return nil
		}
		return fmt.Errorf(gpuSharingFeatureGateError, TimeSlicingStrategy, featuregates.TimeSlicingSettings)
	}
	if s == MpsStrategy {
		if featuregates.Enabled(featuregates.MPSSupport) {
			return nil
		}
		return fmt.Errorf(gpuSharingFeatureGateError, MpsStrategy, featuregates.MPSSupport)
	}
	return fmt.Errorf(gpuSharingUnknownStrategy, s, TimeSlicingStrategy, MpsStrategy)
}

// Validate ensures that GpuSharingStrategy has a valid set of values.
func (s GpuSharingStrategy) Validate() error {
	return validateSharingStrategy(string(s))
}

// Validate ensures that MigDeviceSharingStrategy has a valid set of values.
func (s MigDeviceSharingStrategy) Validate() error {
	return validateSharingStrategy(string(s))
}

// Validate ensures that TimeSliceInterval has a valid set of values.
func (t TimeSliceInterval) Validate() error {
	switch t {
	case DefaultTimeSlice, ShortTimeSlice, MediumTimeSlice, LongTimeSlice:
		return nil
	}
	return fmt.Errorf("unknown time-slice interval: %v, supported time-slice intervals: %s, %s, %s, %s",
		t, DefaultTimeSlice, ShortTimeSlice, MediumTimeSlice, LongTimeSlice)
}

// Validate ensures that TimeSlicingConfig has a valid set of values.
func (c *TimeSlicingConfig) Validate() error {
	if c == nil || c.Interval == nil {
		return fmt.Errorf("no time-slice interval set")
	}
	return c.Interval.Validate()
}

// Validate ensures that MpsConfig has a valid set of values.
func (c *MpsConfig) Validate() error {
	if c.DefaultActiveThreadPercentage != nil {
		if *c.DefaultActiveThreadPercentage < 0 {
			return fmt.Errorf("active thread percentage must not be negative")
		}
		if *c.DefaultActiveThreadPercentage > 100 {
			return fmt.Errorf("active thread percentage must not be greater than 100")
		}
	}
	// The pinned memory limits are converted to whole megabytes when the MPS
	// control daemon is started (see MpsPerDevicePinnedMemoryLimit.Normalize),
	// and a limit below 1Mi is rejected there. Reject it here as well, so that
	// such a config is refused at admission instead of failing every attempt
	// to prepare the claim.
	if c.DefaultPinnedDeviceMemoryLimit != nil {
		if _, valid := (limit)(*c.DefaultPinnedDeviceMemoryLimit).Megabyte(); !valid {
			return fmt.Errorf("%w: default pinned device memory limit must be at least 1Mi: %s", ErrInvalidLimit, c.DefaultPinnedDeviceMemoryLimit.String())
		}
	}
	for key, value := range c.DefaultPerDevicePinnedMemoryLimit {
		// A key is either a device UUID or a device index. Whether a UUID or
		// an index matches an allocated device is only known once the claim
		// is allocated, but a negative index can never match one.
		if index, err := strconv.Atoi(key); err == nil && index < 0 {
			return fmt.Errorf("%w: device index %d must not be negative", ErrInvalidDeviceSelector, index)
		}
		if _, valid := (limit)(value).Megabyte(); !valid {
			return fmt.Errorf("%w: pinned device memory limit for device %s must be at least 1Mi: %s", ErrInvalidLimit, key, value.String())
		}
	}
	return nil
}

// Validate ensures that GpuSharing has a valid set of values.
func (s *GpuSharing) Validate() error {
	if err := s.Strategy.Validate(); err != nil {
		return err
	}
	switch {
	case featuregates.Enabled(featuregates.TimeSlicingSettings) && s.IsTimeSlicing():
		// GetTimeSlicingConfig refuses an MpsConfig next to the TimeSlicing
		// strategy when the claim is prepared; refuse it at admission too.
		if s.MpsConfig != nil {
			return fmt.Errorf("cannot use MpsConfig with the '%v' strategy", TimeSlicingStrategy)
		}
		return s.TimeSlicingConfig.Validate()
	case featuregates.Enabled(featuregates.MPSSupport) && s.IsMps():
		// GetMpsConfig refuses a TimeSlicingConfig next to the MPS strategy
		// when the claim is prepared; refuse it at admission too.
		if s.TimeSlicingConfig != nil {
			return fmt.Errorf("cannot use TimeSlicingConfig with the '%v' strategy", MpsStrategy)
		}
		return s.MpsConfig.Validate()
	}
	return fmt.Errorf("invalid GPU sharing settings: %v", s)
}

// Validate ensures that MigDeviceSharing has a valid set of values.
func (s *MigDeviceSharing) Validate() error {
	if err := s.Strategy.Validate(); err != nil {
		return err
	}
	switch {
	case featuregates.Enabled(featuregates.TimeSlicingSettings) && s.IsTimeSlicing():
		return nil
	case featuregates.Enabled(featuregates.MPSSupport) && s.IsMps():
		return s.MpsConfig.Validate()
	}
	return fmt.Errorf("invalid MIG device sharing settings: %v", s)
}
