package main

import (
	"fmt"
	"sync"
)

const maxIndices = 16

type GPUIndexer interface {
	Reserve(index int, pciBusID PCIBusID) int
	Unreserve(index int, pciBusID PCIBusID) error
}

type gpuIndexer struct {
	sync.Mutex
	indices  map[int]PCIBusID
	capacity int
}

func NewGPUIndexer(capacity int) *gpuIndexer {
	return &gpuIndexer{
		indices:  make(map[int]PCIBusID),
		capacity: capacity,
	}
}

// Reserve a new index for a given PCI bus ID.
// If index is -1, a new index will be chosen automatically and reserved.
// If index is reserved for the given PCI bus ID, return the index.
// If index is reserved for a different PCI bus ID, a new index will
// be chosen and reserved for the given PCI bus ID.
func (i *gpuIndexer) Reserve(index int, pciBusID PCIBusID) int {
	i.Lock()
	defer i.Unlock()
	if index == -1 {
		index = i.getNextAvailableIndex()
	}
	if existing, ok := i.indices[index]; ok {
		if existing == pciBusID {
			return index
		}
		index = i.getNextAvailableIndex()
		if index == -1 {
			return -1
		}
	}
	i.indices[index] = pciBusID
	return index
}

// Unreserve an index for a given PCI bus ID.
// If index is reserved for the given PCI bus ID, then its unreserved.
// If index is reserved for a different PCI bus ID, then we fail the
// task.
func (i *gpuIndexer) Unreserve(index int, pciBusID PCIBusID) error {
	i.Lock()
	defer i.Unlock()
	if existing, ok := i.indices[index]; ok {
		if existing == pciBusID {
			delete(i.indices, index)
			return nil
		}
		return fmt.Errorf("index %d is reserved by PCI bus ID %s", index, existing)
	}
	return nil
}

// getNextAvailableIndex returns the next available index in the
// range [0, capacity).
// If no index is available, return -1.
func (i *gpuIndexer) getNextAvailableIndex() int {
	for idx := 0; idx < i.capacity; idx++ {
		if _, ok := i.indices[idx]; !ok {
			return idx
		}
	}
	return -1
}
