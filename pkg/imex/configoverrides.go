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

package imex

import (
	"fmt"
	"sort"
	"strings"
)

// ParseConfigOverrides parses the "KEY1=VALUE1,KEY2=VALUE2," format used to
// thread the resources.computeDomains.imex.config Helm value through the
// IMEX_CONFIG_OVERRIDES environment variable.
//
// Empty segments (including a trailing comma, or an empty string) are
// ignored. Leading/trailing whitespace around keys and values is trimmed.
// A segment with no "=" or an empty key is an error.
func ParseConfigOverrides(csv string) (map[string]string, error) {
	overrides := make(map[string]string)
	for _, pair := range strings.Split(csv, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, value, found := strings.Cut(pair, "=")
		if !found {
			return nil, fmt.Errorf("invalid IMEX config override %q: expected KEY=VALUE", pair)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("invalid IMEX config override %q: empty key", pair)
		}
		overrides[key] = strings.TrimSpace(value)
	}
	return overrides, nil
}

// driverManagedConfigKeys are nvidia-imex config settings the driver computes
// itself at render time. Allowing to override either would silently
// break ComputeDomain node-to-node IMEX connectivity on that node, so these
// are rejected outright rather than applied.
var driverManagedConfigKeys = []string{
	"IMEX_CMD_BIND_INTERFACE_IP",
	"IMEX_NODE_CONFIG_FILE",
}

// ValidateConfigOverrides rejects overrides that target an nvidia-imex config
// setting the driver must own exclusively (see driverManagedConfigKeys).
func ValidateConfigOverrides(overrides map[string]string) error {
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if strings.ContainsAny(key, "\r\n") {
			return fmt.Errorf("invalid IMEX config override key %q: must not contain a newline", key)
		}
		if strings.ContainsAny(overrides[key], "\r\n") {
			return fmt.Errorf("invalid IMEX config override value for %q: must not contain a newline", key)
		}
	}

	for _, key := range driverManagedConfigKeys {
		if _, ok := overrides[key]; ok {
			return fmt.Errorf("%q is managed by the driver and cannot be set via IMEX_CONFIG_OVERRIDES", key)
		}
	}
	return nil
}
