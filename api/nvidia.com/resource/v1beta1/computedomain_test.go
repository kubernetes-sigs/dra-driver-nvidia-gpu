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

package v1beta1_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const computeDomainCRDPath = "../../../../deployments/helm/dra-driver-nvidia-gpu/crds/resource.nvidia.com_computedomains.yaml"

// nameSchema returns the generated schema of
// spec.channel.resourceClaimTemplate.name for every served version.
func nameSchema(t *testing.T) map[string]map[string]any {
	t.Helper()

	data, err := os.ReadFile(computeDomainCRDPath)
	require.NoError(t, err)

	var crd map[string]any
	require.NoError(t, yaml.Unmarshal(data, &crd))

	schemas := map[string]map[string]any{}
	versions := field[[]any](t, crd, "spec", "versions")
	require.NotEmpty(t, versions)
	for _, v := range versions {
		version := v.(map[string]any)
		schemas[version["name"].(string)] = field[map[string]any](t, version,
			"schema", "openAPIV3Schema", "properties", "spec", "properties", "channel",
			"properties", "resourceClaimTemplate", "properties", "name")
	}
	return schemas
}

func field[T any](t *testing.T, obj map[string]any, path ...string) T {
	t.Helper()
	var cur any = obj
	for _, p := range path {
		m, ok := cur.(map[string]any)
		require.True(t, ok, "expected an object at %q in %v", p, path)
		cur, ok = m[p]
		require.True(t, ok, "missing %q in %v", p, path)
	}
	v, ok := cur.(T)
	require.True(t, ok, "unexpected type %T at %v", cur, path)
	return v
}

// The API server enforces these schema constraints at admission, so check
// them in the generated CRD rather than only in the controller.
func TestComputeDomainCRDResourceClaimTemplateName(t *testing.T) {
	label := strings.Repeat("a", 63)
	name253 := strings.Join([]string{label, label, label, strings.Repeat("a", 61)}, ".")
	require.Len(t, name253, 253)

	tests := map[string]struct {
		name  string
		valid bool
	}{
		"single character":         {name: "a", valid: true},
		"dotted":                   {name: "imex.channel-0", valid: true},
		"253 characters":           {name: name253, valid: true},
		"empty":                    {name: "", valid: false},
		"uppercase":                {name: "Imex-Channel", valid: false},
		"double dot":               {name: "a..b", valid: false},
		"leading dash":             {name: "-a", valid: false},
		"254 characters":           {name: name253 + "a", valid: false},
		"embedded newline":         {name: "imex-channel-0\n  namespace: other", valid: false},
		"trailing space":           {name: "imex-channel-0 ", valid: false},
		"flow mapping":             {name: "{namespace: other}", valid: false},
		"path separator":           {name: "a/b", valid: false},
		"trailing dot":             {name: "a.", valid: false},
		"single dash inside label": {name: "a-b", valid: true},
	}

	for version, schema := range nameSchema(t) {
		require.Equal(t, "string", schema["type"], version)
		pattern := regexp.MustCompile(field[string](t, schema, "pattern"))
		minLength := int(field[int64](t, schema, "minLength"))
		maxLength := int(field[int64](t, schema, "maxLength"))

		for name, test := range tests {
			t.Run(version+"/"+name, func(t *testing.T) {
				valid := len(test.name) >= minLength && len(test.name) <= maxLength && pattern.MatchString(test.name)
				require.Equal(t, test.valid, valid)
			})
		}
	}
}
