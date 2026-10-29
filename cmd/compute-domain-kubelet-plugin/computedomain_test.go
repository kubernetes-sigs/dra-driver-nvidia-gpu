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

package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRefreshGPUCliqueIDRepublishesResources(t *testing.T) {
	manager := &ComputeDomainManager{
		config:          &Config{flags: &Flags{}},
		cliqueID:        "fabric-a",
		getCliqueIDFunc: func() (string, error) { return "fabric-b", nil },
	}
	var published string
	manager.SetCliqueIDChangeCallback(func(_ context.Context, fabricDomainID string) error {
		published = fabricDomainID
		return nil
	})

	require.NoError(t, manager.refreshGPUCliqueID(context.Background()))
	require.Equal(t, "fabric-b", manager.CliqueID())
	require.Equal(t, "fabric-b", published)
}

func TestRefreshGPUCliqueIDRestoresIdentityAfterPublishFailure(t *testing.T) {
	manager := &ComputeDomainManager{
		config:          &Config{flags: &Flags{}},
		cliqueID:        "fabric-a",
		getCliqueIDFunc: func() (string, error) { return "fabric-b", nil },
	}
	manager.SetCliqueIDChangeCallback(func(context.Context, string) error {
		return fmt.Errorf("publication failed")
	})

	require.ErrorContains(t, manager.refreshGPUCliqueID(context.Background()), "publication failed")
	require.Equal(t, "fabric-a", manager.CliqueID())
}
