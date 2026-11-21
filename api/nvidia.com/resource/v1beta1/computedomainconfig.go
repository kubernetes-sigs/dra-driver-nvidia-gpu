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

package v1beta1

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/validate/content"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ValidateDomainID reports whether domainID is safe to use as a single path
// component when constructing on-disk paths for a ComputeDomain.
// DomainID always originates from an opaque, claim-author-controlled device
// configuration, so it must never be allowed to contain a path separator or
// resolve to "." or "..": either would let it escape the directory it is
// joined into.
func ValidateDomainID(domainID string) error {
	if domainID == "" {
		return fmt.Errorf("domainID cannot be empty")
	}
	if errs := content.IsPathSegmentName(domainID); len(errs) > 0 {
		return fmt.Errorf("domainID %q is not a valid single path component: %s", domainID, strings.Join(errs, ", "))
	}
	// DomainID is also a node label value so this rejects over-long or invalid names that
	// pass the path check but would fail later at MkdirAll or the label update.
	if errs := validation.IsValidLabelValue(domainID); len(errs) != 0 {
		return fmt.Errorf("domainID %q must be a valid label value: %s", domainID, strings.Join(errs, "; "))
	}
	return nil
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ComputeDomainChannelConfig holds the set of parameters for configuring an ComputeDomainChannel.
type ComputeDomainChannelConfig struct {
	metav1.TypeMeta `json:",inline"`
	DomainID        string `json:"domainID"`
	AllocationMode  string `json:"allocationMode,omitempty"`
}

// DefaultComputeDomainChannelConfig provides the default ComputeDomainChannel configuration.
func DefaultComputeDomainChannelConfig() *ComputeDomainChannelConfig {
	return &ComputeDomainChannelConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: GroupName + "/" + Version,
			Kind:       ComputeDomainChannelConfigKind,
		},
	}
}

// Normalize updates a ComputeDomainChannelConfig config with implied default values based on other settings.
func (c *ComputeDomainChannelConfig) Normalize() error {
	return nil
}

// Validate ensures that ComputeDomainChannelConfig has a valid set of values.
func (c *ComputeDomainChannelConfig) Validate() error {
	if err := ValidateDomainID(c.DomainID); err != nil {
		return err
	}
	// Unlike the ComputeDomain CRD field, this opaque config gets no schema
	// enum check, and the kubelet plugin treats anything other than All as
	// Single, so a typo would otherwise be silently ignored. Empty is allowed
	// because the field is optional and implies Single.
	switch c.AllocationMode {
	case "", ComputeDomainChannelAllocationModeSingle, ComputeDomainChannelAllocationModeAll:
		return nil
	default:
		return fmt.Errorf("unknown channel allocation mode: %v, supported channel allocation modes: %s, %s",
			c.AllocationMode, ComputeDomainChannelAllocationModeSingle, ComputeDomainChannelAllocationModeAll)
	}
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ComputeDomainDaemonConfig holds the set of parameters for configuring an ComputeDomainDaemon.
type ComputeDomainDaemonConfig struct {
	metav1.TypeMeta `json:",inline"`
	DomainID        string `json:"domainID"`
}

// DefaultComputeDomainDaemonConfig provides the default ComputeDomainDaemon configuration.
func DefaultComputeDomainDaemonConfig() *ComputeDomainDaemonConfig {
	return &ComputeDomainDaemonConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: GroupName + "/" + Version,
			Kind:       ComputeDomainDaemonConfigKind,
		},
	}
}

// Normalize updates a ComputeDomainDaemonConfig config with implied default values based on other settings.
func (c *ComputeDomainDaemonConfig) Normalize() error {
	return nil
}

// Validate ensures that ComputeDomainDaemonConfig has a valid set of values.
func (c *ComputeDomainDaemonConfig) Validate() error {
	return ValidateDomainID(c.DomainID)
}
