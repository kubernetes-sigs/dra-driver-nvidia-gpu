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
	"encoding/base64"
	"encoding/json"
	"fmt"

	nvdev "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
)

type GPUAllocatableSnapshot string

func (s *GPUAllocatableSnapshot) Restore() (AllocatableDevices, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	data, err := base64.StdEncoding.DecodeString(string(*s))
	if err != nil {
		return nil, fmt.Errorf("failed to decode allocatable devices snapshot: %w", err)
	}
	var allocatables AllocatableDevices
	err = json.Unmarshal(data, &allocatables)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal allocatable devices snapshot: %w", err)
	}
	return allocatables, nil
}

// UnmarshalJSON restores the concrete value behind the Profile interface.
func (p *MigProfileInfo) UnmarshalJSON(data []byte) error {
	type profileInfo MigProfileInfo
	decoded := struct {
		profileInfo
		Profile json.RawMessage
	}{profileInfo: profileInfo(*p)}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("failed to unmarshal MIG profile info: %w", err)
	}
	if len(decoded.Profile) != 0 {
		var profile *nvdev.MigProfileInfo
		if err := json.Unmarshal(decoded.Profile, &profile); err != nil {
			return fmt.Errorf("failed to unmarshal MIG profile: %w", err)
		}
		decoded.profileInfo.Profile = nil
		// Assigning a nil concrete pointer would create a non-nil interface.
		if profile != nil {
			decoded.profileInfo.Profile = profile
		}
	}
	*p = MigProfileInfo(decoded.profileInfo)
	return nil
}

// UnmarshalJSON restores concrete values for the CandidateProfiles interfaces.
func (m *MigSpec) UnmarshalJSON(data []byte) error {
	type migSpec MigSpec
	decoded := struct {
		migSpec
		CandidateProfiles json.RawMessage
	}{migSpec: migSpec(*m)}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("failed to unmarshal MIG spec: %w", err)
	}
	if len(decoded.CandidateProfiles) != 0 {
		var profiles []*nvdev.MigProfileInfo
		if err := json.Unmarshal(decoded.CandidateProfiles, &profiles); err != nil {
			return fmt.Errorf("failed to unmarshal MIG candidate profiles: %w", err)
		}
		decoded.migSpec.CandidateProfiles = nil
		if profiles != nil {
			decoded.migSpec.CandidateProfiles = make([]nvdev.MigProfile, len(profiles))
			for i, profile := range profiles {
				if profile != nil {
					decoded.migSpec.CandidateProfiles[i] = profile
				}
			}
		}
	}
	*m = MigSpec(decoded.migSpec)
	return nil
}
