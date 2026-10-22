//go:build !race

package quickfix

import (
	"runtime"
	"sync/atomic"
	"testing"
)

// Check whole-interval totals as well as benchmark averages: even one allocation
// in the warmed completed-write interval must fail this regression test.
func TestHeartbeatObservedStringReadsExactAllocations(test *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	fixture := newHeartbeatStoreFixture(test, "borrowed_external")
	defer fixture.close()
	var events, invalidEvents atomic.Uint64
	SetSendStageObserver(func(event SendStageEvent) {
		events.Add(1)
		if event.Stage == "" || event.Duration < 0 || !event.Success || (event.MsgType != "" && event.MsgType != "0") {
			invalidEvents.Add(1)
		}
	})
	defer SetSendStageObserver(nil)
	fixture.controlApplication.hook = func(message *Message) {
		for range 3 {
			var reject MessageRejectError
			heartbeatStringResult, reject = message.Header.GetString(tagMsgType)
			if reject != nil || heartbeatStringResult != "0" {
				test.Fatalf("callback type = %q, %v", heartbeatStringResult, reject)
			}
		}
	}
	for range 66 {
		if err := fixture.process("idle_heartbeat"); err != nil {
			test.Fatal(err)
		}
	}
	warmupEvents := events.Load()
	var before, after runtime.MemStats
	var operationError error
	completed := 0
	runtime.ReadMemStats(&before)
	for range 100 {
		if operationError = fixture.process("idle_heartbeat"); operationError != nil {
			break
		}
		completed++
	}
	runtime.ReadMemStats(&after)
	allocations, allocatedBytes := after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc
	test.Logf("completed_writes=%d total_mallocs_delta=%d total_alloc_bytes_delta=%d", completed, allocations, allocatedBytes)
	if operationError != nil || completed != 100 {
		test.Fatalf("completed=%d error=%v", completed, operationError)
	}
	eventCount, invalidCount := events.Load(), invalidEvents.Load()
	if invalidCount != 0 || warmupEvents == 0 || eventCount*66 != warmupEvents*166 {
		test.Fatalf("observer events=%d warmup=%d invalid=%d", eventCount, warmupEvents, invalidCount)
	}
	if allocations != 0 || allocatedBytes != 0 {
		test.Fatalf("100 completed observed heartbeats allocated %d objects / %d bytes", allocations, allocatedBytes)
	}
}
