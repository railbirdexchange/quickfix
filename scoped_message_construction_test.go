package quickfix

import (
	"bytes"
	"strings"
	"testing"
)

func TestBorrowedSendAdoptsCallerConstructionScope(t *testing.T) {
	fixture := newScopedOutboundBenchmarkWithDelivery(t, false, "", true)
	defer fixture.close()
	if err := fixture.process(); err != nil {
		t.Fatal(err)
	}
	if fixture.message.arena != nil {
		t.Fatal("a single-use caller paid for a reusable construction arena")
	}
	if err := fixture.process(); err != nil {
		t.Fatal(err)
	}
	if fixture.message.arena == nil || fixture.message.arena.owner != fixture.message {
		t.Fatal("second ordinary send did not establish caller-owned storage")
	}
	arena := fixture.message.arena
	for index, identifier := range []string{"short", "a-much-longer-order-identifier", "x", "another-order"} {
		fixture.message.Body.SetString(11, identifier)
		fixture.message.Body.SetInt(38, index+1)
		sequence := fixture.store.NextSenderMsgSeqNum()
		if err := fixture.process(); err != nil {
			t.Fatal(err)
		}
		retained, ok := fixture.store.message(sequence)
		if !ok {
			t.Fatal("ordinary send did not retain its wire")
		}
		if err := fixture.peer.Parse(retained); err != nil {
			t.Fatal(err)
		}
		if got, _ := fixture.peer.Message().Body.GetString(11); got != identifier {
			t.Fatalf("wire ClOrdID = %q, want %q", got, identifier)
		}
		if got, _ := fixture.peer.Message().Body.GetInt(38); got != index+1 {
			t.Fatalf("wire quantity = %d", got)
		}
		fixture.peer.Reset()
		if fixture.message.arena != arena {
			t.Fatal("ordinary mutation replaced fixed caller storage")
		}
	}
}

func TestBorrowedConstructionCapacityPrecedesPersistence(t *testing.T) {
	fixture := newScopedOutboundBenchmarkWithDelivery(t, false, "", true)
	defer fixture.close()
	for range 2 {
		if err := fixture.process(); err != nil {
			t.Fatal(err)
		}
	}
	sequence := fixture.store.NextSenderMsgSeqNum()
	calls := fixture.application.calls
	vacant := len(fixture.session.outbound.vacant)
	oldWire := arenaTestWire(t, fixture.message)
	limits := fixture.session.boundedIn.limits
	fixture.session.boundedIn.limits.MaxFields /= 2
	if err := SendToTarget(fixture.message, fixture.session.sessionID); err != errMessageArenaLimits {
		t.Fatalf("changed session limits: %v", err)
	}
	fixture.session.boundedIn.limits = limits
	if !bytes.Equal(arenaTestWire(t, fixture.message), oldWire) {
		t.Fatal("capacity mismatch changed caller fields")
	}
	fixture.message.Body.SetString(11, strings.Repeat("x", limits.MaxFrameBytes))
	if err := SendToTarget(fixture.message, fixture.session.sessionID); err != ErrFrameCapacity {
		t.Fatalf("overflow was not reported by ordinary send: %v", err)
	}
	if fixture.store.NextSenderMsgSeqNum() != sequence || fixture.application.calls != calls ||
		len(fixture.session.outbound.vacant) != vacant || fixture.session.outbound.count != 0 {
		t.Fatal("construction failure changed sequence, callback or output ownership")
	}
	if _, exists := fixture.store.message(sequence); exists {
		t.Fatal("construction failure persisted a message")
	}
}

func TestBorrowedChangingScalarsThroughCompletedWriteAllocateZero(t *testing.T) {
	fixture := newScopedOutboundBenchmarkWithDelivery(t, false, "", true)
	defer fixture.close()
	identifiers := [...]string{"order-a", "longer-order-identifier", "b"}
	symbol := []byte("SYMBOL")
	iteration := 0
	process := func() error {
		fixture.message.Body.SetString(11, identifiers[iteration%len(identifiers)])
		fixture.message.Body.SetInt(38, iteration+1)
		fixture.message.Body.SetBytes(55, symbol)
		fixture.message.Header.SetBool(tagPossDupFlag, false)
		iteration++
		return fixture.process()
	}
	for range 66 {
		if err := process(); err != nil {
			t.Fatal(err)
		}
	}
	var operationError error
	allocations := testing.AllocsPerRun(100, func() { operationError = process() })
	if operationError != nil || allocations != 0 {
		t.Fatalf("ordinary scalar construction through completed write: error %v, allocations %g", operationError, allocations)
	}
}
