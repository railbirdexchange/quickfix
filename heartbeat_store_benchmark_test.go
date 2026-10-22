package quickfix

import (
	"bytes"
	"io"
	"testing"

	"github.com/quickfixgo/quickfix/internal"
)

// These external-store stand-ins retain the same bounded journal and sequence
// semantics. Only the capability offered to the session differs. The legacy
// override must not be bypassed by promoted private BoundedStore methods.
type heartbeatLegacyStore struct {
	*BoundedStore
	legacyCalls int
}

func (store *heartbeatLegacyStore) SaveMessageAndIncrNextSenderMsgSeqNum(sequence int, wire []byte) error {
	store.legacyCalls++
	return store.BoundedStore.SaveMessageAndIncrNextSenderMsgSeqNum(sequence, wire)
}

type heartbeatBorrowedStore struct {
	*heartbeatLegacyStore
	borrowedCalls int
}

func (store *heartbeatBorrowedStore) BorrowedStoreOwner() MessageStore { return store }

func (store *heartbeatBorrowedStore) SaveMessageAndIncrNextSenderMsgSeqNumBorrowed(sequence int, wire []byte) error {
	store.borrowedCalls++
	return store.BoundedStore.SaveMessageAndIncrNextSenderMsgSeqNum(sequence, wire)
}

type heartbeatStoreFixture struct {
	*scopedOutboundBenchmark
	controlApplication *scopedControlApplication
	external           *heartbeatLegacyStore
	borrowed           *heartbeatBorrowedStore
	timerDone          chan struct{}
}

func newHeartbeatStoreFixture(test testing.TB, storeKind string) *heartbeatStoreFixture {
	test.Helper()
	base, application := newScopedControlFixture(test)
	fixture := &heartbeatStoreFixture{scopedOutboundBenchmark: base, controlApplication: application, timerDone: make(chan struct{})}
	base.session.application = BorrowApplication(application)
	switch storeKind {
	case "bounded":
	case "legacy_external", "borrowed_external":
		fixture.external = &heartbeatLegacyStore{BoundedStore: base.store}
		base.session.store = fixture.external
		if storeKind == "borrowed_external" {
			fixture.borrowed = &heartbeatBorrowedStore{heartbeatLegacyStore: fixture.external}
			base.session.store = fixture.borrowed
		}
	default:
		test.Fatalf("unknown store kind %q", storeKind)
	}
	// Capability selection is session setup work. The existing fixture's writer
	// has the same fixed queue depth; no frame is in flight during reconfiguration.
	if err := base.session.configureOutbound(NewSessionSettings()); err != nil {
		test.Fatal(err)
	}
	base.session.stateTimer.Stop()
	base.session.stateTimer = internal.NewEventTimer(func() {
		select {
		case base.session.sessionEvent <- internal.NeedHeartbeat:
		case <-fixture.timerDone:
		}
	})
	return fixture
}

func (fixture *heartbeatStoreFixture) close() {
	close(fixture.timerDone)
	fixture.scopedOutboundBenchmark.close()
}

func (fixture *heartbeatStoreFixture) process(kind string) error {
	sequence := fixture.store.NextSenderMsgSeqNum()
	callbacks := fixture.controlApplication.calls
	messageType := "0"
	switch kind {
	case "idle_heartbeat":
		if err := fixture.session.sendHeartbeat(nil, nil); err != nil {
			return err
		}
	case "timer_heartbeat":
		// Expire the existing timer immediately instead of waiting an idle
		// heartbeat interval. Its real worker sends sessionEvent, and the owner
		// dispatches the production state-machine Timeout method.
		fixture.session.stateTimer.Reset(0)
		event := <-fixture.session.sessionEvent
		if event != internal.NeedHeartbeat {
			return io.ErrUnexpectedEOF
		}
		fixture.session.Timeout(fixture.session, event)
	case "test_request":
		messageType = "1"
		if err := fixture.session.sendTestRequest(); err != nil {
			return err
		}
	default:
		return io.ErrUnexpectedEOF
	}
	if fixture.store.NextSenderMsgSeqNum() != sequence+1 || fixture.controlApplication.calls != callbacks+1 {
		return ErrInvalidJournalSequence
	}
	if err := completeScopedControl(fixture.scopedOutboundBenchmark); err != nil {
		return err
	}
	message := fixture.peer.Message()
	actualType, reject := message.Header.GetBytes(tagMsgType)
	if reject != nil || string(actualType) != messageType {
		return io.ErrUnexpectedEOF
	}
	actualSequence, reject := message.Header.GetInt(tagMsgSeqNum)
	if reject != nil || actualSequence != sequence {
		return ErrInvalidJournalSequence
	}
	if messageType == "1" {
		requestID, reject := message.Body.GetBytes(tagTestReqID)
		if reject != nil || string(requestID) != "TEST" {
			return io.ErrUnexpectedEOF
		}
	} else if message.Body.Has(tagTestReqID) {
		return io.ErrUnexpectedEOF
	}
	index := fixture.store.nextEntry(sequence, sequence)
	if index < 0 || !bytes.Equal(fixture.store.entryBytes(index), fixture.connection.wire[:fixture.connection.used]) {
		return io.ErrUnexpectedEOF
	}
	fixture.connection.used = 0
	fixture.floor++
	return fixture.store.RetireBefore(fixture.floor)
}

// BenchmarkHeartbeatStore measures control construction, callbacks, sequence
// advancement, persistence, completed writer copy, returned output slots and
// peer parsing. It is a completed-write component using an independent fixed
// receive buffer, not a TCP or full-session latency benchmark. The timer row
// additionally includes the real reused EventTimer and session-event dispatch.
func BenchmarkHeartbeatStore(benchmark *testing.B) {
	for _, kind := range []string{"idle_heartbeat", "timer_heartbeat", "test_request"} {
		for _, storeKind := range []string{"bounded", "legacy_external", "borrowed_external"} {
			benchmark.Run(kind+"/"+storeKind, func(benchmark *testing.B) {
				fixture := newHeartbeatStoreFixture(benchmark, storeKind)
				defer fixture.close()
				// The underlying fixture pre-fills 1023 journal cells. These
				// completed sends fill the final cell and visit all 33 output
				// slots twice before timing, including timer/control state.
				for warmup := 0; warmup < 66; warmup++ {
					if err := fixture.process(kind); err != nil {
						benchmark.Fatal(err)
					}
				}
				benchmark.ReportAllocs()
				benchmark.ResetTimer()
				for iteration := 0; iteration < benchmark.N; iteration++ {
					if err := fixture.process(kind); err != nil {
						benchmark.Fatal(err)
					}
				}
				benchmark.StopTimer()
				if fixture.controlApplication.calls != benchmark.N+66 {
					benchmark.Fatalf("callbacks=%d, want %d", fixture.controlApplication.calls, benchmark.N+66)
				}
				if fixture.external != nil {
					calls := fixture.external.legacyCalls
					if fixture.borrowed != nil {
						calls += fixture.borrowed.borrowedCalls
					}
					if calls != benchmark.N+66 {
						benchmark.Fatalf("external store calls=%d, want %d", calls, benchmark.N+66)
					}
				}
			})
		}
	}
}
