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
	"path"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

// RegistrarSocketPath returns the path of the registration socket that the
// kubelet plugin helper listens on. With a pod UID, the library picks a
// per-pod name; see kubeletplugin.RollingUpdateRegistrarSocketFile.
func RegistrarSocketPath(registrarDir, driverName, podUID string) string {
	if podUID != "" {
		return path.Join(registrarDir, kubeletplugin.RollingUpdateRegistrarSocketFile(registrarDir, driverName, types.UID(podUID)))
	}
	return path.Join(registrarDir, driverName+"-reg.sock")
}

// DRASocketPath returns the path of the DRA gRPC socket that the kubelet
// plugin helper listens on. The library does not export this name: it uses
// "dra.sock", or "dra-<pod UID>.sock" with a pod UID (see Start in
// vendor/k8s.io/dynamic-resource-allocation/kubeletplugin/draplugin.go).
// Update this function when the vendored library changes that name.
func DRASocketPath(pluginDir, podUID string) string {
	if podUID != "" {
		return path.Join(pluginDir, "dra-"+podUID+".sock")
	}
	return path.Join(pluginDir, "dra.sock")
}
