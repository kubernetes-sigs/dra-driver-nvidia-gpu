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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	cperrors "k8s.io/kubernetes/pkg/kubelet/checkpointmanager/errors"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
)

// IsCheckpointCorruptionError reports whether err indicates either soft
// corruption (checksum mismatch) or hard corruption (invalid checkpoint JSON).
func IsCheckpointCorruptionError(err error) bool {
	if errors.Is(err, cperrors.CorruptCheckpointError{}) {
		return true
	}

	var syntaxError *json.SyntaxError
	if errors.As(err, &syntaxError) {
		return true
	}

	var typeError *json.UnmarshalTypeError
	return errors.As(err, &typeError)
}

// DescribeCheckpointCorruption returns an operator-facing description without
// repeating the generic "checkpoint is corrupted" error text.
func DescribeCheckpointCorruption(err error) string {
	if errors.Is(err, cperrors.CorruptCheckpointError{}) {
		return "checksum verification failed"
	}

	return fmt.Sprintf("invalid checkpoint JSON: %v", err)
}

// HasClaimCDISpec checks for per-claim CDI specs belonging to the driver.
// These files persist independently of the checkpoint and are used as the
// prepared-claim signal when the checkpoint itself cannot be trusted.
func HasClaimCDISpec(cdiRoot string, driverName string) (bool, error) {
	entries, err := os.ReadDir(cdiRoot)
	if err != nil {
		return false, fmt.Errorf("read CDI directory %q: %w", cdiRoot, err)
	}

	prefix := cdiapi.GenerateSpecName("k8s."+driverName, "claim") + "_"
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		extension := filepath.Ext(entry.Name())
		if extension == ".yaml" || extension == ".json" {
			return true, nil
		}
	}

	return false, nil
}
