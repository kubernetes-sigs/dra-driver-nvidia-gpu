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
	"testing"

	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestParseCDDaemonResources(t *testing.T) {
	cases := map[string]struct {
		raw     string
		want    corev1.ResourceRequirements
		wantErr bool
	}{
		"empty string yields empty requirements": {
			raw:  "",
			want: corev1.ResourceRequirements{},
		},
		"whitespace-only string yields empty requirements": {
			raw:  "   ",
			want: corev1.ResourceRequirements{},
		},
		"empty object yields empty requirements": {
			raw:  "{}",
			want: corev1.ResourceRequirements{},
		},
		"requests and limits are decoded": {
			raw: `{"requests":{"cpu":"100m","memory":"128Mi"},"limits":{"cpu":"200m","memory":"256Mi"}}`,
			want: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("200m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
		},
		"requests only is decoded": {
			raw: `{"requests":{"cpu":"250m"}}`,
			want: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("250m"),
				},
			},
		},
		"invalid json is rejected": {
			raw:     `{"requests":`,
			wantErr: true,
		},
		"invalid quantity is rejected": {
			raw:     `{"requests":{"cpu":"not-a-quantity"}}`,
			wantErr: true,
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parseCDDaemonResources(c.raw)
			if c.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.want, got)
		})
	}
}
