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

// Package healthcontrol parses the mock NVML health-control file used to
// simulate XID events and GPU recovery actions in CPU-only e2e tests.
//
// The file is re-read on every claim so a test can change the recovery action
// after an event has already been delivered. Delivered event IDs are remembered
// for the life of the process, matching NVML's once-per-occurrence event
// semantics. Recovery actions are not sticky: the next read returns whatever
// the file currently says, which is how a test transitions a GPU back to NONE.
package healthcontrol

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// FullGPUInstanceID is the GI/CI value NVML reports for an event that applies
// to the whole GPU rather than a MIG instance.
const FullGPUInstanceID uint32 = 0xFFFFFFFF

// Recovery action values match nvmlDeviceGpuRecoveryAction_t.
const (
	RecoveryActionNone              uint32 = 0
	RecoveryActionGPUReset          uint32 = 1
	RecoveryActionNodeReboot        uint32 = 2
	RecoveryActionDrainP2P          uint32 = 3
	RecoveryActionDrainAndReset     uint32 = 4
	RecoveryActionRecoverIMEXDomain uint32 = 5
	RecoveryActionBusReset          uint32 = 6
	RecoveryActionSystemReboot      uint32 = 7
)

const defaultHealthControlPath = "/driver-root/health-control.json"

// HealthControlPath returns the control file path. The GPU plugin mounts the
// mock driver root at /driver-root, so that is the default seen by libnvidia-ml.
func HealthControlPath() string {
	if path := os.Getenv("MOCK_NVML_HEALTH_CONTROL"); path != "" {
		return path
	}
	return defaultHealthControlPath
}

// HealthEvent is one nvmlEventTypeXidCriticalError to deliver.
type HealthEvent struct {
	ID                string `json:"id,omitempty"`
	GPU               int    `json:"gpu"`
	XID               uint64 `json:"xid"`
	GPUInstanceID     uint32 `json:"gpuInstanceId"`
	ComputeInstanceID uint32 `json:"computeInstanceId"`
}

// HealthDocument is the on-disk health-control document.
type HealthDocument struct {
	Events          []HealthEvent    `json:"events,omitempty"`
	RecoveryActions []recoveryAction `json:"recoveryActions,omitempty"`
	recoveryByGPU   map[int]uint32
}

type recoveryAction struct {
	GPU    int    `json:"gpu"`
	Action string `json:"action"`
}

type healthEventJSON struct {
	ID                string  `json:"id,omitempty"`
	GPU               int     `json:"gpu"`
	XID               uint64  `json:"xid"`
	GPUInstanceID     *uint32 `json:"gpuInstanceId,omitempty"`
	ComputeInstanceID *uint32 `json:"computeInstanceId,omitempty"`
}

type healthDocumentJSON struct {
	Events          []healthEventJSON `json:"events,omitempty"`
	RecoveryActions []recoveryAction  `json:"recoveryActions,omitempty"`
}

var recoveryActionByName = map[string]uint32{
	"NONE":                                    RecoveryActionNone,
	"GPU_RECOVERY_ACTION_NONE":                RecoveryActionNone,
	"GPU_RESET":                               RecoveryActionGPUReset,
	"GPU_RECOVERY_ACTION_GPU_RESET":           RecoveryActionGPUReset,
	"NODE_REBOOT":                             RecoveryActionNodeReboot,
	"GPU_RECOVERY_ACTION_NODE_REBOOT":         RecoveryActionNodeReboot,
	"DRAIN_P2P":                               RecoveryActionDrainP2P,
	"GPU_RECOVERY_ACTION_DRAIN_P2P":           RecoveryActionDrainP2P,
	"DRAIN_AND_RESET":                         RecoveryActionDrainAndReset,
	"GPU_RECOVERY_ACTION_DRAIN_AND_RESET":     RecoveryActionDrainAndReset,
	"RECOVER_IMEX_DOMAIN":                     RecoveryActionRecoverIMEXDomain,
	"GPU_RECOVERY_ACTION_RECOVER_IMEX_DOMAIN": RecoveryActionRecoverIMEXDomain,
	"BUS_RESET":                               RecoveryActionBusReset,
	"GPU_RECOVERY_ACTION_BUS_RESET":           RecoveryActionBusReset,
	"SYSTEM_REBOOT":                           RecoveryActionSystemReboot,
	"GPU_RECOVERY_ACTION_SYSTEM_REBOOT":       RecoveryActionSystemReboot,
}

// ParseHealthControl decodes a health-control document. Omitted GI/CI values
// become the full-GPU sentinel so a test that only names a GPU and an XID
// still addresses the advertised full GPU.
func ParseHealthControl(data []byte) (HealthDocument, error) {
	var raw healthDocumentJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return HealthDocument{}, fmt.Errorf("parse health-control file: %w", err)
	}

	doc := HealthDocument{
		Events:          make([]HealthEvent, 0, len(raw.Events)),
		RecoveryActions: raw.RecoveryActions,
		recoveryByGPU:   make(map[int]uint32, len(raw.RecoveryActions)),
	}
	for i, rawEvent := range raw.Events {
		if rawEvent.GPU < 0 {
			return HealthDocument{}, fmt.Errorf("health-control event %d: gpu must be >= 0", i)
		}
		event := HealthEvent{
			ID:                rawEvent.ID,
			GPU:               rawEvent.GPU,
			XID:               rawEvent.XID,
			GPUInstanceID:     FullGPUInstanceID,
			ComputeInstanceID: FullGPUInstanceID,
		}
		if rawEvent.GPUInstanceID != nil {
			event.GPUInstanceID = *rawEvent.GPUInstanceID
		}
		if rawEvent.ComputeInstanceID != nil {
			event.ComputeInstanceID = *rawEvent.ComputeInstanceID
		}
		if event.ID == "" {
			event.ID = fmt.Sprintf("auto-%d-gpu-%d-xid-%d-gi-%d-ci-%d",
				i, event.GPU, event.XID, event.GPUInstanceID, event.ComputeInstanceID)
		}
		doc.Events = append(doc.Events, event)
	}
	for i, action := range raw.RecoveryActions {
		if action.GPU < 0 {
			return HealthDocument{}, fmt.Errorf("health-control recovery action %d: gpu must be >= 0", i)
		}
		value, err := ParseRecoveryAction(action.Action)
		if err != nil {
			return HealthDocument{}, fmt.Errorf("health-control recovery action %d: %w", i, err)
		}
		doc.recoveryByGPU[action.GPU] = value
	}
	return doc, nil
}

// ParseRecoveryAction accepts the short name (GPU_RESET) and the NVML enum
// name (GPU_RECOVERY_ACTION_GPU_RESET).
func ParseRecoveryAction(action string) (uint32, error) {
	name := strings.ToUpper(strings.TrimSpace(action))
	value, ok := recoveryActionByName[name]
	if !ok {
		return 0, fmt.Errorf("unknown GPU recovery action %q", action)
	}
	return value, nil
}

// RecoveryAction returns the configured action for a GPU, or NONE when the
// file does not mention that GPU.
func (d HealthDocument) RecoveryAction(gpu int) uint32 {
	if d.recoveryByGPU == nil {
		return RecoveryActionNone
	}
	action, ok := d.recoveryByGPU[gpu]
	if !ok {
		return RecoveryActionNone
	}
	return action
}

// HealthStore tracks which control-file events have already been delivered.
type HealthStore struct {
	path      string
	mu        sync.Mutex
	doc       HealthDocument
	sum       [sha256.Size]byte
	loaded    bool
	delivered map[string]struct{}
}

// NewHealthStore watches path. A missing file reports no events and recovery
// action NONE.
func NewHealthStore(path string) *HealthStore {
	return &HealthStore{
		path:      path,
		delivered: make(map[string]struct{}),
	}
}

var (
	defaultHealthStoreOnce sync.Once
	defaultHealthStore     *HealthStore
)

// DefaultHealthStore is the process-wide store backed by HealthControlPath.
func DefaultHealthStore() *HealthStore {
	defaultHealthStoreOnce.Do(func() {
		defaultHealthStore = NewHealthStore(HealthControlPath())
	})
	return defaultHealthStore
}

// Claim returns the next undelivered XID event. The same ID is not returned
// again, including across rewrites of the file.
func (s *HealthStore) Claim() (HealthEvent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.refreshLocked()
	for _, event := range s.doc.Events {
		if _, seen := s.delivered[event.ID]; seen {
			continue
		}
		s.delivered[event.ID] = struct{}{}
		return event, true
	}
	return HealthEvent{}, false
}

// RecoveryAction returns the current recovery action for gpu.
func (s *HealthStore) RecoveryAction(gpu int) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.refreshLocked()
	return s.doc.RecoveryAction(gpu)
}

func (s *HealthStore) refreshLocked() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.doc = HealthDocument{}
			s.sum = [sha256.Size]byte{}
			s.loaded = true
		}
		return
	}

	sum := sha256.Sum256(data)
	if s.loaded && sum == s.sum {
		return
	}
	doc, err := ParseHealthControl(data)
	if err != nil {
		// Keep the last valid document so a partial write cannot drop a
		// recovery action the driver is about to query.
		return
	}
	s.doc = doc
	s.sum = sum
	s.loaded = true
}
