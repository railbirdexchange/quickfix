package quickfix

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/config"
	"github.com/quickfixgo/quickfix/internal"
)

type scopedOutboundBenchmarkApplication struct {
	message *Message
	calls   int
}

func (*scopedOutboundBenchmarkApplication) OnCreate(SessionID)          {}
func (*scopedOutboundBenchmarkApplication) OnLogon(SessionID)           {}
func (*scopedOutboundBenchmarkApplication) OnLogout(SessionID)          {}
func (*scopedOutboundBenchmarkApplication) ToAdmin(*Message, SessionID) {}
func (application *scopedOutboundBenchmarkApplication) ToApp(message *Message, _ SessionID) error {
	if message != application.message {
		return io.ErrUnexpectedEOF
	}
	if _, reject := message.Body.GetBytes(11); reject != nil {
		return reject
	}
	application.calls++
	return nil
}
func (*scopedOutboundBenchmarkApplication) FromAdmin(*Message, SessionID) MessageRejectError {
	return nil
}
func (*scopedOutboundBenchmarkApplication) FromApp(*Message, SessionID) MessageRejectError {
	return nil
}

// Write copies into an independent, fixed peer receive buffer. The benchmark
// reads that buffer only after the writer's post-Write completion notification.
type scopedOutboundBenchmarkConnection struct {
	wire []byte
	used int
}

func (*scopedOutboundBenchmarkConnection) Read([]byte) (int, error) { return 0, io.EOF }
func (connection *scopedOutboundBenchmarkConnection) Write(wire []byte) (int, error) {
	if len(wire) > len(connection.wire)-connection.used {
		return 0, io.ErrShortBuffer
	}
	written := copy(connection.wire[connection.used:], wire)
	connection.used += written
	return written, nil
}
func (*scopedOutboundBenchmarkConnection) Close() error                     { return nil }
func (*scopedOutboundBenchmarkConnection) LocalAddr() net.Addr              { return nil }
func (*scopedOutboundBenchmarkConnection) RemoteAddr() net.Addr             { return nil }
func (*scopedOutboundBenchmarkConnection) SetDeadline(time.Time) error      { return nil }
func (*scopedOutboundBenchmarkConnection) SetReadDeadline(time.Time) error  { return nil }
func (*scopedOutboundBenchmarkConnection) SetWriteDeadline(time.Time) error { return nil }

type scopedOutboundBenchmark struct {
	session     *session
	application *scopedOutboundBenchmarkApplication
	message     *Message
	store       *BoundedStore
	connection  *scopedOutboundBenchmarkConnection
	peer        *MessageSlot
	legacyOut   chan []byte
	legacyDone  chan struct{}
	floor       int
}

func newScopedOutboundBenchmark(test testing.TB, groups bool, messageType string) *scopedOutboundBenchmark {
	test.Helper()
	wire, dictionary := boundedOwnershipFixture(test, groups)
	message := NewMessage()
	if err := ParseMessageWithDataDictionary(message, bytes.NewBuffer(wire), dictionary, dictionary); err != nil {
		test.Fatal(err)
	}
	if messageType != "" {
		message.Header.SetString(tagMsgType, messageType)
	}
	beginString, _ := message.Header.GetString(tagBeginString)
	sender, _ := message.Header.GetString(tagSenderCompID)
	target, _ := message.Header.GetString(tagTargetCompID)
	application := &scopedOutboundBenchmarkApplication{message: message}
	settings := NewSessionSettings()
	settings.Set(config.DataDictionary, "spec/FIX40.xml")
	if groups {
		settings.Set(config.DataDictionary, "spec/FIX44.xml")
	}
	current, err := (sessionFactory{}).newSession(SessionID{BeginString: beginString, SenderCompID: sender, TargetCompID: target, Qualifier: "ordinary-outbound-benchmark"}, NewBoundedStoreFactory(1024, 16384), settings, NewNullLogFactory(), application)
	if err != nil {
		test.Fatal(err)
	}
	if err := registerSession(current); err != nil {
		test.Fatal(err)
	}
	current.State = inSession{}
	current.HeartBtInt = time.Hour
	current.stateTimer = internal.NewEventTimer(func() {})
	current.peerTimer = internal.NewEventTimer(func() {})
	peer, err := NewMessageSlot(dictionary, dictionary, DefaultParseLimits())
	if err != nil {
		test.Fatal(err)
	}
	fixture := &scopedOutboundBenchmark{session: current, application: application, message: message,
		store: current.store.(*BoundedStore), connection: &scopedOutboundBenchmarkConnection{wire: make([]byte, 16384)}, peer: peer, floor: 1}
	// Exercise the configured 1024-entry retention window rather than a tiny
	// or empty journal. Each measured acknowledgement retires one old entry,
	// leaving 1023 retained messages for the following operation.
	for sequence := 1; sequence < 1024; sequence++ {
		message.Header.SetInt(tagMsgSeqNum, sequence)
		if err := fixture.store.SaveMessageAndIncrNextSenderMsgSeqNum(sequence, message.build()); err != nil {
			test.Fatal(err)
		}
	}
	if current.outbound == nil {
		fixture.legacyOut = make(chan []byte, 32)
		fixture.legacyDone = make(chan struct{})
		current.messageOut = fixture.legacyOut
		go func() {
			defer close(fixture.legacyDone)
			writeLoopWithCompletion(fixture.connection, fixture.legacyOut, current.log, current.messageEvent)
		}()
	} else {
		current.writeConnection = newOutboundWriter(fixture.connection, len(current.outbound.pending), time.Second, current.messageEvent)
		go current.writeConnection.run()
	}
	return fixture
}

func (fixture *scopedOutboundBenchmark) process() error {
	expected := fixture.store.NextSenderMsgSeqNum()
	if err := SendToTarget(fixture.message, fixture.session.sessionID); err != nil {
		return err
	}
	// Consume queue admission separately from the completion notification.
	<-fixture.session.messageEvent
	fixture.session.sendMutex.Lock()
	fixture.session.sendQueued(true)
	fixture.session.sendMutex.Unlock()
	<-fixture.session.messageEvent
	if fixture.session.outbound != nil && len(fixture.session.outbound.vacant) != len(fixture.session.outbound.slots) {
		return ErrOutboundCapacity
	}
	if err := fixture.peer.Parse(fixture.connection.wire[:fixture.connection.used]); err != nil {
		return err
	}
	sequence, reject := fixture.peer.Message().Header.GetInt(tagMsgSeqNum)
	if reject != nil {
		return reject
	}
	if sequence != expected {
		return ErrInvalidJournalSequence
	}
	fixture.peer.Reset()
	fixture.connection.used = 0
	// The peer has consumed the completed bytes. Its explicit retention
	// acknowledgement releases one oldest entry; a socket write alone does not.
	fixture.floor++
	return fixture.store.RetireBefore(fixture.floor)
}

func (fixture *scopedOutboundBenchmark) close() {
	if fixture.legacyOut != nil {
		close(fixture.legacyOut)
		<-fixture.legacyDone
	} else {
		_ = fixture.session.writeConnection.finish()
	}
	fixture.session.peerTimer.Stop()
	fixture.session.stateTimer.Stop()
	_ = UnregisterSession(fixture.session.sessionID)
}

// BenchmarkScopedOutboundSession measures ordinary SendToTarget with a reused
// caller-owned Message, normal ToApp callback, header preparation, persistence,
// encoding, completed copy to the peer, output-slot return, peer parsing, and
// explicit journal retirement. It is a completed-write component benchmark,
// not a network, inbound-session, or round-trip latency measurement.
func BenchmarkScopedOutboundSession(benchmark *testing.B) {
	for _, scenario := range []struct {
		name, messageType string
		groups            bool
	}{
		{name: "new_order_single"},
		{name: "repeating_group", groups: true},
		{name: "custom_message_type", messageType: "CUSTOM.APPLICATION.MESSAGE.TYPE"},
	} {
		benchmark.Run(scenario.name, func(benchmark *testing.B) {
			fixture := newScopedOutboundBenchmark(benchmark, scenario.groups, scenario.messageType)
			defer fixture.close()
			// FIFO vacant-slot transfer visits all 33 default output slots twice.
			for iteration := 0; iteration < 66; iteration++ {
				if err := fixture.process(); err != nil {
					benchmark.Fatal(err)
				}
			}
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for iteration := 0; iteration < benchmark.N; iteration++ {
				if err := fixture.process(); err != nil {
					benchmark.Fatal(err)
				}
			}
			benchmark.StopTimer()
			if fixture.application.calls != benchmark.N+66 {
				benchmark.Fatalf("callback count: got %d, want %d", fixture.application.calls, benchmark.N+66)
			}
		})
	}
}
