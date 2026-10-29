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
	"encoding/json"
	"fmt"
	"strconv"

	coreapi "k8s.io/api/core/v1"
)

// ChannelReservationsConfigMap stores ComputeDomain UID to channel ID mappings.
// Only the ComputeDomain controller writes it; kubelet plugins validate claims
// against it before exposing a channel from a host IMEX fabric domain.
const ChannelReservationsConfigMap = "compute-domain-imex-channel-reservations"

const (
	// FabricDomainIDAttribute is the ResourceSlice attribute containing the
	// canonical <clusterUUID>.<cliqueID> identity discovered from NVML.
	FabricDomainIDAttribute = "fabricDomainID"

	// ChannelReservationDomainsAnnotation stores a JSON object mapping
	// ComputeDomain UIDs to fabric-domain IDs. Channel IDs remain in ConfigMap
	// Data for compatibility with older kubelet plugins.
	ChannelReservationDomainsAnnotation = "compute-domain.nvidia.com/channel-reservation-domains"
)

// ChannelReservation identifies a channel within one physical NVLink fabric
// domain. An empty FabricDomainID denotes a legacy cluster-global reservation.
type ChannelReservation struct {
	FabricDomainID string
	ChannelID      int
}

// ChannelReservationFor returns a ComputeDomain's reservation from the shared
// registry. Reservations without a fabric-domain annotation are legacy global
// reservations.
func ChannelReservationFor(registry *coreapi.ConfigMap, uid string) (ChannelReservation, bool, error) {
	value, exists := registry.Data[uid]
	if !exists {
		return ChannelReservation{}, false, nil
	}
	id, err := strconv.Atoi(value)
	if err != nil || id < 0 {
		return ChannelReservation{}, false, fmt.Errorf("invalid channel reservation for ComputeDomain %s: %q", uid, value)
	}
	domains, err := channelReservationDomains(registry)
	if err != nil {
		return ChannelReservation{}, false, err
	}
	return ChannelReservation{FabricDomainID: domains[uid], ChannelID: id}, true, nil
}

// SetChannelReservation records a fabric-domain-scoped reservation while
// retaining the legacy numeric ConfigMap data representation.
func SetChannelReservation(registry *coreapi.ConfigMap, uid string, reservation ChannelReservation) error {
	if reservation.FabricDomainID == "" || reservation.ChannelID < 0 {
		return fmt.Errorf("invalid channel reservation for ComputeDomain %s: %+v", uid, reservation)
	}
	if registry.Data == nil {
		registry.Data = make(map[string]string)
	}
	registry.Data[uid] = strconv.Itoa(reservation.ChannelID)

	domains, err := channelReservationDomains(registry)
	if err != nil {
		return err
	}
	domains[uid] = reservation.FabricDomainID
	return setChannelReservationDomains(registry, domains)
}

// DeleteChannelReservation removes both parts of a ComputeDomain reservation.
func DeleteChannelReservation(registry *coreapi.ConfigMap, uid string) error {
	delete(registry.Data, uid)
	domains, err := channelReservationDomains(registry)
	if err != nil {
		return err
	}
	delete(domains, uid)
	return setChannelReservationDomains(registry, domains)
}

func channelReservationDomains(registry *coreapi.ConfigMap) (map[string]string, error) {
	domains := make(map[string]string)
	if registry.Annotations == nil || registry.Annotations[ChannelReservationDomainsAnnotation] == "" {
		return domains, nil
	}
	if err := json.Unmarshal([]byte(registry.Annotations[ChannelReservationDomainsAnnotation]), &domains); err != nil {
		return nil, fmt.Errorf("decode IMEX channel reservation fabric domains: %w", err)
	}
	return domains, nil
}

func setChannelReservationDomains(registry *coreapi.ConfigMap, domains map[string]string) error {
	encoded, err := json.Marshal(domains)
	if err != nil {
		return fmt.Errorf("encode IMEX channel reservation fabric domains: %w", err)
	}
	if registry.Annotations == nil {
		registry.Annotations = make(map[string]string)
	}
	registry.Annotations[ChannelReservationDomainsAnnotation] = string(encoded)
	return nil
}
