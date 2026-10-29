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
	"hash/fnv"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	nvapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/imex"
)

const (
	// claimAdmissionWorkers process admissions in parallel, as each pod
	// waits for one before it is bound.
	claimAdmissionWorkers = 16
	// nodeReleaseWorkers release nodes no claim uses anymore.
	nodeReleaseWorkers = 2

	// claimNodeIndex indexes claims by the nodes of their allocated channels.
	claimNodeIndex = "node"
	// claimDomainIndex indexes claims by the domain IDs they reference.
	claimDomainIndex = "domain"
	// sliceNodeIndex indexes ResourceSlices by their node.
	sliceNodeIndex = "node"

	nodeLockStripes = 256

	legacyChannelDevicePrefix = "channel-"
)

// ClaimAdmissionManager admits or rejects allocated channels through their
// binding conditions, before the scheduler binds the pod. Deciding in one
// place avoids races between the first claims of different ComputeDomains.
//
// Only per-ComputeDomain IMEX daemons need it: a node runs the IMEX daemon of
// one ComputeDomain, so claims are only admitted on nodes no other
// ComputeDomain uses. A ComputeDomain uses a node while it has admitted
// channels there, or while its IMEX daemon runs there, which the kubelet
// plugin's node label shows until the last of its claims is unprepared.
//
// Ownership is derived rather than stored. Decisions are serialized per node,
// and admissions the cache does not show yet are tracked here, so two
// ComputeDomains never both get a free node. After a restart the cache holds
// every admission, as each was written before the pod could bind.
//
// It runs regardless of the ComputeDomainChannelReservations gate: with the
// gate off it admits all channels, so that kubelet plugins enabling the gate
// first cannot hold up pods.
type ClaimAdmissionManager struct {
	config           *ManagerConfig
	getComputeDomain GetComputeDomainFunc
	waitGroup        sync.WaitGroup
	cancelContext    context.CancelFunc

	informer cache.SharedIndexInformer

	// nodeInformer watches the ComputeDomain node labels, for per-ComputeDomain
	// IMEX daemons with the gate on.
	nodeInformer cache.SharedIndexInformer

	// sliceInformer watches the ResourceSlices of this driver for the NVLink
	// clique of each node, with the gate on.
	sliceInformer cache.SharedIndexInformer

	// assigner assigns channels at admission, with the gate on.
	assigner *channelAssigner

	// queue holds claim keys. It is separate from the controller's queue and
	// has no overall rate limit, so that admissions do not wait behind
	// ComputeDomain reconciles.
	queue workqueue.TypedRateLimitingInterface[string]

	// releaseQueue holds names of nodes that may have lost their last claim
	// of the ComputeDomain owning them.
	releaseQueue workqueue.TypedRateLimitingInterface[string]
	releaseDelay time.Duration

	// nodeLocks serializes decisions per node.
	nodeLocks [nodeLockStripes]sync.Mutex

	// inflight holds admissions per node and claim that the cache may not
	// show yet.
	inflightMu sync.Mutex
	inflight   map[string]map[types.UID]string
}

// claimAdmissionSupported reports whether claims can be admitted: the
// informer and binding conditions need resource.k8s.io/v1 ResourceClaims,
// served from Kubernetes 1.34. Older clusters run without admission, unless
// channel reservations need it.
func claimAdmissionSupported(clientset coreclientset.Interface) (bool, error) {
	served, err := servesResource(clientset, resourceapi.SchemeGroupVersion.String(), "resourceclaims")
	if err != nil {
		return false, err
	}
	if !served && channelReservationsEnabled() {
		return false, fmt.Errorf("ComputeDomain channel reservations require %s ResourceClaims, served by Kubernetes 1.34 or later", resourceapi.SchemeGroupVersion)
	}
	return served, nil
}

// servesResource reports whether the API server serves a resource in a group
// version. A group version it does not serve is not an error.
func servesResource(clientset coreclientset.Interface, groupVersion, resource string) (bool, error) {
	resources, err := clientset.Discovery().ServerResourcesForGroupVersion(groupVersion)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("error discovering %s: %w", groupVersion, err)
	}
	return slices.ContainsFunc(resources.APIResources, func(r metav1.APIResource) bool {
		return r.Name == resource
	}), nil
}

func NewClaimAdmissionManager(config *ManagerConfig, getComputeDomain GetComputeDomainFunc) *ClaimAdmissionManager {
	informer := cache.NewSharedIndexInformer(
		cache.ToListWatcherWithWatchListSemantics(&cache.ListWatch{
			ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
				return config.clientsets.Core.ResourceV1().ResourceClaims("").List(ctx, options)
			},
			WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
				return config.clientsets.Core.ResourceV1().ResourceClaims("").Watch(ctx, options)
			},
		}, config.clientsets.Core),
		&resourceapi.ResourceClaim{},
		informerResyncPeriod,
		cache.Indexers{
			claimNodeIndex:   indexClaimByNode,
			claimDomainIndex: indexClaimByDomain,
		},
	)

	nodeInformer := cache.NewSharedIndexInformer(
		cache.ToListWatcherWithWatchListSemantics(&cache.ListWatch{
			ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
				options.LabelSelector = computeDomainLabelKey
				return config.clientsets.Core.CoreV1().Nodes().List(ctx, options)
			},
			WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
				options.LabelSelector = computeDomainLabelKey
				return config.clientsets.Core.CoreV1().Nodes().Watch(ctx, options)
			},
		}, config.clientsets.Core),
		&corev1.Node{},
		informerResyncPeriod,
		cache.Indexers{},
	)

	var sliceInformer cache.SharedIndexInformer
	if channelReservationsEnabled() {
		sliceInformer = cache.NewSharedIndexInformer(
			cache.ToListWatcherWithWatchListSemantics(&cache.ListWatch{
				ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
					options.FieldSelector = resourceapi.ResourceSliceSelectorDriver + "=" + DriverName
					return config.clientsets.Core.ResourceV1().ResourceSlices().List(ctx, options)
				},
				WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
					options.FieldSelector = resourceapi.ResourceSliceSelectorDriver + "=" + DriverName
					return config.clientsets.Core.ResourceV1().ResourceSlices().Watch(ctx, options)
				},
			}, config.clientsets.Core),
			&resourceapi.ResourceSlice{},
			informerResyncPeriod,
			cache.Indexers{sliceNodeIndex: indexSliceByNode},
		)
	}

	return &ClaimAdmissionManager{
		config:           config,
		getComputeDomain: getComputeDomain,
		informer:         informer,
		nodeInformer:     nodeInformer,
		sliceInformer:    sliceInformer,
		assigner:         newChannelAssigner(config.imexMaxChannels),
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemExponentialFailureRateLimiter[string](5*time.Millisecond, 30*time.Second),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "compute-domain-claim-admission"},
		),
		releaseQueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Second, time.Minute),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "compute-domain-node-release"},
		),
		releaseDelay: nodeReleaseDelay,
		inflight:     make(map[string]map[types.UID]string),
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

	// The handlers restore the channels of admitted claims before workers
	// start, so that no channel is assigned twice after a restart.
	registration, err := m.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			m.restoreChannels(obj)
			m.enqueueAdmission(obj)
		},
		UpdateFunc: func(oldObj, newObj any) {
			m.restoreChannels(newObj)
			m.forgetChannels(newObj)
			m.enqueueAdmission(newObj)
			m.forgetDecided(newObj)
			m.enqueueReleases(oldObj, newObj)
		},
		DeleteFunc: func(obj any) {
			if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = tombstone.Obj
			}
			if claim, ok := obj.(*resourceapi.ResourceClaim); ok {
				m.assigner.Forget(claim.UID)
				m.forgetInflight(claim.UID)
				m.enqueueReleases(claim, nil)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("error adding event handlers for ResourceClaim informer: %w", err)
	}

	m.waitGroup.Go(func() {
		m.informer.Run(ctx.Done())
	})
	synced := []cache.InformerSynced{registration.HasSynced}

	if m.sliceInformer != nil {
		m.waitGroup.Go(func() {
			m.sliceInformer.Run(ctx.Done())
		})
		synced = append(synced, m.sliceInformer.HasSynced)
	}

	if m.ownsNodes() {
		// Owned nodes are checked once, which releases those left behind
		// before a restart.
		if _, err := m.nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj any) {
				if node, ok := obj.(*corev1.Node); ok {
					m.releaseQueue.AddAfter(node.Name, m.releaseDelay)
				}
			},
		}); err != nil {
			return fmt.Errorf("error adding event handlers for Node informer: %w", err)
		}
		m.waitGroup.Go(func() {
			m.nodeInformer.Run(ctx.Done())
		})
		synced = append(synced, m.nodeInformer.HasSynced)
	}

	if !cache.WaitForCacheSync(ctx.Done(), synced...) {
		return fmt.Errorf("informer cache sync for ResourceClaims and Nodes failed")
	}

	for range claimAdmissionWorkers {
		m.waitGroup.Go(func() {
			for m.processNextItem(ctx) {
			}
		})
	}
	if m.ownsNodes() {
		for range nodeReleaseWorkers {
			m.waitGroup.Go(func() {
				for m.processNextRelease(ctx) {
				}
			})
		}
	}

	return nil
}

func (m *ClaimAdmissionManager) Stop() error {
	m.queue.ShutDown()
	m.releaseQueue.ShutDown()
	if m.cancelContext != nil {
		m.cancelContext()
	}
	m.waitGroup.Wait()
	return nil
}

func (m *ClaimAdmissionManager) processNextItem(ctx context.Context) bool {
	key, shutdown := m.queue.Get()
	if shutdown {
		return false
	}
	defer m.queue.Done(key)

	if err := m.admitKey(ctx, key); err != nil {
		klog.V(1).Infof("ClaimAdmission %s: %v (attempt %d)", key, err, m.queue.NumRequeues(key))
		m.queue.AddRateLimited(key)
		return true
	}
	m.queue.Forget(key)
	return true
}

// ownsNodes reports whether nodes are owned by ComputeDomains.
func (m *ClaimAdmissionManager) ownsNodes() bool {
	return channelReservationsEnabled() && !m.config.imexConfig.SharedIMEXDomain()
}

func indexClaimByNode(obj any) ([]string, error) {
	claim, ok := obj.(*resourceapi.ResourceClaim)
	if !ok {
		return nil, nil
	}
	var nodes []string
	for _, result := range channelResults(claim) {
		if !slices.Contains(nodes, result.Pool) {
			nodes = append(nodes, result.Pool)
		}
	}
	return nodes, nil
}

func indexSliceByNode(obj any) ([]string, error) {
	slice, ok := obj.(*resourceapi.ResourceSlice)
	if !ok || slice.Spec.NodeName == nil {
		return nil, nil
	}
	return []string{*slice.Spec.NodeName}, nil
}

func indexClaimByDomain(obj any) ([]string, error) {
	claim, ok := obj.(*resourceapi.ResourceClaim)
	if !ok {
		return nil, nil
	}
	return claimDomainIDs(claim), nil
}

// claimsForComputeDomain returns the cached ResourceClaims referencing a
// ComputeDomain.
func (m *ClaimAdmissionManager) claimsForComputeDomain(uid string) []*resourceapi.ResourceClaim {
	return m.claimsByIndex(claimDomainIndex, uid)
}

func (m *ClaimAdmissionManager) claimsByIndex(index, key string) []*resourceapi.ResourceClaim {
	objs, err := m.informer.GetIndexer().ByIndex(index, key)
	if err != nil {
		klog.Errorf("Error looking up ResourceClaims by %s %s: %v", index, key, err)
		return nil
	}
	var claims []*resourceapi.ResourceClaim
	for _, obj := range objs {
		if claim, ok := obj.(*resourceapi.ResourceClaim); ok {
			claims = append(claims, claim)
		}
	}
	return claims
}

func (m *ClaimAdmissionManager) enqueueAdmission(obj any) {
	claim, ok := obj.(*resourceapi.ResourceClaim)
	if !ok || len(pendingChannelResults(claim)) == 0 {
		return
	}
	key, err := cache.MetaNamespaceKeyFunc(claim)
	if err != nil {
		klog.Errorf("Error getting key of ResourceClaim: %v", err)
		return
	}
	m.queue.Add(key)
}

// forgetDecided drops the in-process admissions of a claim once the cache
// shows them, or the claim lost its allocation.
func (m *ClaimAdmissionManager) forgetDecided(obj any) {
	claim, ok := obj.(*resourceapi.ResourceClaim)
	if ok && len(pendingChannelResults(claim)) == 0 {
		m.forgetInflight(claim.UID)
	}
}

func (m *ClaimAdmissionManager) addInflight(node string, claimUID types.UID, domainID string) {
	m.inflightMu.Lock()
	defer m.inflightMu.Unlock()
	if m.inflight[node] == nil {
		m.inflight[node] = make(map[types.UID]string)
	}
	m.inflight[node][claimUID] = domainID
}

func (m *ClaimAdmissionManager) forgetInflight(claimUID types.UID) {
	m.inflightMu.Lock()
	defer m.inflightMu.Unlock()
	for node, claims := range m.inflight {
		delete(claims, claimUID)
		if len(claims) == 0 {
			delete(m.inflight, node)
		}
	}
}

func (m *ClaimAdmissionManager) lockNode(node string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(node))
	mu := &m.nodeLocks[h.Sum32()%nodeLockStripes]
	mu.Lock()
	return mu.Unlock
}

// isChannelDevice reports whether a device of this driver is an IMEX
// channel: the shared channel device, or a per-channel device published
// without channel reservations.
func isChannelDevice(device string) bool {
	return device == imex.ChannelDeviceName || strings.HasPrefix(device, legacyChannelDevicePrefix)
}

// channelResults returns the claim's allocated channels. Each kubelet plugin
// publishes one pool, named after its node.
func channelResults(claim *resourceapi.ResourceClaim) []resourceapi.DeviceRequestAllocationResult {
	if claim.Status.Allocation == nil {
		return nil
	}
	var results []resourceapi.DeviceRequestAllocationResult
	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver == DriverName && isChannelDevice(result.Device) {
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

// channelInUse reports whether an allocated channel counts as in use: it was
// admitted, or allocated without waiting for admission.
func channelInUse(claim *resourceapi.ResourceClaim, result *resourceapi.DeviceRequestAllocationResult) bool {
	if !slices.Contains(result.BindingConditions, imex.ChannelAdmittedCondition) {
		return true
	}
	status := deviceStatus(claim, result)
	return status != nil && meta.IsStatusConditionTrue(status.Conditions, imex.ChannelAdmittedCondition)
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

// channelConfig returns the channel config applying to a request. Like in
// kubelet plugins, claim configs take precedence over class configs, and
// later configs over earlier ones.
func channelConfig(claim *resourceapi.ResourceClaim, request string) *nvapi.ComputeDomainChannelConfig {
	if claim.Status.Allocation == nil {
		return nil
	}
	var config *nvapi.ComputeDomainChannelConfig
	for _, source := range []resourceapi.AllocationConfigSource{resourceapi.AllocationConfigSourceClass, resourceapi.AllocationConfigSourceClaim} {
		for _, c := range claim.Status.Allocation.Devices.Config {
			if c.Source != source || (len(c.Requests) > 0 && !slices.Contains(c.Requests, request)) {
				continue
			}
			if decoded := decodeChannelConfig(c.Opaque); decoded != nil {
				config = decoded
			}
		}
	}
	return config
}

// channelDomainID returns the domain ID of the channel config applying to a
// request.
func channelDomainID(claim *resourceapi.ResourceClaim, request string) string {
	if config := channelConfig(claim, request); config != nil {
		return config.DomainID
	}
	return ""
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

// admissionDecision is the outcome for one allocated channel.
type admissionDecision struct {
	admitted bool
	reason   string
	message  string
	// data is the channel allocation of the device status.
	data *runtime.RawExtension
}

func admitDecision() admissionDecision {
	return admissionDecision{admitted: true, reason: "Admitted", message: "channel admitted for the ComputeDomain"}
}

func admitChannelDecision(allocation imex.ChannelAllocation) admissionDecision {
	data, err := allocation.Encode()
	if err != nil {
		return rejectDecision("InvalidChannel", "%v", err)
	}
	d := admitDecision()
	d.message = fmt.Sprintf("channel %d admitted for the ComputeDomain in %s", allocation.Channel, allocation.IMEXDomain)
	d.data = data
	return d
}

func rejectDecision(reason, format string, args ...any) admissionDecision {
	return admissionDecision{reason: reason, message: fmt.Sprintf(format, args...)}
}

func (m *ClaimAdmissionManager) admitKey(ctx context.Context, key string) error {
	latest, exists, err := m.informer.GetStore().GetByKey(key)
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
			m.forgetInflight(claim.UID)
			return err
		}
		klog.V(2).Infof("Channel %s/%s for ResourceClaim %s/%s: %s (%s)", pending[i].Pool, pending[i].Device, claim.Namespace, claim.Name, decision.reason, decision.message)
		setChannelAdmissionCondition(claim, &pending[i], decision)
	}

	if _, err := m.config.clientsets.Core.ResourceV1().ResourceClaims(claim.Namespace).UpdateStatus(ctx, claim, metav1.UpdateOptions{}); err != nil {
		m.forgetInflight(claim.UID)
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

	node := result.Pool
	if !m.config.imexConfig.SharedIMEXDomain() {
		unlock := m.lockNode(node)
		defer unlock()
		if other := m.otherNodeUser(node, domainID, claim.UID); other != "" {
			return rejectDecision("NodeUsedByOtherComputeDomain", "node %s is in use by ComputeDomain %s", node, other), nil
		}
		other, err := m.claimNode(ctx, node, domainID)
		if err != nil {
			return admissionDecision{}, err
		}
		if other != "" {
			return rejectDecision("NodeUsedByOtherComputeDomain", "node %s is in use by ComputeDomain %s", node, other), nil
		}
		m.addInflight(node, claim.UID, domainID)
	}

	// Channel devices of kubelet plugins without the gate, and channel 0
	// kept for templates from before the gate, carry their channel in
	// their name.
	if result.Device != imex.ChannelDeviceName {
		return admitDecision(), nil
	}
	key, legacy := m.channelKey(claim, result)
	if legacy {
		return admitChannelDecision(imex.ChannelAllocation{Channel: legacySharedChannel, IMEXDomain: key.imexDomain}), nil
	}
	channel, err := m.assigner.Assign(key, claim.UID)
	if err != nil {
		return rejectDecision("NoFreeChannel", "%v", err), nil
	}
	return admitChannelDecision(imex.ChannelAllocation{Channel: channel, IMEXDomain: key.imexDomain}), nil
}

// channelKey returns which claims share the channel of an allocated channel
// device. Claims that do not name their template, which the controller never
// creates, get the legacy shared channel that is never assigned otherwise.
func (m *ClaimAdmissionManager) channelKey(claim *resourceapi.ResourceClaim, result *resourceapi.DeviceRequestAllocationResult) (channelKey, bool) {
	config := channelConfig(claim, result.Request)
	key := channelKey{imexDomain: m.imexDomain(result.Pool)}
	if config == nil || config.ResourceClaimTemplateName == "" {
		return key, true
	}
	key.computeDomain = config.DomainID
	key.template = config.ResourceClaimTemplateName
	return key, false
}

// imexDomain returns the IMEX domain of a node, in which channels are unique:
// the NVLink clique its kubelet plugin publishes, or the node itself.
func (m *ClaimAdmissionManager) imexDomain(node string) string {
	if m.sliceInformer == nil {
		return "node/" + node
	}
	objs, err := m.sliceInformer.GetIndexer().ByIndex(sliceNodeIndex, node)
	if err != nil {
		klog.Errorf("Error looking up ResourceSlices of node %s: %v", node, err)
	}
	for _, obj := range objs {
		slice, ok := obj.(*resourceapi.ResourceSlice)
		if !ok {
			continue
		}
		for _, device := range slice.Spec.Devices {
			if device.Name != imex.ChannelDeviceName {
				continue
			}
			if clique := device.Attributes[imex.ChannelCliqueAttribute].StringValue; clique != nil && *clique != "" {
				return *clique
			}
		}
	}
	return "node/" + node
}

// restoreChannels records the channels of a claim's admitted channel devices,
// which rebuilds the assignments after a restart.
func (m *ClaimAdmissionManager) restoreChannels(obj any) {
	claim, ok := obj.(*resourceapi.ResourceClaim)
	if !ok {
		return
	}
	for _, result := range channelResults(claim) {
		if result.Device != imex.ChannelDeviceName {
			continue
		}
		status := deviceStatus(claim, &result)
		if status == nil || !meta.IsStatusConditionTrue(status.Conditions, imex.ChannelAdmittedCondition) {
			continue
		}
		allocation, err := imex.DecodeChannelAllocation(status.Data)
		if err != nil || allocation == nil {
			klog.Warningf("ResourceClaim %s/%s has no valid channel for %s: %v", claim.Namespace, claim.Name, result.Device, err)
			continue
		}
		config := channelConfig(claim, result.Request)
		if config == nil || config.ResourceClaimTemplateName == "" {
			continue
		}
		key := channelKey{imexDomain: allocation.IMEXDomain, computeDomain: config.DomainID, template: config.ResourceClaimTemplateName}
		m.assigner.Restore(key, allocation.Channel, claim.UID)
	}
}

// forgetChannels releases the channels of a claim that lost its allocation.
func (m *ClaimAdmissionManager) forgetChannels(obj any) {
	if claim, ok := obj.(*resourceapi.ResourceClaim); ok && len(channelResults(claim)) == 0 {
		m.assigner.Forget(claim.UID)
	}
}

// otherNodeUser returns a ComputeDomain other than domainID using the node:
// one with in-use channels there, an admission the cache does not show yet,
// or its IMEX daemon still on the node.
func (m *ClaimAdmissionManager) otherNodeUser(node, domainID string, self types.UID) string {
	m.inflightMu.Lock()
	for uid, other := range m.inflight[node] {
		if uid != self && other != domainID {
			m.inflightMu.Unlock()
			return other
		}
	}
	m.inflightMu.Unlock()

	for _, claim := range m.claimsByIndex(claimNodeIndex, node) {
		if claim.UID == self {
			continue
		}
		for _, result := range channelResults(claim) {
			if result.Pool != node || !channelInUse(claim, &result) {
				continue
			}
			if other := channelDomainID(claim, result.Request); other != "" && other != domainID {
				return other
			}
		}
	}

	obj, exists, err := m.nodeInformer.GetStore().GetByKey(node)
	if err != nil || !exists {
		return ""
	}
	if n, ok := obj.(*corev1.Node); ok {
		if label := n.Labels[computeDomainLabelKey]; label != "" && label != domainID {
			return label
		}
	}
	return ""
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
	if decision.data != nil {
		status.Data = decision.data
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             metav1.ConditionTrue,
		Reason:             decision.reason,
		Message:            decision.message,
		ObservedGeneration: claim.Generation,
	})
}
