package lpp

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
)

func qty(v string) *resource.Quantity {
	q := resource.MustParse(v)
	return &q
}

func TestCapacityTracker_AllocateRelease(t *testing.T) {
	ct := NewCapacityTracker()
	ct.Allocate("n1", "/p", 100)
	assert.Equal(t, int64(100), ct.GetAllocated("n1", "/p"))
	ct.Allocate("n1", "/p", 50)
	assert.Equal(t, int64(150), ct.GetAllocated("n1", "/p"))
	ct.Release("n1", "/p", 100)
	assert.Equal(t, int64(50), ct.GetAllocated("n1", "/p"))
	ct.Release("n1", "/p", 50)
	// fully released — entry deleted, GetAllocated returns 0
	assert.Equal(t, int64(0), ct.GetAllocated("n1", "/p"))
}

func TestCapacityTracker_HasCapacity_NilLimit(t *testing.T) {
	ct := NewCapacityTracker()
	ct.Allocate("n1", "/p", 1<<40) // 1 TiB
	assert.True(t, ct.HasCapacity("n1", "/p", 1<<60, nil))
}

func TestCapacityTracker_HasCapacity_AtLimit(t *testing.T) {
	ct := NewCapacityTracker()
	ct.Allocate("n1", "/p", 600)
	// 1Ki = 1024
	assert.True(t, ct.HasCapacity("n1", "/p", 424, qty("1Ki")))  // 600+424 = 1024
	assert.False(t, ct.HasCapacity("n1", "/p", 425, qty("1Ki"))) // 600+425 = 1025 > 1024
}

func TestCapacityTracker_TryAllocate_RejectsOverLimit(t *testing.T) {
	ct := NewCapacityTracker()
	require.True(t, ct.TryAllocate("n1", "/p", 700, qty("1Ki")))
	assert.Equal(t, int64(700), ct.GetAllocated("n1", "/p"))
	assert.False(t, ct.TryAllocate("n1", "/p", 400, qty("1Ki"))) // would be 1100
	assert.Equal(t, int64(700), ct.GetAllocated("n1", "/p"))     // unchanged
	assert.True(t, ct.TryAllocate("n1", "/p", 324, qty("1Ki")))  // exactly 1024
}

func TestCapacityTracker_TryAllocate_NilLimit(t *testing.T) {
	ct := NewCapacityTracker()
	for i := 0; i < 100; i++ {
		require.True(t, ct.TryAllocate("n1", "/p", 1<<30, nil))
	}
	assert.Equal(t, int64(100)<<30, ct.GetAllocated("n1", "/p"))
}

func TestCapacityTracker_TryAllocate_Concurrent(t *testing.T) {
	ct := NewCapacityTracker()
	limit := qty("100Ki")

	var ok int64
	const goroutines = 200
	const each = 1024 // 1 KiB; 100 should fit
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if ct.TryAllocate("n1", "/p", each, limit) {
				atomic.AddInt64(&ok, 1)
			}
		}()
	}
	wg.Wait()
	// exactly 100 should fit, never more (no over-allocation race)
	assert.Equal(t, int64(100), atomic.LoadInt64(&ok))
	assert.Equal(t, int64(100*each), ct.GetAllocated("n1", "/p"))
}

func TestCapacityTracker_TryResize_Expand(t *testing.T) {
	ct := NewCapacityTracker()
	ct.Allocate("n1", "/p", 500)
	assert.True(t, ct.TryResize("n1", "/p", 500, 800, qty("1Ki")))
	assert.Equal(t, int64(800), ct.GetAllocated("n1", "/p"))

	// expand beyond limit fails, allocation unchanged
	assert.False(t, ct.TryResize("n1", "/p", 800, 1500, qty("1Ki")))
	assert.Equal(t, int64(800), ct.GetAllocated("n1", "/p"))
}

func TestCapacityTracker_TryResize_Shrink(t *testing.T) {
	ct := NewCapacityTracker()
	ct.Allocate("n1", "/p", 800)
	// shrink always succeeds, even when over limit (doesn't make it worse)
	assert.True(t, ct.TryResize("n1", "/p", 800, 200, qty("1Ki")))
	assert.Equal(t, int64(200), ct.GetAllocated("n1", "/p"))
	// shrink to zero deletes the entry
	assert.True(t, ct.TryResize("n1", "/p", 200, 0, nil))
	assert.Equal(t, int64(0), ct.GetAllocated("n1", "/p"))
}

func TestCapacityTracker_KeysAreNodePath(t *testing.T) {
	ct := NewCapacityTracker()
	ct.Allocate("n1", "/a", 100)
	ct.Allocate("n1", "/b", 200)
	ct.Allocate("n2", "/a", 300)
	assert.Equal(t, int64(100), ct.GetAllocated("n1", "/a"))
	assert.Equal(t, int64(200), ct.GetAllocated("n1", "/b"))
	assert.Equal(t, int64(300), ct.GetAllocated("n2", "/a"))
	assert.Equal(t, int64(0), ct.GetAllocated("n2", "/b"))
}
