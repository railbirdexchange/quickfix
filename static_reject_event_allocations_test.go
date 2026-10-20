//go:build !race

package quickfix

import (
	"runtime"
	"testing"
)

func TestStaticRejectEventExactAllocations(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	for _, reason := range fixedRejectEventReasons() {
		t.Run(reason, func(t *testing.T) {
			s, log := newRetainedRejectEventSession(256, true, true)
			for range 66 {
				s.logControlReject(reason)
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for range 1000 {
				s.logControlReject(reason)
			}
			runtime.ReadMemStats(&after)
			t.Logf("retained_events=1000 mallocs=%d bytes=%d", after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc)
			if after.Mallocs != before.Mallocs || after.TotalAlloc != before.TotalAlloc {
				t.Fatal("fixed owned reject event allocated")
			}
			if log.ownedCalls != 1066 || log.next != 1066 || log.borrowedCalls != 0 || log.formattedCalls != 0 {
				t.Fatal("allocation interval did not deliver every event to the retaining hook")
			}
		})
	}
}
