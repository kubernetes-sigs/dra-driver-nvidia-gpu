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
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelAssigner(t *testing.T) {
	a := newChannelAssigner(4)
	jobA := channelKey{imexDomain: "clique-1", computeDomain: "cd-a", template: "job"}
	jobB := channelKey{imexDomain: "clique-1", computeDomain: "cd-b", template: "job"}
	jobC := channelKey{imexDomain: "clique-1", computeDomain: "cd-c", template: "job"}
	jobD := channelKey{imexDomain: "clique-1", computeDomain: "cd-d", template: "job"}
	otherClique := channelKey{imexDomain: "clique-2", computeDomain: "cd-b", template: "job"}

	assign := func(key channelKey, claim string) int {
		t.Helper()
		channel, err := a.Assign(key, types.UID(claim))
		require.NoError(t, err)
		return channel
	}

	assert.Equal(t, 1, assign(jobA, "a1"), "channel 0 is never assigned")
	assert.Equal(t, 1, assign(jobA, "a2"))
	assert.Equal(t, 2, assign(jobB, "b1"))
	assert.Equal(t, 1, assign(otherClique, "b2"), "cliques have their own channels")
	assert.Equal(t, 3, assign(jobC, "c1"))
	_, err := a.Assign(jobD, "d1")
	require.ErrorContains(t, err, "all 3 IMEX channels of clique-1 are in use")

	a.Forget("a1")
	_, err = a.Assign(jobD, "d1")
	require.Error(t, err, "a channel stays assigned while any claim holds it")
	a.Forget("a2")
	assert.Equal(t, 1, assign(jobD, "d1"), "the freed channel, after wrapping around")

	a.Forget("b1")
	a.Forget("c1")
	assert.Equal(t, 2, assign(jobA, "a3"), "freed channels are reused in turn, not lowest first")
}

func TestChannelAssignerRestore(t *testing.T) {
	a := newChannelAssigner(8)
	jobA := channelKey{imexDomain: "clique-1", computeDomain: "cd-a", template: "job"}
	jobB := channelKey{imexDomain: "clique-1", computeDomain: "cd-b", template: "job"}

	a.Restore(jobA, 5, "a1")
	a.Restore(jobA, 5, "a1")
	channel, err := a.Assign(jobA, "a2")
	require.NoError(t, err)
	assert.Equal(t, 5, channel, "restored claims share their channel with new ones")
	channel, err = a.Assign(jobB, "b1")
	require.NoError(t, err)
	assert.Equal(t, 6, channel, "assignment continues after restored channels")

	a.Forget("a1")
	a.Forget("a2")
	channel, err = a.Assign(jobB, "b2")
	require.NoError(t, err)
	assert.Equal(t, 6, channel)
	assert.Empty(t, a.claims[jobA], "forgetting every claim frees the key")
}
