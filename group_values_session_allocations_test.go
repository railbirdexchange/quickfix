//go:build !race

package quickfix

import (
	"runtime"
	"testing"
)

func TestGroupValuesCompletedSessionExactAllocations(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	fixture := newGroupValuesFullSession(t)
	cycle := fixture.scenario("groups")
	if err := cycle(); err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	var cycleError error
	completed := 0
	runtime.ReadMemStats(&before)
	for range 100 {
		if cycleError = cycle(); cycleError != nil {
			break
		}
		completed++
	}
	runtime.ReadMemStats(&after)
	allocations, allocatedBytes := after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc
	t.Logf("completed_batches=%d orders_per_batch=32 total_mallocs_delta=%d total_alloc_bytes_delta=%d", completed, allocations, allocatedBytes)
	if cycleError != nil || completed != 100 || allocations != 0 || allocatedBytes != 0 {
		t.Fatalf("completed=%d error=%v allocations=%d bytes=%d", completed, cycleError, allocations, allocatedBytes)
	}
}
