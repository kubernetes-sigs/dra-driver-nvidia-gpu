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
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
)

// channelDeviceState is the node state that the published channel device
// reflects.
type channelDeviceState struct {
	// owner is the ComputeDomain whose IMEX daemon the node runs, from the
	// node label, under per-ComputeDomain IMEX daemons.
	owner string
	// cliqueID is the NVLink clique of the node, in which channels are
	// unique.
	cliqueID string
}

type publishFunc func(ctx context.Context, resources resourceslice.DriverResources) error

// devicePublisher publishes the devices of the node, and publishes them again
// when the state the channel device reflects changes.
type devicePublisher struct {
	config      *Config
	allocatable AllocatableDevices
	publish     publishFunc

	mu      sync.Mutex
	state   channelDeviceState
	started bool

	cancel  context.CancelFunc
	factory informers.SharedInformerFactory
}

func newDevicePublisher(config *Config, allocatable AllocatableDevices, publish publishFunc) *devicePublisher {
	return &devicePublisher{
		config:      config,
		allocatable: allocatable,
		publish:     publish,
	}
}

// Update changes the state the channel device reflects, and publishes the
// devices if it changed once publishing started.
func (p *devicePublisher) Update(ctx context.Context, update func(*channelDeviceState)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.state
	update(&state)
	if state == p.state {
		return nil
	}
	if p.started {
		if err := p.publishLocked(ctx, state); err != nil {
			return err
		}
	}
	p.state = state
	return nil
}

// Start publishes the devices for the first time.
func (p *devicePublisher) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.publishLocked(ctx, p.state); err != nil {
		return err
	}
	p.started = true
	return nil
}

func (p *devicePublisher) publishLocked(ctx context.Context, state channelDeviceState) error {
	resources := resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			p.config.flags.nodeName: {Slices: computeDomainPublishedSlices(p.allocatable, p.config, state)},
		},
	}
	if err := p.publish(ctx, resources); err != nil {
		return fmt.Errorf("error in PublishResources(): %w", err)
	}
	klog.V(2).Infof("Published devices, channel device owner %q, clique %q", state.owner, state.cliqueID)
	return nil
}

// WatchOwner follows the node label naming the ComputeDomain whose IMEX
// daemon the node runs. The controller sets it when it admits the first
// claim of a ComputeDomain on the node, and this plugin removes it after the
// last one is unprepared. It returns once the label is known.
func (p *devicePublisher) WatchOwner(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	p.cancel = cancel

	factory := informers.NewSharedInformerFactoryWithOptions(
		p.config.clientsets.Core,
		informerResyncPeriod,
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", p.config.flags.nodeName).String()
		}),
	)
	informer := factory.Core().V1().Nodes().Informer()
	onNode := func(obj any) {
		node, ok := obj.(*corev1.Node)
		if !ok || node.Name != p.config.flags.nodeName {
			return
		}
		owner := node.Labels[computeDomainLabelKey]
		if err := p.Update(ctx, func(s *channelDeviceState) { s.owner = owner }); err != nil {
			klog.Errorf("Error publishing the channel device owner %q: %v", owner, err)
		}
	}
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    onNode,
		UpdateFunc: func(_, obj any) { onNode(obj) },
	}); err != nil {
		return fmt.Errorf("error adding event handler for Node informer: %w", err)
	}

	p.factory = factory
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return fmt.Errorf("informer cache sync for Node %s failed", p.config.flags.nodeName)
	}
	return nil
}

func (p *devicePublisher) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	if p.factory != nil {
		p.factory.Shutdown()
	}
}

// startPublishing publishes the devices of the node, with the state the
// channel device reflects in this mode.
func (d *driver) startPublishing(ctx context.Context, config *Config) error {
	if channelReservationsEnabled() {
		// Registered first, so that no change is missed.
		d.state.computeDomainManager.OnCliqueIDChange(func(cliqueID string) {
			if err := d.publisher.Update(ctx, func(s *channelDeviceState) { s.cliqueID = cliqueID }); err != nil {
				klog.Errorf("Error publishing the channel device clique %q: %v", cliqueID, err)
			}
		})
		cliqueID := d.state.computeDomainManager.CliqueID()
		if err := d.publisher.Update(ctx, func(s *channelDeviceState) { s.cliqueID = cliqueID }); err != nil {
			return err
		}
		if !config.imexConfig.SharedIMEXDomain() {
			if err := d.publisher.WatchOwner(ctx); err != nil {
				return err
			}
		}
	}
	return d.publisher.Start(ctx)
}
