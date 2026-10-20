//go:build !race

package quickfix

import (
	"runtime"
	"testing"
)

func TestSessionRequiredRejectExactAllocations(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	fixture := newRequiredValidationFixture(t, true)
	defer fixture.close()
	for range 66 {
		if err := completeRequiredValidationReject(fixture); err != nil {
			t.Fatal(err)
		}
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range 1000 {
		requiredValidationResult = fixture.session.Validate(fixture.message)
		requiredValidationText = requiredValidationResult.Error()
		requiredValidationText = requiredValidationResult.Error()
	}
	runtime.ReadMemStats(&after)
	t.Logf("validations=1000 text_reads=2000 mallocs=%d bytes=%d", after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc)
	if after.Mallocs != before.Mallocs || after.TotalAlloc != before.TotalAlloc {
		t.Fatal("warmed required-field validation allocated")
	}
	runtime.ReadMemStats(&before)
	for range 100 {
		if err := completeRequiredValidationReject(fixture); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	t.Logf("completed_rejects=100 mallocs=%d bytes=%d", after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc)
	if after.Mallocs != before.Mallocs || after.TotalAlloc != before.TotalAlloc {
		t.Fatal("warmed completed rejects allocated")
	}
}
