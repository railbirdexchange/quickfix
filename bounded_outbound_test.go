package quickfix

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var boundedOutboundTestMessageType = []byte("D")

type boundedOutboundTestApplication struct {
	toAdmin func(*OutboundMessage) error
	toApp   func(*OutboundMessage) error
}

func (*boundedOutboundTestApplication) OnCreate(SessionID)                               {}
func (*boundedOutboundTestApplication) OnLogon(SessionID)                                {}
func (*boundedOutboundTestApplication) OnLogout(SessionID)                               {}
func (*boundedOutboundTestApplication) ToAdmin(*Message, SessionID)                      {}
func (*boundedOutboundTestApplication) ToApp(*Message, SessionID) error                  { return nil }
func (*boundedOutboundTestApplication) FromAdmin(*Message, SessionID) MessageRejectError { return nil }
func (*boundedOutboundTestApplication) FromApp(*Message, SessionID) MessageRejectError   { return nil }
func (*boundedOutboundTestApplication) FromAdminBorrowed(*MessageSlot, SessionID) MessageRejectError {
	return nil
}
func (*boundedOutboundTestApplication) FromAppBorrowed(*MessageSlot, SessionID) MessageRejectError {
	return nil
}
func (application *boundedOutboundTestApplication) ToAdminBorrowed(message *OutboundMessage, _ SessionID) error {
	if application.toAdmin != nil {
		return application.toAdmin(message)
	}
	return nil
}
func (application *boundedOutboundTestApplication) ToAppBorrowed(message *OutboundMessage, _ SessionID) error {
	if application.toApp != nil {
		return application.toApp(message)
	}
	return nil
}

type boundedOutboundTestAddress struct{}

func (boundedOutboundTestAddress) Network() string { return "fixed-test-connection" }
func (boundedOutboundTestAddress) String() string  { return "fixed-test-connection" }

// The writer owns this sink until its completed-channel send. Tests inspect or
// reuse its fixed storage only after receiving completion or joining the writer.
type boundedOutboundTestConn struct {
	wire          []byte
	used          int
	calls         int
	maxWrite      int
	writeError    error
	errorBytes    int
	deadlineError error
	zeroProgress  bool
	invalidCount  bool
	entered       chan struct{}
	release       <-chan struct{}
}

func (connection *boundedOutboundTestConn) Write(wire []byte) (int, error) {
	connection.calls++
	if connection.entered != nil {
		connection.entered <- struct{}{}
	}
	if connection.release != nil {
		<-connection.release
	}
	if connection.invalidCount {
		return len(wire) + 1, nil
	}
	if connection.zeroProgress {
		return 0, nil
	}
	count := len(wire)
	if connection.maxWrite > 0 && count > connection.maxWrite {
		count = connection.maxWrite
	}
	if connection.writeError != nil && count > connection.errorBytes {
		count = connection.errorBytes
	}
	if count > len(connection.wire)-connection.used {
		return 0, io.ErrShortBuffer
	}
	copy(connection.wire[connection.used:connection.used+count], wire[:count])
	connection.used += count
	return count, connection.writeError
}

func (*boundedOutboundTestConn) Read([]byte) (int, error)        { return 0, io.EOF }
func (*boundedOutboundTestConn) Close() error                    { return nil }
func (*boundedOutboundTestConn) LocalAddr() net.Addr             { return boundedOutboundTestAddress{} }
func (*boundedOutboundTestConn) RemoteAddr() net.Addr            { return boundedOutboundTestAddress{} }
func (*boundedOutboundTestConn) SetDeadline(time.Time) error     { return nil }
func (*boundedOutboundTestConn) SetReadDeadline(time.Time) error { return nil }
func (connection *boundedOutboundTestConn) SetWriteDeadline(time.Time) error {
	return connection.deadlineError
}

func newBoundedOutboundTestController(tb testing.TB, queue, frameBytes, builderBytes, journalCapacity, journalBytes int) (*boundedOutboundSession, *boundedOutboundTestApplication) {
	tb.Helper()
	store, err := NewBoundedStore(journalCapacity, journalBytes)
	require.NoError(tb, err)
	application := &boundedOutboundTestApplication{}
	fields := min(64, frameBytes/3)
	s := &session{
		sessionID: SessionID{BeginString: BeginStringFIX44, SenderCompID: "SENDER", TargetCompID: "TARGET"},
		store:     store, application: application, log: nullLog{},
		boundedIn: &boundedInboundSession{
			limits: ParseLimits{MaxFrameBytes: frameBytes, MaxFields: fields, MaxGroupEntries: 8, MaxGroupDepth: 4},
			log:    nullLog{},
		},
	}
	s.State = inSession{}
	s.HeartBtInt = time.Minute
	s.MaxLatency = time.Minute
	controller, err := newBoundedOutbound(s, application, store, queue, builderBytes, time.Second)
	require.NoError(tb, err)
	s.boundedOut = controller
	return controller, application
}

func startBoundedOutboundTestWriter(tb testing.TB, controller *boundedOutboundSession, connection net.Conn) *boundedWriteConnection {
	tb.Helper()
	writer := newBoundedWriteConnection(connection, controller)
	controller.writer = writer
	go writer.run()
	tb.Cleanup(func() {
		if controller.writer == writer {
			writer.finish()
			controller.writer = nil
		}
	})
	return writer
}

func receiveBoundedOutboundTestCompletion(tb testing.TB, controller *boundedOutboundSession) *outboundSlot {
	tb.Helper()
	select {
	case slot := <-controller.completed:
		return slot
	case <-time.After(5 * time.Second):
		tb.Fatal("writer did not return its output slot")
		return nil
	}
}

func completeBoundedOutboundTestWrite(tb testing.TB, controller *boundedOutboundSession) *outboundSlot {
	tb.Helper()
	slot := receiveBoundedOutboundTestCompletion(tb, controller)
	require.NoError(tb, controller.reclaim(slot))
	return slot
}

func sendBoundedOutboundTestMessage(tb testing.TB, controller *boundedOutboundSession, id string) error {
	tb.Helper()
	builder, err := controller.begin(boundedOutboundTestMessageType)
	if err != nil {
		return err
	}
	if err := builder.Body().SetString(11, id); err != nil {
		controller.abort()
		return err
	}
	return controller.sendBuilder(nil, nil, 0)
}

func assertBoundedOutboundSlotsReturned(tb testing.TB, controller *boundedOutboundSession) {
	tb.Helper()
	require.Len(tb, controller.free, cap(controller.free))
	require.Empty(tb, controller.ready)
	require.Empty(tb, controller.completed)
	require.Len(tb, controller.permits, cap(controller.permits))
	seen := make(map[*outboundSlot]bool, len(controller.free))
	for _, slot := range controller.free {
		require.NotNil(tb, slot)
		require.False(tb, seen[slot], "output slot was returned more than once")
		seen[slot] = true
		require.Empty(tb, slot.wire)
		require.Nil(tb, slot.err)
	}
}

func TestBoundedOutboundCompletedAndPartialWrites(t *testing.T) {
	for _, maximumWrite := range []int{0, 3} {
		t.Run(map[bool]string{true: "partial", false: "whole"}[maximumWrite != 0], func(t *testing.T) {
			controller, _ := newBoundedOutboundTestController(t, 1, 1024, 1024, 2, 1024)
			connection := &boundedOutboundTestConn{wire: make([]byte, 1024), maxWrite: maximumWrite}
			startBoundedOutboundTestWriter(t, controller, connection)
			require.NoError(t, sendBoundedOutboundTestMessage(t, controller, "completed"))
			slot := receiveBoundedOutboundTestCompletion(t, controller)
			require.NoError(t, slot.err)
			retained, found := controller.store.message(1)
			require.True(t, found)
			require.Equal(t, retained, connection.wire[:connection.used], "completion must cover the entire FIX frame")
			require.Equal(t, retained, slot.wire)
			if maximumWrite > 0 {
				require.Greater(t, connection.calls, 1)
			}
			require.NoError(t, controller.reclaim(slot))
			require.Equal(t, 2, controller.store.NextSenderMsgSeqNum())
			require.Equal(t, 1, controller.store.floor, "write completion must not acknowledge journal retirement")
			connection.wire[0] = 'X'
			require.Equal(t, byte('8'), retained[0], "journal must own bytes independently of writer storage")
			assertBoundedOutboundSlotsReturned(t, controller)
		})
	}
}

func TestBoundedOutboundWriteFailures(t *testing.T) {
	wantWrite, wantDeadline := errors.New("write failed"), errors.New("deadline failed")
	for _, test := range []struct {
		name       string
		connection boundedOutboundTestConn
		want       error
	}{
		{"write", boundedOutboundTestConn{writeError: wantWrite}, wantWrite},
		{"partial-error", boundedOutboundTestConn{writeError: wantWrite, errorBytes: 3}, wantWrite},
		{"zero-progress", boundedOutboundTestConn{zeroProgress: true}, io.ErrNoProgress},
		{"invalid-count", boundedOutboundTestConn{invalidCount: true}, io.ErrShortWrite},
		{"deadline", boundedOutboundTestConn{deadlineError: wantDeadline}, wantDeadline},
	} {
		t.Run(test.name, func(t *testing.T) {
			controller, _ := newBoundedOutboundTestController(t, 1, 1024, 1024, 1, 1024)
			connection := test.connection
			connection.wire = make([]byte, 1024)
			writer := startBoundedOutboundTestWriter(t, controller, &connection)
			require.NoError(t, sendBoundedOutboundTestMessage(t, controller, "failure"))
			slot := receiveBoundedOutboundTestCompletion(t, controller)
			require.ErrorIs(t, controller.reclaim(slot), test.want)
			<-writer.done
			require.Equal(t, 2, controller.store.NextSenderMsgSeqNum(), "socket failure cannot undo committed persistence")
			_, retained := controller.store.message(1)
			require.True(t, retained)
			if test.name == "deadline" {
				require.Zero(t, connection.calls)
			}
			assertBoundedOutboundSlotsReturned(t, controller)
			_, err := controller.reserveOutput()
			require.ErrorIs(t, err, ErrBoundedOutboundDisconnected)
		})
	}
}

func TestBoundedOutboundSlowSocketTimeout(t *testing.T) {
	controller, _ := newBoundedOutboundTestController(t, 1, 1024, 1024, 1, 1024)
	controller.writeTimeout = 20 * time.Millisecond
	connection, peer := net.Pipe()
	defer connection.Close()
	defer peer.Close()
	writer := startBoundedOutboundTestWriter(t, controller, connection)
	require.NoError(t, sendBoundedOutboundTestMessage(t, controller, "unread-peer"))
	slot := receiveBoundedOutboundTestCompletion(t, controller)
	err := controller.reclaim(slot)
	var timeout net.Error
	require.ErrorAs(t, err, &timeout)
	require.True(t, timeout.Timeout())
	<-writer.done
	assertBoundedOutboundSlotsReturned(t, controller)
}

func TestBoundedOutboundSlowWriterBackpressure(t *testing.T) {
	controller, _ := newBoundedOutboundTestController(t, 1, 1024, 1024, 3, 1024)
	gate := make(chan struct{})
	connection := &boundedOutboundTestConn{wire: make([]byte, 4096), entered: make(chan struct{}, 3), release: gate}
	writer := startBoundedOutboundTestWriter(t, controller, connection)
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	})
	require.NoError(t, sendBoundedOutboundTestMessage(t, controller, "writer-owned"))
	<-connection.entered
	require.NoError(t, sendBoundedOutboundTestMessage(t, controller, "queued"))
	require.Empty(t, controller.free)
	require.Len(t, controller.ready, 1)
	require.Equal(t, 3, controller.store.NextSenderMsgSeqNum())
	started, result := make(chan struct{}), make(chan error, 1)
	// Transfer the sole session/controller ownership to this goroutine until it
	// returns result. The test only controls the socket gate during that time.
	go func() {
		builder, err := controller.begin(boundedOutboundTestMessageType)
		if err == nil {
			err = builder.Body().SetString(11, "backpressured")
		}
		close(started)
		if err == nil {
			err = controller.sendBuilder(nil, nil, 0)
		}
		result <- err
	}()
	<-started
	select {
	case err := <-result:
		t.Fatalf("send returned before a write completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(gate)
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("send did not resume after socket progress")
	}
	writer.finish()
	controller.writer = nil
	require.Equal(t, 4, controller.store.NextSenderMsgSeqNum())
	require.Equal(t, 3, connection.calls)
	assertBoundedOutboundSlotsReturned(t, controller)
}

func TestBoundedOutboundQueuedFailureCleanup(t *testing.T) {
	controller, _ := newBoundedOutboundTestController(t, 2, 1024, 1024, 3, 1024)
	gate := make(chan struct{})
	writeFailure := errors.New("connection failed")
	connection := &boundedOutboundTestConn{wire: make([]byte, 4096), entered: make(chan struct{}, 1), release: gate, writeError: writeFailure}
	writer := startBoundedOutboundTestWriter(t, controller, connection)
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	})
	require.NoError(t, sendBoundedOutboundTestMessage(t, controller, "fails"))
	<-connection.entered
	require.NoError(t, sendBoundedOutboundTestMessage(t, controller, "queued-one"))
	require.NoError(t, sendBoundedOutboundTestMessage(t, controller, "queued-two"))
	require.Empty(t, controller.free)
	require.Len(t, controller.ready, 2)
	close(gate)
	<-writer.done
	writer.finish()
	controller.writer = nil
	require.Equal(t, 1, connection.calls, "failed writer must not write queued frames")
	require.Equal(t, 4, controller.store.NextSenderMsgSeqNum())
	for sequence := 1; sequence <= 3; sequence++ {
		_, found := controller.store.message(sequence)
		require.True(t, found, "queued committed data must remain recoverable")
	}
	assertBoundedOutboundSlotsReturned(t, controller)
}

func TestBoundedOutboundCapacityBeforeSequenceAndJournal(t *testing.T) {
	for _, test := range []struct {
		name                    string
		frame, builder, journal int
		fill                    bool
		want                    error
	}{
		{"builder", 1024, 8, 1024, false, ErrOutboundBuilderCapacity},
		{"frame", 64, 1024, 1024, false, ErrBufferTooSmall},
		{"journal-message", 1024, 1024, 16, false, ErrJournalMessageCapacity},
		{"journal-full", 1024, 1024, 1024, true, ErrJournalFull},
	} {
		t.Run(test.name, func(t *testing.T) {
			controller, _ := newBoundedOutboundTestController(t, 1, test.frame, test.builder, 1, test.journal)
			connection := &boundedOutboundTestConn{wire: make([]byte, 1024)}
			startBoundedOutboundTestWriter(t, controller, connection)
			if test.fill {
				require.NoError(t, controller.store.SaveMessageAndIncrNextSenderMsgSeqNum(1, []byte("existing")))
			}
			before := snapshotBoundedStore(controller.store)
			err := sendBoundedOutboundTestMessage(t, controller, "capacity-must-not-commit")
			require.ErrorIs(t, err, test.want)
			require.Equal(t, before, snapshotBoundedStore(controller.store))
			require.Equal(t, -1, controller.store.reservedIndex)
			require.Zero(t, connection.calls)
			assertBoundedOutboundSlotsReturned(t, controller)
		})
	}
}

// This peer receives bytes copied by the writer into a fixed in-process sink.
// It parses and acknowledges each complete FIX frame before the session retires
// that sequence. Write completion alone never advances the recovery floor.
func boundedOutboundOwnershipCycle(controller *boundedOutboundSession, connection *boundedOutboundTestConn, peer *MessageSlot) error {
	sequence := controller.store.NextSenderMsgSeqNum()
	builder, err := controller.begin(boundedOutboundTestMessageType)
	if err != nil {
		return err
	}
	if err := builder.Body().SetString(11, "ownership"); err != nil {
		return err
	}
	if err := controller.sendBuilder(nil, nil, 0); err != nil {
		return err
	}
	if err := controller.reclaim(<-controller.completed); err != nil {
		return err
	}
	if err := peer.Parse(connection.wire[:connection.used]); err != nil {
		return err
	}
	received, reject := peer.Message().Header.GetInt(tagMsgSeqNum)
	if reject != nil {
		return reject
	}
	if received != sequence {
		return ErrInvalidJournalSequence
	}
	peer.Reset()
	connection.used = 0
	return controller.store.RetireBefore(received + 1)
}

func TestBoundedOutboundOwnershipZeroAllocations(t *testing.T) {
	controller, _ := newBoundedOutboundTestController(t, 2, 1024, 1024, 2, 1024)
	connection := &boundedOutboundTestConn{wire: make([]byte, 1024)}
	startBoundedOutboundTestWriter(t, controller, connection)
	peer, err := NewMessageSlot(nil, nil, controller.session.boundedIn.limits)
	require.NoError(t, err)
	require.NoError(t, boundedOutboundOwnershipCycle(controller, connection, peer))
	allocations := testing.AllocsPerRun(1000, func() {
		if err := boundedOutboundOwnershipCycle(controller, connection, peer); err != nil {
			panic(err)
		}
	})
	require.Zero(t, allocations)
	assertBoundedOutboundSlotsReturned(t, controller)
}

// BenchmarkBoundedOutboundOwnership measures completed-write ownership with a
// fixed in-process sink and explicit peer acknowledgement at production-default
// capacities. It is a component benchmark, not real-network/full-session proof.
func BenchmarkBoundedOutboundOwnership(b *testing.B) {
	b.StopTimer()
	limits := DefaultParseLimits()
	store, err := NewBoundedStore(1024, 16384)
	require.NoError(b, err)
	application := &boundedOutboundTestApplication{}
	s := &session{
		sessionID: SessionID{BeginString: BeginStringFIX44, SenderCompID: "SENDER", TargetCompID: "TARGET"},
		store:     store, application: application, log: nullLog{},
		boundedIn: &boundedInboundSession{limits: limits, log: nullLog{}},
	}
	s.State = inSession{}
	s.HeartBtInt = time.Minute
	s.MaxLatency = time.Minute
	controller, err := newBoundedOutbound(s, application, store, 32, 32768, 5*time.Second)
	require.NoError(b, err)
	s.boundedOut = controller
	connection := &boundedOutboundTestConn{wire: make([]byte, limits.MaxFrameBytes)}
	startBoundedOutboundTestWriter(b, controller, connection)
	peer, err := NewMessageSlot(nil, nil, limits)
	require.NoError(b, err)
	// Each cycle returns all output ownership. Rotate that owned free stack
	// between warm-up cycles so its LIFO policy visits every one of 33 slots.
	for range len(controller.free) {
		require.NoError(b, boundedOutboundOwnershipCycle(controller, connection, peer))
		last := len(controller.free) - 1
		warmed := controller.free[last]
		copy(controller.free[1:], controller.free[:last])
		controller.free[0] = warmed
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.StartTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if err := boundedOutboundOwnershipCycle(controller, connection, peer); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
}
