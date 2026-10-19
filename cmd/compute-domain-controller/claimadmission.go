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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
)

const (
	// nodeOwnershipSweepInterval bounds how long a node stays owned after a
	// missed claim event.
	nodeOwnershipSweepInterval = time.Minute

	deviceTaintRuleKindNode = "Node"

	// maxObjectNameLength is the DNS subdomain name limit.
	maxObjectNameLength = 253
)

// ClaimAdmissionManager admits or rejects allocated channels through their
// binding conditions, before the scheduler binds the pod. Deciding in one
// place avoids races between the first claims of different ComputeDomains.
//
// Driver-managed: a node runs the IMEX daemon of one ComputeDomain, so claims
// are only admitted on nodes their ComputeDomain owns. Ownership is a per-node
// DeviceTaintRule, created atomically by the first claim and deleted once the
// owner has no claim left on the node.
//
// Host-managed: claims are only admitted on channels reserved for their
// ComputeDomain.
//
// It runs regardless of the ComputeDomainChannelReservations gate: with the
// gate off it admits all channels, so that kubelet plugins enabling the gate
// first cannot hold up pods, and removes leftover DeviceTaintRules.
type ClaimAdmissionManager struct {
	config           *ManagerConfig
	getComputeDomain GetComputeDomainFunc
	waitGroup        sync.WaitGroup
	cancelContext    context.CancelFunc

	informer cache.SharedIndexInformer

	// nodeLocks serializes ownership decisions and releases per node.
	nodeLocksMu sync.Mutex
	nodeLocks   map[string]*sync.Mutex
}

func NewClaimAdmissionManager(config *ManagerConfig, getComputeDomain GetComputeDomainFunc) *ClaimAdmissionManager {
	informer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
				return config.clientsets.Core.ResourceV1().ResourceClaims("").List(ctx, options)
			},
			WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
				return config.clientsets.Core.ResourceV1().ResourceClaims("").Watch(ctx, options)
			},
		},
		&resourceapi.ResourceClaim{},
		informerResyncPeriod,
		cache.Indexers{},
	)

	return &ClaimAdmissionManager{
		config:           config,
		getComputeDomain: getComputeDomain,
		informer:         informer,
		nodeLocks:        make(map[string]*sync.Mutex),
	}
}

func (m *ClaimAdmissionManager) Start(ctx context.Context) (rerr error) {
	ctx, cancel := context.WithCancel(ctx)
	m.cancelContext = cancel

	defer func() {
		if rerr != nil {
			if err := m.Stop(); err != nil {
				klog.Errorf("error stopping ClaimAdmission manager: %v", err)
			}
		}
	}()

	_, err := m.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			m.enqueueAdmission(obj)
		},
		UpdateFunc: func(oldObj, newObj any) {
			m.enqueueAdmission(newObj)
			if releasedChannels(oldObj, newObj) {
				m.enqueueReleases(oldObj)
			}
		},
		DeleteFunc: func(obj any) {
			if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = tombstone.Obj
			}
			m.enqueueReleases(obj)
		},
	})
	if err != nil {
		return fmt.Errorf("error adding event handlers for ResourceClaim informer: %w", err)
	}

	m.waitGroup.Go(func() {
		m.informer.Run(ctx.Done())
	})

	if !cache.WaitForCacheSync(ctx.Done(), m.informer.HasSynced) {
		return fmt.Errorf("informer cache sync for ResourceClaims failed")
	}

	if m.config.deviceTaintRules != nil {
		m.waitGroup.Go(func() {
			m.periodicDeviceTaintRuleSweep(ctx)
		})
	}

	return nil
}

func (m *ClaimAdmissionManager) Stop() error {
	if m.cancelContext != nil {
		m.cancelContext()
	}
	m.waitGroup.Wait()
	return nil
}

func (m *ClaimAdmissionManager) enqueueAdmission(obj any) {
	claim, ok := obj.(*resourceapi.ResourceClaim)
	if !ok || len(pendingChannelResults(claim)) == 0 {
		return
	}
	m.config.workQueue.EnqueueWithKey(claim, "admit/"+string(claim.UID), m.admit)
}

// enqueueReleases rechecks ownership of the nodes a claim had channels on.
func (m *ClaimAdmissionManager) enqueueReleases(obj any) {
	if !channelReservationsEnabled() || m.config.imexConfig.EffectiveHostManaged() {
		return
	}
	claim, ok := obj.(*resourceapi.ResourceClaim)
	if !ok {
		return
	}
	for _, result := range channelResults(claim) {
		m.enqueueRelease(result.Pool)
	}
}

// releasedChannels reports whether an update deallocated or started deleting
// the claim, so that resyncs do not trigger release checks.
func releasedChannels(oldObj, newObj any) bool {
	oldClaim, ok := oldObj.(*resourceapi.ResourceClaim)
	if !ok {
		return false
	}
	newClaim, ok := newObj.(*resourceapi.ResourceClaim)
	if !ok {
		return false
	}
	if len(channelResults(oldClaim)) == 0 {
		return false
	}
	return len(channelResults(newClaim)) == 0 || (oldClaim.DeletionTimestamp == nil && newClaim.DeletionTimestamp != nil)
}

func (m *ClaimAdmissionManager) enqueueRelease(node string) {
	m.config.workQueue.EnqueueRawWithKey(node, "release-node/"+node, m.releaseNode)
}

// periodicDeviceTaintRuleSweep releases nodes whose claim events were missed
// and, with the gate off, removes leftover DeviceTaintRules.
func (m *ClaimAdmissionManager) periodicDeviceTaintRuleSweep(ctx context.Context) {
	ticker := time.NewTicker(nodeOwnershipSweepInterval)
	defer ticker.Stop()
	for {
		if err := m.sweepDeviceTaintRules(ctx); err != nil {
			klog.Errorf("Error sweeping DeviceTaintRules: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *ClaimAdmissionManager) sweepDeviceTaintRules(ctx context.Context) error {
	if !channelReservationsEnabled() {
		return m.config.deviceTaintRules.DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{
			LabelSelector: deviceTaintRuleKindLabelKey,
		})
	}
	if m.config.imexConfig.EffectiveHostManaged() {
		return nil
	}
	rules, err := m.config.deviceTaintRules.List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", deviceTaintRuleKindLabelKey, deviceTaintRuleKindNode),
	})
	if err != nil {
		return fmt.Errorf("error listing node ownership DeviceTaintRules: %w", err)
	}
	for _, rule := range rules {
		if rule.Spec.DeviceSelector != nil && rule.Spec.DeviceSelector.Pool != nil {
			m.enqueueRelease(*rule.Spec.DeviceSelector.Pool)
		}
	}
	return nil
}

func (m *ClaimAdmissionManager) lockNode(node string) func() {
	m.nodeLocksMu.Lock()
	mu, ok := m.nodeLocks[node]
	if !ok {
		mu = &sync.Mutex{}
		m.nodeLocks[node] = mu
	}
	m.nodeLocksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// channelResults returns the claim's allocated channels. Each kubelet plugin
// publishes one pool, named after its node.
func channelResults(claim *resourceapi.ResourceClaim) []resourceapi.DeviceRequestAllocationResult {
	if claim.Status.Allocation == nil {
		return nil
	}
	var results []resourceapi.DeviceRequestAllocationResult
	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver == DriverName && strings.HasPrefix(result.Device, "channel-") {
			results = append(results, result)
		}
	}
	return results
}

// pendingChannelResults returns allocated channels awaiting a decision.
func pendingChannelResults(claim *resourceapi.ResourceClaim) []resourceapi.DeviceRequestAllocationResult {
	var pending []resourceapi.DeviceRequestAllocationResult
	for _, result := range channelResults(claim) {
		if !slices.Contains(result.BindingConditions, imex.ChannelAdmittedCondition) {
			continue
		}
		status := deviceStatus(claim, &result)
		if status != nil && (meta.IsStatusConditionTrue(status.Conditions, imex.ChannelAdmittedCondition) ||
			meta.IsStatusConditionTrue(status.Conditions, imex.ChannelRejectedCondition)) {
			continue
		}
		pending = append(pending, result)
	}
	return pending
}

func deviceStatus(claim *resourceapi.ResourceClaim, result *resourceapi.DeviceRequestAllocationResult) *resourceapi.AllocatedDeviceStatus {
	for i := range claim.Status.Devices {
		status := &claim.Status.Devices[i]
		if status.Driver == result.Driver && status.Pool == result.Pool && status.Device == result.Device &&
			ptr.Deref(status.ShareID, "") == string(ptr.Deref(result.ShareID, "")) {
			return status
		}
	}
	return nil
}

// channelDomainID returns the domain ID of the channel config applying to a
// request. Like in kubelet plugins, claim configs take precedence over class
// configs, and later configs over earlier ones.
func channelDomainID(claim *resourceapi.ResourceClaim, request string) string {
	if claim.Status.Allocation == nil {
		return ""
	}
	domainID := ""
	for _, source := range []resourceapi.AllocationConfigSource{resourceapi.AllocationConfigSourceClass, resourceapi.AllocationConfigSourceClaim} {
		for _, c := range claim.Status.Allocation.Devices.Config {
			if c.Source != source || (len(c.Requests) > 0 && !slices.Contains(c.Requests, request)) {
				continue
			}
			if id := opaqueChannelConfigDomainID(c.Opaque); id != "" {
				domainID = id
			}
		}
	}
	return domainID
}

// decodeChannelConfigs decodes the claim's configs like the kubelet plugin.
func decodeChannelConfigs(claim *resourceapi.ResourceClaim) error {
	for _, c := range claim.Status.Allocation.Devices.Config {
		if c.Opaque == nil || c.Opaque.Driver != DriverName {
			continue
		}
		if _, err := runtime.Decode(nvapi.StrictDecoder, c.Opaque.Parameters.Raw); err != nil {
			return fmt.Errorf("error decoding config parameters: %w", err)
		}
	}
	return nil
}

func channelID(device string) (int, bool) {
	id, err := strconv.Atoi(strings.TrimPrefix(device, "channel-"))
	return id, err == nil
}

// admissionDecision is the outcome for one allocated channel.
type admissionDecision struct {
	admitted bool
	reason   string
	message  string
}

func admitDecision() admissionDecision {
	return admissionDecision{admitted: true, reason: "Admitted", message: "channel admitted for the ComputeDomain"}
}

func rejectDecision(reason, format string, args ...any) admissionDecision {
	return admissionDecision{reason: reason, message: fmt.Sprintf(format, args...)}
}

func (m *ClaimAdmissionManager) admit(ctx context.Context, obj any) error {
	queued, ok := obj.(*resourceapi.ResourceClaim)
	if !ok {
		return fmt.Errorf("failed to cast to ResourceClaim")
	}
	// Retries must act on the latest state.
	latest, exists, err := m.informer.GetStore().Get(queued)
	if err != nil {
		return fmt.Errorf("error getting ResourceClaim from informer: %w", err)
	}
	if !exists {
		return nil
	}
	cached, ok := latest.(*resourceapi.ResourceClaim)
	if !ok {
		return fmt.Errorf("unexpected object %T in ResourceClaim informer", latest)
	}
	claim := cached.DeepCopy()

	pending := pendingChannelResults(claim)
	if len(pending) == 0 {
		return nil
	}
	for i := range pending {
		decision, err := m.decide(ctx, claim, &pending[i])
		if err != nil {
			return err
		}
		klog.V(2).Infof("Channel %s/%s for ResourceClaim %s/%s: %s (%s)", pending[i].Pool, pending[i].Device, claim.Namespace, claim.Name, decision.reason, decision.message)
		setChannelAdmissionCondition(claim, &pending[i], decision)
	}

	if _, err := m.config.clientsets.Core.ResourceV1().ResourceClaims(claim.Namespace).UpdateStatus(ctx, claim, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("error updating ResourceClaim %s/%s status: %w", claim.Namespace, claim.Name, err)
	}
	return nil
}

func (m *ClaimAdmissionManager) decide(ctx context.Context, claim *resourceapi.ResourceClaim, result *resourceapi.DeviceRequestAllocationResult) (admissionDecision, error) {
	if !channelReservationsEnabled() {
		return admitDecision(), nil
	}
	if err := decodeChannelConfigs(claim); err != nil {
		return rejectDecision("InvalidConfig", "%v", err), nil
	}
	domainID := channelDomainID(claim, result.Request)
	if domainID == "" {
		return rejectDecision("NoComputeDomain", "no ComputeDomainChannelConfig applies to request %q", result.Request), nil
	}
	cd, err := m.getComputeDomain(domainID)
	if err != nil {
		return admissionDecision{}, fmt.Errorf("error getting ComputeDomain: %w", err)
	}
	if cd == nil {
		return rejectDecision("ComputeDomainNotFound", "ComputeDomain %s not found", domainID), nil
	}
	if cd.Namespace != claim.Namespace {
		return rejectDecision("NamespaceMismatch", "ComputeDomain %s is in namespace %s, not %s", domainID, cd.Namespace, claim.Namespace), nil
	}
	if cd.GetDeletionTimestamp() != nil {
		return rejectDecision("ComputeDomainDeleting", "ComputeDomain %s/%s is being deleted", cd.Namespace, cd.Name), nil
	}

	if m.config.imexConfig.EffectiveHostManaged() {
		id, ok := channelID(result.Device)
		if !ok || !slices.Contains(cd.Status.Channels, id) {
			return rejectDecision("ChannelNotReserved", "%s is not reserved for ComputeDomain %s/%s (reserved: %v)", result.Device, cd.Namespace, cd.Name, cd.Status.Channels), nil
		}
		return admitDecision(), nil
	}

	node := result.Pool
	unlock := m.lockNode(node)
	defer unlock()
	owner, err := m.claimNode(ctx, node, domainID)
	if err != nil {
		return admissionDecision{}, err
	}
	if owner != domainID {
		return rejectDecision("NodeOwnedByOtherComputeDomain", "node %s is in use by ComputeDomain %s", node, owner), nil
	}
	return admitDecision(), nil
}

func setChannelAdmissionCondition(claim *resourceapi.ResourceClaim, result *resourceapi.DeviceRequestAllocationResult, decision admissionDecision) {
	status := deviceStatus(claim, result)
	if status == nil {
		claim.Status.Devices = append(claim.Status.Devices, resourceapi.AllocatedDeviceStatus{
			Driver:  result.Driver,
			Pool:    result.Pool,
			Device:  result.Device,
			ShareID: (*string)(result.ShareID),
		})
		status = &claim.Status.Devices[len(claim.Status.Devices)-1]
	}
	conditionType := imex.ChannelRejectedCondition
	if decision.admitted {
		conditionType = imex.ChannelAdmittedCondition
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             metav1.ConditionTrue,
		Reason:             decision.reason,
		Message:            decision.message,
		ObservedGeneration: claim.Generation,
	})
}

func nodeOwnershipRuleName(node string) string {
	name := "computedomain-node-" + node
	if len(name) <= maxObjectNameLength {
		return name
	}
	sum := sha256.Sum256([]byte(node))
	return "computedomain-node-" + hex.EncodeToString(sum[:])
}

// claimNode returns the owner of a node, making domainID the owner if the node
// is free. The rule name is derived from the node, so concurrent creates
// conflict and only one ComputeDomain wins.
func (m *ClaimAdmissionManager) claimNode(ctx context.Context, node, domainID string) (string, error) {
	client := m.config.deviceTaintRules
	rule := &resourceapi.DeviceTaintRule{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeOwnershipRuleName(node),
			Labels: map[string]string{
				computeDomainLabelKey:       domainID,
				deviceTaintRuleKindLabelKey: deviceTaintRuleKindNode,
			},
		},
		Spec: resourceapi.DeviceTaintRuleSpec{
			// All devices of the node, tolerated by the owner's channel
			// and daemon claims.
			DeviceSelector: &resourceapi.DeviceTaintSelector{
				Driver: ptr.To(DriverName),
				Pool:   ptr.To(node),
			},
			Taint: resourceapi.DeviceTaint{
				Key:    imex.ChannelOwnerTaintKey,
				Value:  domainID,
				Effect: resourceapi.DeviceTaintEffectNoSchedule,
			},
		},
	}
	_, err := client.Create(ctx, rule)
	if err == nil {
		klog.Infof("Node %s now in use by ComputeDomain %s", node, domainID)
		return domainID, nil
	}
	if !errors.IsAlreadyExists(err) {
		return "", fmt.Errorf("error creating node ownership DeviceTaintRule for node %s: %w", node, err)
	}
	existing, err := client.Get(ctx, rule.Name)
	if err != nil {
		return "", fmt.Errorf("error getting node ownership DeviceTaintRule for node %s: %w", node, err)
	}
	if existing.GetDeletionTimestamp() != nil {
		return "", fmt.Errorf("node ownership DeviceTaintRule for node %s is being deleted", node)
	}
	return existing.Spec.Taint.Value, nil
}

// releaseNode deletes a node's ownership rule once its owner has no claim left
// on the node. The node lock keeps admissions out in between, and a claim not
// yet in the informer re-claims the node when it is admitted.
func (m *ClaimAdmissionManager) releaseNode(ctx context.Context, obj any) error {
	node, ok := obj.(string)
	if !ok {
		return fmt.Errorf("failed to cast to node name")
	}
	unlock := m.lockNode(node)
	defer unlock()

	client := m.config.deviceTaintRules
	rule, err := client.Get(ctx, nodeOwnershipRuleName(node))
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("error getting node ownership DeviceTaintRule for node %s: %w", node, err)
	}
	owner := rule.Spec.Taint.Value
	if m.nodeInUseByComputeDomain(node, owner) {
		return nil
	}

	err = client.Delete(ctx, rule.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &rule.UID, ResourceVersion: &rule.ResourceVersion},
	})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("error deleting node ownership DeviceTaintRule for node %s: %w", node, err)
	}
	klog.Infof("Node %s no longer in use by ComputeDomain %s", node, owner)
	return nil
}

func (m *ClaimAdmissionManager) nodeInUseByComputeDomain(node, domainID string) bool {
	for _, obj := range m.informer.GetStore().List() {
		claim, ok := obj.(*resourceapi.ResourceClaim)
		if !ok {
			continue
		}
		for _, result := range channelResults(claim) {
			if result.Pool == node && channelDomainID(claim, result.Request) == domainID {
				return true
			}
		}
	}
	return false
}
