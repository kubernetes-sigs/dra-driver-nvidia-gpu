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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFindCharDeviceHolders(t *testing.T) {
	f, err := os.Open("/dev/null")
	require.NoError(t, err)
	defer f.Close()

	exe, err := os.Executable()
	require.NoError(t, err)
	self := fmt.Sprintf("%d(%s)", os.Getpid(), filepath.Base(exe))

	tests := map[string]struct {
		procRoot     string
		major, minor uint32
		wantSelf     bool
		wantErr      bool
	}{
		"device held by this process": {
			procRoot: "/proc",
			major:    1,
			minor:    3,
			wantSelf: true,
		},
		"device not held by this process": {
			procRoot: "/proc",
			major:    nvidiaDeviceMajor,
			minor:    4095,
		},
		"missing proc root": {
			procRoot: filepath.Join(t.TempDir(), "missing"),
			major:    1,
			minor:    3,
			wantErr:  true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			holders, err := findCharDeviceHolders(tc.procRoot, tc.major, tc.minor)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tc.wantSelf {
				require.Contains(t, holders, self)
			} else {
				require.NotContains(t, holders, self)
			}
		})
	}
}

func TestWaitForGPUFree(t *testing.T) {
	vm := &VfioPciManager{}
	info := &VfioDeviceInfo{parent: &GpuInfo{minor: 4095}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, vm.WaitForGPUFree(ctx, info))
}
