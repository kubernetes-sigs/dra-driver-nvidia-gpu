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
	"path/filepath"
	"testing"
	"time"

	"github.com/NVIDIA/nvidia-container-toolkit/pkg/nvcdi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	utilcache "k8s.io/apimachinery/pkg/util/cache"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

type fakeNVCDI struct {
	nvcdi.Interface

	commonEditsCalls int
	commonEdits      *cdiapi.ContainerEdits
	commonEditsErr   error

	deviceSpecsCalls map[string]int
	deviceSpecs      map[string][]cdispec.Device
	deviceSpecsErrs  map[string]error
}

func (f *fakeNVCDI) GetCommonEdits() (*cdiapi.ContainerEdits, error) {
	f.commonEditsCalls++
	return f.commonEdits, f.commonEditsErr
}

func (f *fakeNVCDI) GetDeviceSpecsByID(ids ...string) ([]cdispec.Device, error) {
	uuid := ids[0]
	f.deviceSpecsCalls[uuid]++
	if err := f.deviceSpecsErrs[uuid]; err != nil {
		return nil, err
	}
	return f.deviceSpecs[uuid], nil
}

func TestGetCommonEditsCached(t *testing.T) {
	newCommonEdits := func() *cdiapi.ContainerEdits {
		return &cdiapi.ContainerEdits{
			ContainerEdits: &cdispec.ContainerEdits{
				Env: []string{"FOO=bar"},
			},
		}
	}

	tests := map[string]struct {
		commonEdits        *cdiapi.ContainerEdits
		commonEditsErr     error
		cachedValue        any
		callCount          int
		wantUnderlyingCall int
		wantErr            string
		checkResults       func(*testing.T, []*cdiapi.ContainerEdits)
	}{
		"cache miss followed by cache hit": {
			commonEdits:        newCommonEdits(),
			callCount:          2,
			wantUnderlyingCall: 1,
			checkResults: func(t *testing.T, results []*cdiapi.ContainerEdits) {
				require.Len(t, results, 2)
				assert.NotSame(t, results[0], results[1])
				assert.Equal(t, results[0], results[1])
			},
		},
		"underlying errors are not cached": {
			commonEditsErr:     errors.New("mock error"),
			callCount:          2,
			wantUnderlyingCall: 2,
			wantErr:            "mock error",
		},
		"invalid cached value returns an error": {
			cachedValue:        "invalid value",
			callCount:          1,
			wantUnderlyingCall: 0,
			wantErr:            "expected *cdiapi.ContainerEdits",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			fakeNVCDIClaim := &fakeNVCDI{
				commonEdits:    tc.commonEdits,
				commonEditsErr: tc.commonEditsErr,
			}
			handler := &CDIHandler{
				nvcdiClaim: fakeNVCDIClaim,
				specCache:  utilcache.NewExpiring(),
			}

			if tc.cachedValue != nil {
				handler.specCache.Set(
					"commonEdits",
					tc.cachedValue,
					5*time.Minute,
				)
			}

			var results []*cdiapi.ContainerEdits
			for range tc.callCount {
				got, err := handler.GetCommonEditsCached()

				if tc.wantErr != "" {
					require.Error(t, err)
					assert.Contains(t, err.Error(), tc.wantErr)
					continue
				}

				require.NoError(t, err)
				results = append(results, got)
			}

			assert.Equal(
				t,
				tc.wantUnderlyingCall,
				fakeNVCDIClaim.commonEditsCalls,
			)

			if tc.checkResults != nil {
				tc.checkResults(t, results)
			}
		})
	}
}

