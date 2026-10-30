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
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// channelKey identifies the claims that share a channel: those of one
// workload ResourceClaimTemplate of a ComputeDomain in one IMEX domain.
type channelKey struct {
	imexDomain    string
	computeDomain string
	template      string
}

// imexDomainChannels holds the channels assigned in one IMEX domain.
type imexDomainChannels struct {
	channels map[channelKey]int
	owners   map[int]channelKey
	// next is where the search for a free channel starts, so that a freed
	// channel is reused last.
	next int
}

// channelAssigner assigns IMEX channels to claims at admission. A channel
// only needs to be unique in its IMEX domain, the NVLink clique of a node, so
// each clique has its own channels. Claims of one template of a ComputeDomain
// share a channel in a clique, which stays assigned while any of them holds
// it. Assignments live in memory and in the status of admitted claims, from
// which they are rebuilt after a restart.
type channelAssigner struct {
	maxChannels int

	mu      sync.Mutex
	domains map[string]*imexDomainChannels
	claims  map[channelKey]map[types.UID]bool
	byClaim map[types.UID][]channelKey
}

func newChannelAssigner(maxChannels int) *channelAssigner {
	return &channelAssigner{
		maxChannels: maxChannels,
		domains:     make(map[string]*imexDomainChannels),
		claims:      make(map[channelKey]map[types.UID]bool),
		byClaim:     make(map[types.UID][]channelKey),
	}
}

// Assign returns the channel of a claim's key, assigning a free one first.
// The legacy shared channel is never assigned this way.
func (a *channelAssigner) Assign(key channelKey, claim types.UID) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := a.domain(key.imexDomain)
	if channel, ok := d.channels[key]; ok {
		a.hold(key, claim)
		return channel, nil
	}
	for i := range a.maxChannels - 1 {
		channel := 1 + (d.next-1+i)%(a.maxChannels-1)
		if _, taken := d.owners[channel]; taken {
			continue
		}
		d.channels[key] = channel
		d.owners[channel] = key
		d.next = channel + 1
		a.hold(key, claim)
		return channel, nil
	}
	return 0, fmt.Errorf("all %d IMEX channels of %s are in use", a.maxChannels-1, key.imexDomain)
}

// Restore records the channel of an admitted claim found in its status.
func (a *channelAssigner) Restore(key channelKey, channel int, claim types.UID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := a.domain(key.imexDomain)
	if current, ok := d.channels[key]; ok && current != channel {
		klog.Warningf("ResourceClaim %s has channel %d of %+v, which has channel %d", claim, channel, key, current)
	}
	if owner, ok := d.owners[channel]; ok && owner != key {
		klog.Warningf("ResourceClaim %s has channel %d of %+v, which is also assigned to %+v", claim, channel, key, owner)
	}
	if _, ok := d.channels[key]; !ok {
		d.channels[key] = channel
	}
	if _, ok := d.owners[channel]; !ok {
		d.owners[channel] = key
	}
	d.next = max(d.next, channel+1)
	a.hold(key, claim)
}

// Forget releases what a claim holds. A channel is free once no claim holds
// it.
func (a *channelAssigner) Forget(claim types.UID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, key := range a.byClaim[claim] {
		delete(a.claims[key], claim)
		if len(a.claims[key]) > 0 {
			continue
		}
		delete(a.claims, key)
		d := a.domains[key.imexDomain]
		if channel, ok := d.channels[key]; ok {
			delete(d.channels, key)
			if d.owners[channel] == key {
				delete(d.owners, channel)
			}
		}
		if len(d.channels) == 0 {
			delete(a.domains, key.imexDomain)
		}
	}
	delete(a.byClaim, claim)
}

func (a *channelAssigner) domain(name string) *imexDomainChannels {
	d, ok := a.domains[name]
	if !ok {
		d = &imexDomainChannels{channels: make(map[channelKey]int), owners: make(map[int]channelKey), next: 1}
		a.domains[name] = d
	}
	return d
}

func (a *channelAssigner) hold(key channelKey, claim types.UID) {
	if a.claims[key] == nil {
		a.claims[key] = make(map[types.UID]bool)
	}
	if !a.claims[key][claim] {
		a.claims[key][claim] = true
		a.byClaim[claim] = append(a.byClaim[claim], key)
	}
}
