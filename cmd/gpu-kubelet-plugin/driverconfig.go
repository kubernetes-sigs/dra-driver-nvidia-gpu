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
	"errors"
	"fmt"
	"os"
	"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	coreclientset "k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

const (
	driverConfigVersion   = "v1alpha1"
	driverConfigDirectory = "/available-configs"
	driverConfigNodeLabel = "nvidia.com/dra-driver-gpu.config"
)

// DriverConfig is the versioned startup configuration selected for this node.
// The Alpha schema is intentionally limited to node-local device availability.
type DriverConfig struct {
	Version string           `json:"version"`
	GPU     *GPUDriverConfig `json:"gpu,omitempty"`
}

// GPUDriverConfig controls which discovered GPU device types remain allocatable.
type GPUDriverConfig struct {
	AdvertisedDeviceTypes []string `json:"advertisedDeviceTypes"`
}

// validate keeps the Alpha schema versioned and closed to a non-empty,
// duplicate-free set of public device types already understood by the driver.
func (c *DriverConfig) validate() error {
	if c.Version != driverConfigVersion {
		return fmt.Errorf("unsupported version %q, expected %q", c.Version, driverConfigVersion)
	}
	if c.GPU == nil {
		return errors.New("gpu configuration is required")
	}
	if len(c.GPU.AdvertisedDeviceTypes) == 0 {
		return errors.New("gpu.advertisedDeviceTypes must not be empty")
	}
	supported := map[string]struct{}{
		GpuDeviceType:       {},
		MigStaticDeviceType: {},
		VfioDeviceType:      {},
	}
	seen := make(map[string]struct{}, len(c.GPU.AdvertisedDeviceTypes))
	for _, deviceType := range c.GPU.AdvertisedDeviceTypes {
		if _, ok := supported[deviceType]; !ok {
			return fmt.Errorf("unsupported gpu.advertisedDeviceTypes value %q", deviceType)
		}
		if _, ok := seen[deviceType]; ok {
			return fmt.Errorf("duplicate gpu.advertisedDeviceTypes value %q", deviceType)
		}
		seen[deviceType] = struct{}{}
	}
	return nil
}

// "mig" covers static and dynamic MIG, which share the public type; a nil config remains allow-all.
func (c *DriverConfig) advertises(deviceType string) bool {
	if c == nil || c.GPU == nil {
		return true
	}
	if deviceType == MigDynamicDeviceType {
		deviceType = MigStaticDeviceType
	}
	for _, allowed := range c.GPU.AdvertisedDeviceTypes {
		if deviceType == allowed {
			return true
		}
	}
	return false
}

// resolveDriverConfigFromDirectory gives an explicit node label precedence over
// the Helm-selected default. When neither is set, returning no policy preserves
// publication from the release-wide settings.
func resolveDriverConfigFromDirectory(
	ctx context.Context,
	client coreclientset.Interface,
	nodeName string,
	configDirectory string,
	defaultProfile string,
) (*DriverConfig, string, error) {
	if client == nil {
		return nil, "", errors.New("kubernetes client is required")
	}
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("get node %q: %w", nodeName, err)
	}

	profile := defaultProfile
	if selected, exists := node.Labels[driverConfigNodeLabel]; exists {
		if selected == "" {
			return nil, "", fmt.Errorf("driver config node label %q must not be empty", driverConfigNodeLabel)
		}
		profile = selected
	}
	if profile == "" {
		return nil, "", nil
	}
	if errs := validation.IsConfigMapKey(profile); len(errs) > 0 {
		return nil, "", fmt.Errorf("invalid driver config profile %q: %v", profile, errs)
	}

	data, err := os.ReadFile(filepath.Join(configDirectory, profile))
	if err != nil {
		return nil, "", fmt.Errorf("read driver config profile %q: %w", profile, err)
	}

	config := &DriverConfig{}
	if err := yaml.UnmarshalStrict(data, config); err != nil {
		return nil, "", fmt.Errorf("decode driver config profile %q: %w", profile, err)
	}
	if err := config.validate(); err != nil {
		return nil, "", fmt.Errorf("validate driver config profile %q: %w", profile, err)
	}

	return config, profile, nil
}
