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
	"encoding/json"
	"fmt"

	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager/checksum"
)

const CheckpointVersion = "v2"

type Checkpoint struct {
	// Version records the latest checkpoint version written, allowing readers
	// to quickly determine the format without inspecting nested fields.
	Version string `json:"version,omitempty"`
	// Note: The Checksum below is only associated with the V1 checkpoint
	// (because it doesn't have an embedded one). All future versions have
	// their checksum directly embedded in them to better support
	// downgrades. This checksum will be removed once the V1 checkpoint is
	// no longer supported.
	Checksum checksum.Checksum `json:"checksum"`
	V1       *CheckpointV1     `json:"v1,omitempty"`
	V2       *CheckpointV2     `json:"v2,omitempty"`
	// other holds unknown fields from a newer checkpoint format, preserved
	// so that a downgraded driver can round-trip data it does not understand.
	other map[string]json.RawMessage
}

// MarshalJSON implements json.Marshaler, merging known fields with any
// unknown fields captured from a newer checkpoint format.
func (cp *Checkpoint) MarshalJSON() ([]byte, error) {
	type Alias struct {
		Version  string            `json:"version,omitempty"`
		Checksum checksum.Checksum `json:"checksum"`
		V1       *CheckpointV1     `json:"v1,omitempty"`
		V2       *CheckpointV2     `json:"v2,omitempty"`
	}
	known, err := json.Marshal(&Alias{
		Version:  cp.Version,
		Checksum: cp.Checksum,
		V1:       cp.V1,
		V2:       cp.V2,
	})
	if err != nil {
		return nil, err
	}
	if len(cp.other) == 0 {
		return known, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(known, &merged); err != nil {
		return nil, err
	}
	for k, v := range cp.other {
		merged[k] = v
	}
	return json.Marshal(merged)
}

// UnmarshalJSON implements json.Unmarshaler, populating known fields and
// preserving any unrecognised fields (future versions) in cp.other.
func (cp *Checkpoint) UnmarshalJSON(data []byte) error {
	type Alias struct {
		Version  string            `json:"version,omitempty"`
		Checksum checksum.Checksum `json:"checksum"`
		V1       *CheckpointV1     `json:"v1,omitempty"`
		V2       *CheckpointV2     `json:"v2,omitempty"`
	}
	var alias Alias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	cp.Version = alias.Version
	cp.Checksum = alias.Checksum
	cp.V1 = alias.V1
	cp.V2 = alias.V2

	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	delete(all, "version")
	delete(all, "checksum")
	delete(all, "v1")
	delete(all, "v2")
	if len(all) > 0 {
		cp.other = all
	} else {
		cp.other = nil
	}
	return nil
}

func (cp *Checkpoint) DeepCopy() *Checkpoint {
	if cp == nil {
		return nil
	}
	out := &Checkpoint{
		Version:  cp.Version,
		Checksum: cp.Checksum,
		V1:       cp.V1.DeepCopy(),
		V2:       cp.V2.DeepCopy(),
	}
	if len(cp.other) > 0 {
		out.other = make(map[string]json.RawMessage, len(cp.other))
		for k, v := range cp.other {
			raw := make(json.RawMessage, len(v))
			copy(raw, v)
			out.other[k] = raw
		}
	}
	return out
}

func (cp *Checkpoint) ToLatestVersion() *Checkpoint {
	latest := &Checkpoint{}
	switch {
	case cp.V2 != nil:
		latest.V2 = cp.V2
	case cp.V1 != nil:
		latest.V2 = cp.V1.ToV2()
	default:
		latest.V2 = &CheckpointV2{}
	}
	if latest.V2.PreparedClaims == nil {
		latest.V2.PreparedClaims = make(PreparedClaimsByUID)
	}
	return latest
}

func (cp *Checkpoint) MarshalCheckpoint() ([]byte, error) {
	cp = cp.ToLatestVersion()
	cp.Version = CheckpointVersion
	cp.V1 = cp.V2.ToV1()
	if err := cp.SetChecksumV1(); err != nil {
		return nil, fmt.Errorf("error setting v1 checksum: %v", err)
	}
	if err := cp.SetChecksumV2(); err != nil {
		return nil, fmt.Errorf("error setting v2 checksum: %v", err)
	}
	return json.Marshal(cp)
}

// SetChecksumV1 computes and sets the V1 checksum, which covers only the
// V1 view of the checkpoint (Version, V2, and other are excluded so that
// older drivers computing the same checksum get identical JSON).
func (cp *Checkpoint) SetChecksumV1() error {
	type v1View struct {
		Checksum checksum.Checksum `json:"checksum"`
		V1       *CheckpointV1     `json:"v1,omitempty"`
	}
	view := v1View{V1: cp.V1}
	out, err := json.Marshal(view)
	if err != nil {
		return err
	}
	cp.Checksum = checksum.New(out)
	return nil
}

func (cp *Checkpoint) SetChecksumV2() error {
	cp.V2.Checksum = 0
	out, err := json.Marshal(*cp.V2)
	if err != nil {
		return err
	}
	cp.V2.Checksum = checksum.New(out)
	return nil
}

func (cp *Checkpoint) UnmarshalCheckpoint(data []byte) error {
	return json.Unmarshal(data, cp)
}

func (cp *Checkpoint) VerifyChecksum() error {
	if err := cp.VerifyChecksumV1(); err != nil {
		return err
	}
	if err := cp.VerifyChecksumV2(); err != nil {
		return err
	}
	return nil
}

// VerifyChecksumV1 verifies the V1 checksum using the same V1-only view that
// SetChecksumV1 used, ensuring older drivers can also verify successfully.
func (cp *Checkpoint) VerifyChecksumV1() error {
	type v1View struct {
		Checksum checksum.Checksum `json:"checksum"`
		V1       *CheckpointV1     `json:"v1,omitempty"`
	}
	view := v1View{V1: cp.V1}
	out, err := json.Marshal(view)
	if err != nil {
		return err
	}
	return cp.Checksum.Verify(out)
}

func (cp *Checkpoint) VerifyChecksumV2() error {
	if cp.V2 == nil {
		return nil
	}

	ck := cp.V2.Checksum
	defer func() {
		cp.V2.Checksum = ck
	}()
	cp.V2.Checksum = 0
	out, err := json.Marshal(*cp.V2)
	if err != nil {
		return err
	}
	return ck.Verify(out)
}
