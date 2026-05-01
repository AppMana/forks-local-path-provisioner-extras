package lpp

import (
	"sync"

	"k8s.io/apimachinery/pkg/api/resource"
)

// CapacityTracker tracks allocated storage per (node, path) pair.
type CapacityTracker struct {
	mu        sync.RWMutex
	allocated map[string]int64
}

func NewCapacityTracker() *CapacityTracker {
	return &CapacityTracker{allocated: map[string]int64{}}
}

func capKey(node, basePath string) string { return node + ":" + basePath }

func (ct *CapacityTracker) Allocate(node, basePath string, sizeBytes int64) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	ct.allocated[capKey(node, basePath)] += sizeBytes
}

func (ct *CapacityTracker) Release(node, basePath string, sizeBytes int64) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	k := capKey(node, basePath)
	ct.allocated[k] -= sizeBytes
	if ct.allocated[k] <= 0 {
		delete(ct.allocated, k)
	}
}

func (ct *CapacityTracker) GetAllocated(node, basePath string) int64 {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	return ct.allocated[capKey(node, basePath)]
}

func (ct *CapacityTracker) HasCapacity(node, basePath string, sizeBytes int64, maxCapacity *resource.Quantity) bool {
	if maxCapacity == nil {
		return true
	}
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	return ct.allocated[capKey(node, basePath)]+sizeBytes <= maxCapacity.Value()
}

// TryAllocate atomically checks capacity and allocates if possible.
func (ct *CapacityTracker) TryAllocate(node, basePath string, sizeBytes int64, maxCapacity *resource.Quantity) bool {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	k := capKey(node, basePath)
	if maxCapacity != nil && ct.allocated[k]+sizeBytes > maxCapacity.Value() {
		return false
	}
	ct.allocated[k] += sizeBytes
	return true
}

// TryResize atomically adjusts allocation from oldBytes to newBytes, respecting maxCapacity.
func (ct *CapacityTracker) TryResize(node, basePath string, oldBytes, newBytes int64, maxCapacity *resource.Quantity) bool {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	k := capKey(node, basePath)
	delta := newBytes - oldBytes
	if delta > 0 && maxCapacity != nil && ct.allocated[k]+delta > maxCapacity.Value() {
		return false
	}
	ct.allocated[k] += delta
	if ct.allocated[k] <= 0 {
		delete(ct.allocated, k)
	}
	return true
}
