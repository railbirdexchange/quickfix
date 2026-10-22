//go:build !race

package quickfix

import (
	"runtime"
	"testing"
)

func TestTimestampValidationExactAllocations(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	for _, scenario := range timestampValidationBenchmarks {
		t.Run(scenario.name, func(t *testing.T) {
			value := []byte(scenario.value)
			for range 66 {
				timestampValidationAccepted = validFieldValue("UTCTIMESTAMP", value)
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for range 1000 {
				timestampValidationAccepted = validFieldValue("UTCTIMESTAMP", value)
			}
			runtime.ReadMemStats(&after)
			t.Logf("validations=1000 mallocs=%d bytes=%d", after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc)
			if timestampValidationAccepted != scenario.valid {
				t.Fatal("timestamp acceptance changed")
			}
			if after.Mallocs != before.Mallocs || after.TotalAlloc != before.TotalAlloc {
				t.Fatal("boolean timestamp validation allocated")
			}
		})
	}
}

func TestSessionTimestampRejectExactAllocations(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	fixture := newTimestampValidationFixture(t, []byte("19000229-01:02:03.123"), false)
	defer fixture.close()
	for range 66 {
		if err := completeTimestampValidationReject(fixture); err != nil {
			t.Fatal(err)
		}
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range 100 {
		if err := completeTimestampValidationReject(fixture); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	t.Logf("completed_rejects=100 mallocs=%d bytes=%d", after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc)
	if after.Mallocs != before.Mallocs || after.TotalAlloc != before.TotalAlloc {
		t.Fatal("completed timestamp reject allocated")
	}
}
