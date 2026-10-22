//go:build !race

package quickfix

import (
	"runtime"
	"testing"
)

func TestPreparedMissingRejectExactAllocations(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	wire, dictionary := boundedOwnershipFixture(t, true)
	slot, err := NewMessageSlot(dictionary, dictionary, DefaultParseLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := slot.Parse(wire); err != nil {
		t.Fatal(err)
	}
	fields := &slot.Message().Body.FieldMap
	read := func() {
		_, missingFieldRejectResult = fields.GetString(526)
		missingFieldRejectText = missingFieldRejectResult.Error()
		missingFieldRejectText = missingFieldRejectResult.Error()
	}
	read()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range 1000 {
		read()
	}
	runtime.ReadMemStats(&after)
	t.Logf("reads=1000 text_reads=2000 total_mallocs_delta=%d total_alloc_bytes_delta=%d", after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc)
	if after.Mallocs != before.Mallocs || after.TotalAlloc != before.TotalAlloc {
		t.Fatalf("1000 configured missing-field reads and two text reads each allocated %d objects, %d bytes", after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc)
	}
}

func TestPreparedRejectCompletedHeartbeatsExactAllocations(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	fixture := newPreparedRejectHeartbeatFixture(t)
	defer fixture.close()
	fixture.controlApplication.hook = func(message *Message) {
		for _, tag := range [...]Tag{11, 41, 526} {
			_, missingFieldRejectResult = message.Body.GetString(tag)
			missingFieldRejectText = missingFieldRejectResult.Error()
			missingFieldRejectText = missingFieldRejectResult.Error()
		}
	}
	for range 66 {
		if err := fixture.process("idle_heartbeat"); err != nil {
			t.Fatal(err)
		}
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range 100 {
		if err := fixture.process("idle_heartbeat"); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	t.Logf("completed_writes=100 total_mallocs_delta=%d total_alloc_bytes_delta=%d", after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc)
	if after.Mallocs != before.Mallocs || after.TotalAlloc != before.TotalAlloc {
		t.Fatalf("100 completed heartbeats with missing-field/text reads allocated %d objects, %d bytes", after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc)
	}
}
