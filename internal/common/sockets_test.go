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

package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const testPodUID = "08a5f01c-b566-45ea-abb9-6522b1bcbc22"

func TestRegistrarSocketPath(t *testing.T) {
	require.Equal(t, "/var/lib/kubelet/plugins_registry/gpu.nvidia.com-reg.sock",
		RegistrarSocketPath("/var/lib/kubelet/plugins_registry", "gpu.nvidia.com", ""))
	require.Equal(t, "/var/lib/kubelet/plugins_registry/gpu.nvidia.com-"+testPodUID+"-reg.sock",
		RegistrarSocketPath("/var/lib/kubelet/plugins_registry", "gpu.nvidia.com", testPodUID))
}

func TestDRASocketPath(t *testing.T) {
	require.Equal(t, "/var/lib/kubelet/plugins/gpu.nvidia.com/dra.sock",
		DRASocketPath("/var/lib/kubelet/plugins/gpu.nvidia.com", ""))
	require.Equal(t, "/var/lib/kubelet/plugins/gpu.nvidia.com/dra-"+testPodUID+".sock",
		DRASocketPath("/var/lib/kubelet/plugins/gpu.nvidia.com", testPodUID))
}
