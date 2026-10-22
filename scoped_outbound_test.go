package quickfix

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"
)

func TestOutboundRingReservationAndReplacement(t *testing.T) {
	ring, err := newOutboundRing(2, 32)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"first", "second"} {
		slot, err := ring.reserve(false)
		if err != nil {
			t.Fatal(err)
		}
		slot.wire = append(slot.wire, value...)
		ring.enqueue(slot)
	}
	if _, err := ring.reserve(false); err != ErrOutboundCapacity {
		t.Fatalf("full pending queue: %v", err)
	}
	replacement, err := ring.reserve(true)
	if err != nil {
		t.Fatal(err)
	}
	if string(ring.front().wire) != "first" || ring.count != 2 {
		t.Fatal("reservation changed the existing pending queue")
	}
	if _, err := ring.reserve(true); err != ErrOutboundCapacity {
		t.Fatalf("exhausted replacement reserve: %v", err)
	}
	// Aborting a replacement leaves every old frame in place.
	replacement.release()
	replacement, err = ring.reserve(true)
	if err != nil {
		t.Fatal(err)
	}
	replacement.wire = append(replacement.wire, "replacement"...)
	ring.dropPending()
	ring.enqueue(replacement)
	if got := string(ring.front().wire); got != "replacement" {
		t.Fatalf("replacement wire: %q", got)
	}
	ring.dropPending()
	if ring.count != 0 || len(ring.vacant) != len(ring.slots) {
		t.Fatalf("returned slots: pending=%d vacant=%d", ring.count, len(ring.vacant))
	}
	for range ring.slots {
		slot := <-ring.vacant
		if len(slot.wire) != 0 || cap(slot.wire) != 32 {
			t.Fatalf("returned frame bounds: len=%d cap=%d", len(slot.wire), cap(slot.wire))
		}
	}
}

func TestOutboundRingInvalidLimits(t *testing.T) {
	maximum := int(^uint(0) >> 1)
	for _, limits := range [][2]int{{0, 32}, {-1, 32}, {1, 0}, {1, -1}, {maximum, 32}, {2, maximum}} {
		if _, err := newOutboundRing(limits[0], limits[1]); err != ErrInvalidOutboundLimits {
			t.Fatalf("limits %v: %v", limits, err)
		}
	}
}

type scopedOutboundTestConnection struct {
	scopedOutboundBenchmarkConnection
	maxWrite int
	zero     bool
	failAt   int
	failure  error
	calls    int
	started  chan struct{}
	allow    chan struct{}
}

func (connection *scopedOutboundTestConnection) Write(wire []byte) (int, error) {
	connection.calls++
	if connection.started != nil {
		connection.started <- struct{}{}
		<-connection.allow
	}
	if connection.zero {
		return 0, nil
	}
	if connection.calls == connection.failAt {
		return 0, connection.failure
	}
	if connection.maxWrite > 0 && len(wire) > connection.maxWrite {
		wire = wire[:connection.maxWrite]
	}
	return connection.scopedOutboundBenchmarkConnection.Write(wire)
}

func TestOutboundWriterCompletionAndFailure(t *testing.T) {
	failure := errors.New("writer failed")
	for _, test := range []struct {
		name     string
		maxWrite int
		zero     bool
		failAt   int
		wantErr  error
		wantWire string
	}{
		{name: "complete", wantWire: "firstsecond"},
		{name: "short_writes", maxWrite: 2, wantWire: "firstsecond"},
		{name: "zero_progress", zero: true, wantErr: io.ErrNoProgress},
		{name: "error_reclaims_queued", failAt: 1, wantErr: failure},
		{name: "partial_then_error", maxWrite: 2, failAt: 2, wantErr: failure, wantWire: "fi"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ring, err := newOutboundRing(2, 32)
			if err != nil {
				t.Fatal(err)
			}
			connection := &scopedOutboundTestConnection{scopedOutboundBenchmarkConnection: scopedOutboundBenchmarkConnection{wire: make([]byte, 64)}, maxWrite: test.maxWrite, zero: test.zero, failAt: test.failAt, failure: failure}
			writer := newOutboundWriter(connection, 2, time.Second, make(chan bool, 1))
			for _, value := range []string{"first", "second"} {
				slot, err := ring.reserve(false)
				if err != nil {
					t.Fatal(err)
				}
				slot.wire = append(slot.wire, value...)
				<-writer.permits
				writer.ready <- slot
			}
			go writer.run()
			if err := writer.finish(); err != test.wantErr {
				t.Fatalf("writer result: got %v, want %v", err, test.wantErr)
			}
			if got := string(connection.wire[:connection.used]); got != test.wantWire {
				t.Fatalf("completed wire: got %q, want %q", got, test.wantWire)
			}
			if len(ring.vacant) != len(ring.slots) {
				t.Fatalf("returned %d of %d slots", len(ring.vacant), len(ring.slots))
			}
		})
	}
}

func TestOutboundWriterRetainsSlotUntilWriteReturns(t *testing.T) {
	ring, err := newOutboundRing(1, 32)
	if err != nil {
		t.Fatal(err)
	}
	connection := &scopedOutboundTestConnection{scopedOutboundBenchmarkConnection: scopedOutboundBenchmarkConnection{wire: make([]byte, 32)}, started: make(chan struct{}), allow: make(chan struct{})}
	wake := make(chan bool, 1)
	writer := newOutboundWriter(connection, 1, time.Second, wake)
	slot, err := ring.reserve(false)
	if err != nil {
		t.Fatal(err)
	}
	slot.wire = append(slot.wire, "owned bytes"...)
	<-writer.permits
	writer.ready <- slot
	go writer.run()
	<-connection.started
	if len(ring.vacant) != len(ring.slots)-1 {
		t.Fatal("active write returned its slot early")
	}
	select {
	case <-wake:
		t.Fatal("active write published completion")
	default:
	}
	close(connection.allow)
	<-wake
	if len(ring.vacant) != len(ring.slots) || !bytes.Equal(connection.wire[:connection.used], []byte("owned bytes")) {
		t.Fatal("completion did not return the written frame")
	}
	if err := writer.finish(); err != nil {
		t.Fatal(err)
	}
}
