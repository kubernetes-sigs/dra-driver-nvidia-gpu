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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	coreclientset "k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

const (
	driverConfigDirectory = "/available-configs"
	driverConfigNodeLabel = "nvidia.com/dra-driver-gpu.config"
)

// DriverConfig is the GpuDriverConfig selected for this node. A local type
// keeps plugin-internal device-type mapping out of the API package.
type DriverConfig configapi.GpuDriverConfig

// A nil config remains allow-all.
func (c *DriverConfig) advertises(deviceType string) bool {
	if c == nil {
		return true
	}
	switch deviceType {
	case GpuDeviceType:
		return c.Advertisement.Gpu
	case MigStaticDeviceType, MigDynamicDeviceType:
		return c.Advertisement.Mig
	case VfioDeviceType:
		return c.Advertisement.Vfio
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

	jsonData, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, "", fmt.Errorf("decode driver config profile %q: %w", profile, err)
	}
	obj, err := runtime.Decode(configapi.StrictDecoder, jsonData)
	if err != nil {
		return nil, "", fmt.Errorf("decode driver config profile %q: %w", profile, err)
	}
	config, ok := obj.(*configapi.GpuDriverConfig)
	if !ok {
		return nil, "", fmt.Errorf("driver config profile %q: expected kind %s, got %T", profile, configapi.GpuDriverConfigKind, obj)
	}
	if err := config.Normalize(); err != nil {
		return nil, "", fmt.Errorf("normalize driver config profile %q: %w", profile, err)
	}
	if err := config.Validate(); err != nil {
		return nil, "", fmt.Errorf("validate driver config profile %q: %w", profile, err)
	}

	return (*DriverConfig)(config), profile, nil
}
