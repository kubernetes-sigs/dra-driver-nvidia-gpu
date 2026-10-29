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
	"testing"

	"github.com/stretchr/testify/require"
	coreapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestChannelReservationRegistry(t *testing.T) {
	registry := &coreapi.ConfigMap{}
	reservation := ChannelReservation{FabricDomainID: "fabric-a", ChannelID: 7}
	require.NoError(t, SetChannelReservation(registry, "domain-a", reservation))
	require.Equal(t, "7", registry.Data["domain-a"])
	require.JSONEq(t, `{"domain-a":"fabric-a"}`, registry.Annotations[ChannelReservationDomainsAnnotation])

	got, exists, err := ChannelReservationFor(registry, "domain-a")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, reservation, got)

	require.NoError(t, DeleteChannelReservation(registry, "domain-a"))
	_, exists, err = ChannelReservationFor(registry, "domain-a")
	require.NoError(t, err)
	require.False(t, exists)
	require.JSONEq(t, `{}`, registry.Annotations[ChannelReservationDomainsAnnotation])
}

func TestLegacyChannelReservation(t *testing.T) {
	registry := &coreapi.ConfigMap{Data: map[string]string{"domain-a": "2"}}
	got, exists, err := ChannelReservationFor(registry, "domain-a")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, ChannelReservation{ChannelID: 2}, got)
}

func TestInvalidChannelReservationRegistry(t *testing.T) {
	registry := &coreapi.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{ChannelReservationDomainsAnnotation: "{"}},
		Data:       map[string]string{"domain-a": "0"},
	}
	_, _, err := ChannelReservationFor(registry, "domain-a")
	require.ErrorContains(t, err, "decode IMEX channel reservation fabric domains")
}
