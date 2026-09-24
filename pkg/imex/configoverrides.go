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
