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
	"errors"
	"fmt"

	"github.com/Masterminds/semver/v3"
)

const voltaCudaComputeCapability = "7.0"

// ensureCapability checks if all GPUs meet the required CUDA capability.
func ensureCapability(gpuInfo map[string]*GpuInfo, gpus []string, cudaComputeCapability string) (err error) {
	required, parseErr := semver.NewVersion(cudaComputeCapability)
	if parseErr != nil {
		return fmt.Errorf("invalid required cudaComputeCapability %q: %w", cudaComputeCapability, parseErr)
	}

	for _, id := range gpus {
		info, ok := gpuInfo[id]
		if !ok || info == nil {
			return fmt.Errorf(
				"could not check gpu %s: missing gpu info (required >= %q)",
				id,
				cudaComputeCapability,
			)
		}

		cc, parseErr := semver.NewVersion(info.CudaComputeCapability)
		if parseErr != nil {
			err = errors.Join(fmt.Errorf("gpu %s has invalid cudaComputeCapability %q: %w",
				id, info.CudaComputeCapability, parseErr), err)
			continue
		}

		if cc.Compare(required) < 0 {
			err = errors.Join(fmt.Errorf(
				"%s has insufficient cudaComputeCapability %q, wanted >= %q",
				id,
				info.CudaComputeCapability,
				cudaComputeCapability,
			), err)
		}
	}

	return err
}
