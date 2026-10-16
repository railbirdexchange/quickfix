package quickfix

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/datadictionary"
)

const boundedConnectionTestTimeout = 5 * time.Second

type boundedConnectionObservedReader struct {
	reader  io.Reader
	started chan struct{}
	reads   chan struct{}
	read    bool
}

func (reader *boundedConnectionObservedReader) Read(dst []byte) (int, error) {
	if !reader.read {
		reader.read = true
		close(reader.started)
	}
	count, err := reader.reader.Read(dst)
	if count > 0 && reader.reads != nil {
		reader.reads <- struct{}{}
	}
	return count, err
}

type boundedConnectionDeadlineFailure struct{ net.Conn }

func (connection boundedConnectionDeadlineFailure) SetReadDeadline(time.Time) error {
	return io.ErrClosedPipe
}

func boundedConnectionTestPipe(t *testing.T) net.Conn {
	t.Helper()
	connection, peer := net.Pipe()
	t.Cleanup(func() {
		_ = connection.Close()
		_ = peer.Close()
	})
	return connection
}

func boundedConnectionTestInbound(t *testing.T, queue int) *boundedInboundSession {
	t.Helper()
	limits := inboundRingTestLimits()
	ring, err := newInboundRing(limits, queue, 0)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := NewMessageSlot(nil, nil, limits)
	if err != nil {
		t.Fatal(err)
	}
	return &boundedInboundSession{limits: limits, ring: ring, parsed: parsed, log: nullLog{}}
}

func boundedConnectionTestWait(t *testing.T, done <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(boundedConnectionTestTimeout):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func boundedConnectionTestReceive(t *testing.T, messageIn <-chan fixIn) fixIn {
	t.Helper()
	select {
	case message, ok := <-messageIn:
		if !ok {
			t.Fatal("inbound channel closed before expected message")
		}
		return message
	case <-time.After(boundedConnectionTestTimeout):
		t.Fatal("timed out waiting for inbound message")
		return fixIn{}
	}
}

func boundedConnectionTestStart(t *testing.T, connection net.Conn, previous *parser, inbound *boundedInboundSession, queue int, first []byte, received time.Time) (*boundedReadConnection, chan fixIn, func()) {
	t.Helper()
	messageIn := make(chan fixIn, queue)
	control, err := startBoundedRead(connection, previous, inbound, messageIn, first, received)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		t.Helper()
		if stopped {
			return
		}
		stopped = true
		done := make(chan struct{})
		go func() {
			control.stopAndDrain(messageIn)
			close(done)
		}()
		boundedConnectionTestWait(t, done, "reader cancellation and drain")
	}
	t.Cleanup(stop)
	return control, messageIn, stop
}

func TestBoundedConnectionStopBlockedRead(t *testing.T) {
	for _, failDeadline := range []bool{false, true} {
		name := "read_deadline"
		if failDeadline {
			name = "close_fallback"
		}
		t.Run(name, func(t *testing.T) {
			connection := boundedConnectionTestPipe(t)
			if failDeadline {
				connection = boundedConnectionDeadlineFailure{connection}
			}
			observed := &boundedConnectionObservedReader{reader: connection, started: make(chan struct{})}
			inbound := boundedConnectionTestInbound(t, 0)
			control, messageIn, stop := boundedConnectionTestStart(t, connection, newParser(observed), inbound, 0, nil, time.Time{})
			boundedConnectionTestWait(t, observed.started, "socket read")
			stop()
			boundedConnectionTestWait(t, control.done, "reader completion")
			if _, open := <-messageIn; open {
				t.Fatal("stop left inbound channel open")
			}
			assertInboundRingVacant(t, inbound.ring)
		})
	}
}

func TestBoundedConnectionStopBlockedInboundSend(t *testing.T) {
	// With no receiver, the first complete frame remains reader-owned until
	// cancellation or the drain transfers it. Both paths must return one slot.
	observed := &boundedConnectionObservedReader{
		reader: bytes.NewBufferString(boundedHeartbeatFrame), started: make(chan struct{}),
	}
	inbound := boundedConnectionTestInbound(t, 0)
	_, _, stop := boundedConnectionTestStart(t, boundedConnectionTestPipe(t), newParser(observed), inbound, 0, nil, time.Time{})
	boundedConnectionTestWait(t, observed.started, "frame read before inbound send")
	stop()
	assertInboundRingVacant(t, inbound.ring)
}

func TestBoundedConnectionStopDrainsQueuedSlots(t *testing.T) {
	const queueDepth = 2
	observed := &boundedConnectionObservedReader{
		reader:  &allocationBaselineRepeatingReader{message: []byte(boundedHeartbeatFrame)},
		started: make(chan struct{}), reads: make(chan struct{}, queueDepth+1),
	}
	inbound := boundedConnectionTestInbound(t, queueDepth)
	_, messageIn, stop := boundedConnectionTestStart(t, boundedConnectionTestPipe(t), newParser(observed), inbound, queueDepth, nil, time.Time{})
	for count := 0; count < queueDepth+1; count++ {
		boundedConnectionTestWait(t, observed.reads, "queued frames and blocked reader-owned frame")
	}
	if len(messageIn) != queueDepth {
		t.Fatal("reader did not fill the inbound queue before acquiring its next frame")
	}
	stop()
	assertInboundRingVacant(t, inbound.ring)
}

func TestBoundedConnectionSlowConsumerAndReconnect(t *testing.T) {
	inbound := boundedConnectionTestInbound(t, 0)
	firstWire := []byte(boundedHeartbeatFrame)
	secondWire := []byte("8=FIX.4.4\x019=5\x0135=1\x0110=164\x01")
	wire := append(append(append([]byte(nil), firstWire...), secondWire...), firstWire...)
	_, messageIn, stop := boundedConnectionTestStart(t, boundedConnectionTestPipe(t), newParser(bytes.NewReader(wire)), inbound, 0, nil, time.Time{})
	first := boundedConnectionTestReceive(t, messageIn)
	second := boundedConnectionTestReceive(t, messageIn)
	if first.slot == nil || second.slot == nil || first.slot == second.slot {
		t.Fatal("reader did not transfer two distinct slots")
	}
	if len(inbound.ring.vacant) != 0 {
		t.Fatal("reader exposed an extra vacant slot while consumer owns every slot")
	}
	s := &session{boundedIn: inbound}
	active, err := s.parseInboundSlot(first.slot)
	if err != nil {
		t.Fatal(err)
	}
	borrowedType, reject := active.Header.GetBytes(tagMsgType)
	if reject != nil || string(borrowedType) != "0" {
		t.Fatalf("borrowed message type = %q, reject = %v", borrowedType, reject)
	}
	// The next frame cannot acquire a vacant slot. Shutdown must complete
	// without taking either slot or invalidating the slow consumer's view.
	stop()
	if len(inbound.ring.vacant) != 0 || !bytes.Equal(first.slot.frame, firstWire) || !bytes.Equal(second.slot.frame, secondWire) || string(borrowedType) != "0" {
		t.Fatal("shutdown changed a consumer-owned frame or borrowed field")
	}
	second.slot.release()
	if inbound.parsed.Message() != active || string(borrowedType) != "0" {
		t.Fatal("returning another raw slot reset active session scratch")
	}
	inbound.parsed.Reset()
	first.slot.release()
	assertInboundRingVacant(t, inbound.ring)

	// A new connection reuses exactly the same ring after the old reader exits.
	_, reconnected, stopReconnected := boundedConnectionTestStart(t, boundedConnectionTestPipe(t), newParser(bytes.NewReader(secondWire)), inbound, 0, nil, time.Time{})
	message := boundedConnectionTestReceive(t, reconnected)
	if message.slot == nil || !bytes.Equal(message.slot.frame, secondWire) {
		t.Fatal("reconnected reader did not transfer its new frame")
	}
	message.slot.release()
	stopReconnected()
	assertInboundRingVacant(t, inbound.ring)
}

func TestScopedInboundReaderCancellationDoesNotResetActiveScratch(t *testing.T) {
	inbound := boundedConnectionTestInbound(t, 0)
	connection := boundedConnectionTestPipe(t)
	observed := &boundedConnectionObservedReader{reader: connection, started: make(chan struct{})}
	_, incoming, stop := boundedConnectionTestStart(t, connection, newParser(observed), inbound, 0,
		[]byte(boundedHeartbeatFrame), time.Now())
	message := boundedConnectionTestReceive(t, incoming)
	boundedConnectionTestWait(t, observed.started, "reader owning another slot during a blocked read")
	s := &session{boundedIn: inbound}
	active, err := s.parseInboundSlot(message.slot)
	if err != nil {
		t.Fatal(err)
	}
	value, reject := active.Header.GetBytes(tagMsgType)
	if reject != nil {
		t.Fatal(reject)
	}
	stop()
	if inbound.parsed.Message() != active || active.inbound != message.slot || string(value) != "0" || string(active.Bytes()) != boundedHeartbeatFrame {
		t.Fatal("cancelling and releasing the reader-owned slot changed active session scratch")
	}
	if len(inbound.ring.vacant) != len(inbound.ring.slots)-1 {
		t.Fatal("reader cancellation returned the active frame or leaked its own frame")
	}
	inbound.parsed.Reset()
	message.slot.release()
	assertInboundRingVacant(t, inbound.ring)
}

func TestBoundedConnectionFirstFrameAndCoalescedRemainder(t *testing.T) {
	first := []byte(boundedHeartbeatFrame)
	second := []byte("8=FIX.4.4\x019=5\x0135=1\x0110=164\x01")
	third := []byte("8=FIX.4.4\x019=5\x0135=2\x0110=165\x01")
	previous := newParser(bytes.NewReader(third))
	previous.buffer = second
	inbound := boundedConnectionTestInbound(t, 3)
	received := time.Date(2026, time.October, 15, 12, 0, 0, 123, time.UTC)
	control, messageIn, stop := boundedConnectionTestStart(t, boundedConnectionTestPipe(t), previous, inbound, 3, first, received)
	for index, expected := range [][]byte{first, second, third} {
		message := boundedConnectionTestReceive(t, messageIn)
		if message.slot == nil || !bytes.Equal(message.slot.frame, expected) {
			t.Fatalf("frame %d was lost or reordered", index)
		}
		if index == 0 && !message.receiveTime.Equal(received) {
			t.Fatal("discovery frame lost its original receive time")
		}
		if index != 0 && message.receiveTime.IsZero() {
			t.Fatal("following frame has no receive time")
		}
		if &message.slot.frame[0] == &expected[0] {
			t.Fatal("reader transferred borrowed discovery storage instead of a ring slot")
		}
		message.slot.release()
	}
	terminal := boundedConnectionTestReceive(t, messageIn)
	if terminal.slot != nil || terminal.readError != io.EOF {
		t.Fatalf("terminal read = %#v, want EOF without a slot", terminal)
	}
	boundedConnectionTestWait(t, control.done, "EOF completion")
	stop()
	assertInboundRingVacant(t, inbound.ring)
}

func TestBoundedConnectionFrameCapacityTerminalError(t *testing.T) {
	inbound := boundedConnectionTestInbound(t, 0)
	previous := newParser(bytes.NewBufferString("8=FIX.4.4\x019=9999\x01"))
	control, messageIn, stop := boundedConnectionTestStart(t, boundedConnectionTestPipe(t), previous, inbound, 0, nil, time.Time{})
	terminal := boundedConnectionTestReceive(t, messageIn)
	if terminal.slot != nil || terminal.readError != ErrFrameCapacity {
		t.Fatalf("terminal read = %#v, want frame capacity without a slot", terminal)
	}
	boundedConnectionTestWait(t, control.done, "capacity error completion")
	stop()
	assertInboundRingVacant(t, inbound.ring)
}

func TestBoundedConnectionRejectOversizedFirstFrame(t *testing.T) {
	inbound := boundedConnectionTestInbound(t, 0)
	messageIn := make(chan fixIn)
	control, err := startBoundedRead(boundedConnectionTestPipe(t), newParser(bytes.NewReader(nil)), inbound, messageIn,
		make([]byte, inbound.limits.MaxFrameBytes+1), time.Time{})
	if control != nil || err != ErrFrameCapacity {
		t.Fatalf("oversized discovery frame: reader = %v, error = %v", control, err)
	}
	assertInboundRingVacant(t, inbound.ring)
}

type boundedConnectionRefreshFailure struct {
	*memoryStore
	refreshCalls int
}

func (store *boundedConnectionRefreshFailure) Refresh() error {
	store.refreshCalls++
	return io.ErrUnexpectedEOF
}

func TestBoundedConnectionFailedLogonPreparationReclaimsAcceptedReader(t *testing.T) {
	inbound := boundedConnectionTestInbound(t, 1)
	store := &boundedConnectionRefreshFailure{memoryStore: &memoryStore{}}
	if err := store.memoryStore.Reset(); err != nil {
		t.Fatal(err)
	}
	if err := store.SetNextSenderMsgSeqNum(7); err != nil {
		t.Fatal(err)
	}
	if err := store.SetNextTargetMsgSeqNum(11); err != nil {
		t.Fatal(err)
	}
	s := &session{store: store, log: nullLog{}, boundedIn: inbound, stateMachine: stateMachine{State: latentState{}}}
	s.RefreshOnLogon = true
	for _, failPreparation := range []bool{true, false} {
		connection := boundedConnectionTestPipe(t)
		observed := &boundedConnectionObservedReader{reader: connection, started: make(chan struct{})}
		messageIn := make(chan fixIn, 1)
		messageOut := make(chan []byte, 1)
		control, err := startBoundedRead(connection, newParser(observed), inbound, messageIn, []byte(boundedHeartbeatFrame), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			select {
			case <-control.stop:
			default:
				control.stopAndDrain(messageIn)
			}
		})
		// The discovery frame is queued and the reader owns another slot in a
		// blocked socket read before the session accepts this connection.
		boundedConnectionTestWait(t, observed.started, "read following queued discovery frame")
		s.InitiateLogon = failPreparation
		// A nil acknowledgement channel avoids involving writer startup here.
		// Production still acknowledges before Connect prepares the logon.
		s.onAdmin(connect{messageIn: messageIn, messageOut: messageOut, reader: control})
		if failPreparation {
			if s.IsConnected() || store.refreshCalls != 1 {
				t.Fatal("failed refresh did not leave the accepted session disconnected")
			}
		} else {
			// Reuse the same ring for a subsequent accepted connection. Acceptor
			// mode avoids logon timers and permits a direct frame handoff check.
			if !s.IsConnected() || s.readConnection != control || s.messageIn != messageIn {
				t.Fatal("subsequent connection could not reuse the session ring")
			}
			message := boundedConnectionTestReceive(t, messageIn)
			if message.slot == nil || string(message.slot.frame) != boundedHeartbeatFrame {
				t.Fatal("subsequent connection lost its queued discovery frame")
			}
			message.slot.release()
			s.Disconnected(s)
		}
		boundedConnectionTestWait(t, control.done, "accepted reader teardown")
		if s.messageIn != nil || s.messageOut != nil || s.readConnection != nil || s.IsConnected() {
			t.Fatal("disconnect retained connection channels, reader, or connected state")
		}
		select {
		case _, open := <-messageOut:
			if open {
				t.Fatal("disconnect left the outbound channel open")
			}
		default:
			t.Fatal("disconnect did not close the outbound channel")
		}
		if store.NextSenderMsgSeqNum() != 7 || store.NextTargetMsgSeqNum() != 11 || len(store.messageMap) != 0 {
			t.Fatal("failed preparation or reader teardown changed sequence/store state")
		}
		assertInboundRingVacant(t, inbound.ring)
	}
}

var boundedOwnershipValue byte

func boundedOwnershipFixture(t testing.TB, groups bool) ([]byte, *datadictionary.DataDictionary) {
	t.Helper()
	if !groups {
		_, wire, dictionary := allocationBaselineFIX40Order(t)
		return wire, dictionary
	}
	dictionary, err := datadictionary.Parse("spec/FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	return []byte(allocationBaselineRepeatingGroupMessage), dictionary
}

func boundedOwnershipHandle(t testing.TB, slot *inboundSlot, parsed *MessageSlot, validator Validator, groups bool) {
	if err := parsed.Parse(slot.frame); err != nil {
		t.Fatal(err)
	}
	if reject := validator.Validate(parsed.Message()); reject != nil {
		t.Fatal(reject)
	}
	value, reject := parsed.Message().Body.GetBytes(11)
	if reject != nil || len(value) == 0 {
		t.Fatalf("borrowed ClOrdID = %q, reject = %v", value, reject)
	}
	boundedOwnershipValue = value[0]
	if groups {
		group, err := parsed.Group(453)
		if err != nil || group.Len() != 1 {
			t.Fatalf("borrowed Parties length = %d, error = %v", group.Len(), err)
		}
		iterator := group.Iter()
		entry, ok := iterator.Next()
		if !ok {
			t.Fatal("borrowed Parties entry missing")
		}
		party, ok := entry.GetBytes(448)
		if !ok || string(party) != "4501" {
			t.Fatalf("borrowed PartyID = %q", party)
		}
		boundedOwnershipValue = party[0]
		if _, ok := iterator.Next(); ok {
			t.Fatal("borrowed Parties has an extra entry")
		}
	}
	parsed.Reset()
	slot.release()
}

func TestBoundedOwnershipFixtures(t *testing.T) {
	for _, groups := range []bool{false, true} {
		wire, dictionary := boundedOwnershipFixture(t, groups)
		limits := inboundRingTestLimits()
		ring, err := newInboundRing(limits, 2, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := NewMessageSlot(dictionary, dictionary, limits)
		if err != nil {
			t.Fatal(err)
		}
		validator := NewValidator(defaultValidatorSettings, dictionary, nil)
		for range ring.slots {
			slot := inboundRingTestAcquire(t, ring)
			slot.frame = slot.frame[:len(wire)]
			copy(slot.frame, wire)
			boundedOwnershipHandle(t, slot, parsed, validator, groups)
		}
		assertInboundRingVacant(t, ring)
	}
}

// BenchmarkBoundedInboundOwnership measures framing, channel ownership transfer,
// dictionary/group parsing using one session scratch, ordinary validation,
// borrowed handler reads, and raw-slot return.
// It does not exercise session sequence/store state or socket writes; those need
// the separate full-session qualification. Every timed iteration ends only once
// its slot has been returned. Setup, all-slot warm-up, and shutdown are untimed.
func BenchmarkBoundedInboundOwnership(b *testing.B) {
	for _, groups := range []bool{false, true} {
		name := "new_order_single"
		if groups {
			name = "repeating_group"
		}
		b.Run(name, func(b *testing.B) {
			wire, dictionary := boundedOwnershipFixture(b, groups)
			limits := DefaultParseLimits()
			const queueDepth, stashCapacity = 32, 32
			ring, err := newInboundRing(limits, queueDepth, stashCapacity)
			if err != nil {
				b.Fatal(err)
			}
			parsed, err := NewMessageSlot(dictionary, dictionary, limits)
			if err != nil {
				b.Fatal(err)
			}
			validator := NewValidator(defaultValidatorSettings, dictionary, nil)
			for range ring.slots {
				slot := <-ring.vacant
				slot.frame = slot.frame[:len(wire)]
				copy(slot.frame, wire)
				boundedOwnershipHandle(b, slot, parsed, validator, groups)
			}
			parser, err := newBoundedParser(&allocationBaselineRepeatingReader{message: wire}, limits.MaxFrameBytes)
			if err != nil {
				b.Fatal(err)
			}
			messageIn := make(chan fixIn, queueDepth)
			control := &boundedReadConnection{parser: parser, ring: ring, stop: make(chan struct{}), done: make(chan struct{}), messageIn: messageIn}
			go control.read(nil, time.Time{})
			defer func() {
				b.StopTimer()
				close(control.stop)
				for message := range messageIn {
					if message.slot != nil {
						message.slot.release()
					}
				}
				<-control.done
			}()
			for range ring.slots {
				message := <-messageIn
				if message.slot == nil {
					b.Fatal("reader stopped during warm-up")
				}
				boundedOwnershipHandle(b, message.slot, parsed, validator, groups)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(wire)))
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				message := <-messageIn
				if message.slot == nil {
					b.Fatal("reader stopped during measurement")
				}
				boundedOwnershipHandle(b, message.slot, parsed, validator, groups)
			}
		})
	}
}
