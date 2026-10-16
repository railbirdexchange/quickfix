package quickfix

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Lifecycle tests acknowledge an admin request only after the session owner has
// finished its current teardown. OnLogout alone is too early: it precedes the
// writer join, inbound drain, and slot return.
func boundedLifecycleOwnerBarrier(t *testing.T, s *session) {
	t.Helper()
	require.NoError(t, boundedLifecycleBarrier(s))
}

func boundedLifecycleBarrier(s *session) error {
	reply := make(chan waitChan)
	select {
	case s.admin <- waitForInSessionReq{rep: reply}:
	case <-time.After(10 * time.Second):
		return errors.New("session owner did not accept teardown barrier")
	}
	select {
	case _, waiting := <-reply:
		if waiting {
			return errors.New("lifecycle fixture unexpectedly left session time")
		}
	case <-time.After(10 * time.Second):
		return errors.New("session owner did not complete teardown barrier")
	}
	return nil
}

// disconnect joins the actual connection's teardown without stopping the
// session owner. The same session, journal, and rings can then reconnect.
func (f *boundedFullSession) disconnect() error {
	if f.peer != nil {
		_ = f.peer.Close()
	}
	select {
	case <-f.application.logouts:
	case <-time.After(10 * time.Second):
		return errors.New("full session did not observe disconnect")
	}
	if err := boundedLifecycleBarrier(f.session); err != nil {
		return err
	}
	if f.session.IsConnected() || f.session.readConnection != nil || f.session.messageIn != nil || f.session.boundedOut.writer != nil {
		return errBoundedFullFixture
	}
	if len(f.session.boundedIn.ring.vacant) != len(f.session.boundedIn.ring.slots) || len(f.session.boundedOut.free) != cap(f.session.boundedOut.free) {
		return errBoundedFullFixture
	}
	return nil
}

func (f *boundedFullSession) reconnect() error {
	if err := f.disconnect(); err != nil {
		return err
	}
	return f.connect()
}

func assertBoundedLifecycleDisconnected(t *testing.T, s *session) {
	t.Helper()
	boundedLifecycleOwnerBarrier(t, s)
	require.False(t, s.IsConnected())
	require.Nil(t, s.readConnection)
	require.Nil(t, s.boundedOut.writer)
	require.Nil(t, s.messageIn)
	assertInboundRingVacant(t, s.boundedIn.ring)
	assertBoundedOutboundSlotsReturned(t, s.boundedOut)
}

// A real TCP connection remains underneath this deterministic failure gate.
// The owner installs it after a completed-write barrier. Only the first new
// Write is stalled, so the test can fill the configured output queue exactly
// before allowing that Write to fail. Kernel backpressure is covered separately
// by the full-session slow-consumer scenario.
type boundedLifecycleWriteGate struct {
	net.Conn
	entered  chan struct{}
	decision chan error
	cancel   chan struct{}
}

func (gate *boundedLifecycleWriteGate) Write(wire []byte) (int, error) {
	select {
	case gate.entered <- struct{}{}:
	case <-gate.cancel:
		return 0, io.ErrClosedPipe
	}
	select {
	case err := <-gate.decision:
		if err != nil {
			return 0, err
		}
		return gate.Conn.Write(wire)
	case <-gate.cancel:
		return 0, io.ErrClosedPipe
	}
}

type boundedLifecycleCapacityLog struct {
	nullLog
	full chan struct{}
}

func (log *boundedLifecycleCapacityLog) OnEvent(event string) {
	if event == ErrJournalFull.Error() {
		select {
		case log.full <- struct{}{}:
		default:
		}
	}
}

type boundedLifecycleFailure struct {
	before, after boundedStoreSnapshot
	err           error
}

func TestBoundedSessionLifecycleRetainedJournalAfterTCPReset(t *testing.T) {
	f := newBoundedFullSession(t, true)
	require.NoError(t, f.sendOrder(f.sendSequence))
	report, err := f.readReport()
	require.NoError(t, err)
	sequence, reject := report.Header.GetInt(tagMsgSeqNum)
	require.Nil(t, reject)
	original, reject := report.Header.GetBytes(tagSendingTime)
	require.Nil(t, reject)
	savedTime := append([]byte(nil), original...)
	savedBody := append([]byte(nil), report.bodyBytes...)

	// The peer received this response but has not acknowledged its recovery
	// floor. A TCP reset must leave the committed journal available to resend.
	require.NoError(t, f.peer.SetLinger(0))
	require.NoError(t, f.disconnect())
	assertBoundedLifecycleDisconnected(t, f.session)
	store := f.session.boundedOut.store
	retained, ok := store.message(sequence)
	require.True(t, ok)
	savedWire := append([]byte(nil), retained...)
	nextSender, nextTarget := store.NextSenderMsgSeqNum(), store.NextTargetMsgSeqNum()
	require.Equal(t, f.receiveSequence, nextSender)
	require.Equal(t, f.sendSequence, nextTarget)

	// Leave room for the new Logon and explicitly defer retirement until the
	// peer has checked the replay from the previous connection.
	require.NoError(t, f.connectWithAcknowledgement(false))
	f.prepare("2", f.sendSequence)
	f.builder.Body().SetInt(tagBeginSeqNo, sequence)
	f.builder.Body().SetInt(tagEndSeqNo, sequence)
	require.NoError(t, f.writePrepared())
	f.sendSequence++
	replay, err := f.readReport()
	require.NoError(t, err)
	replayedSequence, reject := replay.Header.GetInt(tagMsgSeqNum)
	require.Nil(t, reject)
	require.Equal(t, sequence, replayedSequence)
	duplicate, reject := replay.Header.GetBytes(tagPossDupFlag)
	require.Nil(t, reject)
	require.Equal(t, "Y", string(duplicate))
	replayedTime, reject := replay.Header.GetBytes(tagOrigSendingTime)
	require.Nil(t, reject)
	require.Equal(t, savedTime, replayedTime)
	require.True(t, bytes.Equal(savedBody, replay.bodyBytes))
	// The borrowed replay buffer must not modify independently retained bytes.
	// Teardown transfers journal ownership back to this test for inspection.
	require.NoError(t, f.disconnect())
	assertBoundedLifecycleDisconnected(t, f.session)
	retained, ok = store.message(sequence)
	require.True(t, ok)
	require.Equal(t, savedWire, retained)
	require.Equal(t, nextSender+1, store.NextSenderMsgSeqNum(), "replay must not allocate a new sequence")
	require.Equal(t, nextTarget+2, store.NextTargetMsgSeqNum(), "reconnect Logon and ResendRequest are processed")
	require.NoError(t, f.connectWithAcknowledgement(false))
	checkpoint, err := f.acknowledge(false)
	require.NoError(t, err)
	require.Zero(t, checkpoint.retained, "explicit peer acknowledgement retires the replayed history")
	require.NoError(t, f.ordinaryBatch(1, false), "ordinary traffic resumes on the same session and rings")
}

func TestBoundedSessionLifecycleFullOutputFailureIsAtomic(t *testing.T) {
	f := newBoundedFullSession(t, true)
	gate := &boundedLifecycleWriteGate{Conn: f.server, entered: make(chan struct{}, 1), decision: make(chan error, 1), cancel: make(chan struct{})}
	t.Cleanup(func() { close(gate.cancel) })
	filled := make(chan struct{}, 1)
	result := make(chan boundedLifecycleFailure, 1)
	require.NoError(t, f.configure(func(a *boundedFullApplication) {
		a.session.ResetOnDisconnect = true
		a.session.boundedOut.writer.connection = gate
		a.onOrder = func(slot *MessageSlot) MessageRejectError {
			out := a.session.boundedOut
			for range cap(out.free) {
				if reject := a.handleOrder(slot); reject != nil {
					return reject
				}
			}
			before := snapshotBoundedStore(out.store)
			filled <- struct{}{}
			reject := a.handleOrder(slot)
			var err error
			if failure, ok := reject.(*boundedOutboundReject); ok {
				err = failure.err
			} else if reject != nil {
				err = reject
			}
			result <- boundedLifecycleFailure{before: before, after: snapshotBoundedStore(out.store), err: err}
			return reject
		}
	}))
	require.NoError(t, f.sendOrder(f.sendSequence))
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("writer never entered the deterministic TCP gate")
	}
	select {
	case <-filled:
	case <-time.After(10 * time.Second):
		t.Fatal("session did not fill writer plus all queued output slots")
	}
	select {
	case failure := <-result:
		t.Fatalf("send returned before any output slot completed: %v", failure.err)
	case <-time.After(20 * time.Millisecond):
	}
	gate.decision <- io.ErrClosedPipe
	var failure boundedLifecycleFailure
	select {
	case failure = <-result:
	case <-time.After(10 * time.Second):
		t.Fatal("blocked send did not observe writer failure")
	}
	require.True(t, errors.Is(failure.err, io.ErrClosedPipe) || errors.Is(failure.err, ErrBoundedOutboundDisconnected), "unexpected send failure: %v", failure.err)
	require.Equal(t, failure.before, failure.after, "the blocked send must not assign a sequence or change retained bytes")
	require.NoError(t, f.disconnect())
	assertBoundedLifecycleDisconnected(t, f.session)
	require.Equal(t, failure.before, snapshotBoundedStore(f.session.boundedOut.store), "ResetOnDisconnect must not erase a failed writer's committed prefix")
	require.ErrorIs(t, f.writer.result, io.ErrClosedPipe)

	store := f.session.boundedOut.store
	first := store.NextSenderMsgSeqNum() - cap(f.session.boundedOut.free)
	last := store.NextSenderMsgSeqNum() - 1
	retained := make([]struct{ body, original []byte }, last-first+1)
	for sequence := first; sequence <= last; sequence++ {
		wire, exists := store.message(sequence)
		require.True(t, exists)
		require.NoError(t, f.peerSlot.Parse(wire))
		message := f.peerSlot.Message()
		original, reject := message.Header.GetBytes(tagSendingTime)
		require.Nil(t, reject)
		retained[sequence-first].body = append([]byte(nil), message.bodyBytes...)
		retained[sequence-first].original = append([]byte(nil), original...)
	}
	// No response from the failed writer reached the peer. The test peer
	// records that missing range, accepts the higher reconnect Logon, and asks
	// explicitly for that range before acknowledging any new recovery floor.
	f.receiveSequence = store.NextSenderMsgSeqNum()
	f.sendSequence = store.NextTargetMsgSeqNum()
	require.NoError(t, f.connectWithAcknowledgement(false))
	f.prepare("2", f.sendSequence)
	f.builder.Body().SetInt(tagBeginSeqNo, first)
	f.builder.Body().SetInt(tagEndSeqNo, last)
	require.NoError(t, f.writePrepared())
	f.sendSequence++
	for sequence := first; sequence <= last; sequence++ {
		replay, err := f.readReport()
		require.NoError(t, err)
		actual, reject := replay.Header.GetInt(tagMsgSeqNum)
		require.Nil(t, reject)
		require.Equal(t, sequence, actual)
		duplicate, reject := replay.Header.GetBytes(tagPossDupFlag)
		require.Nil(t, reject)
		require.Equal(t, "Y", string(duplicate))
		original, reject := replay.Header.GetBytes(tagOrigSendingTime)
		require.Nil(t, reject)
		require.Equal(t, retained[sequence-first].original, original)
		require.Equal(t, retained[sequence-first].body, replay.bodyBytes)
	}
	f.application.commands <- func(a *boundedFullApplication) {
		a.onOrder = nil
		a.session.ResetOnDisconnect = false
	}
	checkpoint, err := f.acknowledge(false)
	require.NoError(t, err)
	require.Equal(t, last+2, checkpoint.nextSender, "only reconnect Logon consumes a new sender sequence")
	require.Zero(t, checkpoint.retained)
	require.NoError(t, f.ordinaryBatch(1, false))
}

func TestBoundedSessionLifecycleFullJournalRefusesReconnect(t *testing.T) {
	f := newBoundedFullSession(t, true)
	log := &boundedLifecycleCapacityLog{full: make(chan struct{}, 1)}
	require.NoError(t, f.configure(func(a *boundedFullApplication) {
		a.session.log = log
		a.onOrder = func(slot *MessageSlot) MessageRejectError {
			for range len(a.session.boundedOut.store.entries) {
				if reject := a.handleOrder(slot); reject != nil {
					return reject
				}
			}
			return nil
		}
	}))
	require.NoError(t, f.sendOrder(f.sendSequence))
	for range 1024 {
		_, err := f.readReport()
		require.NoError(t, err)
	}
	// A completed write is not a recovery acknowledgement. None of these
	// entries may be implicitly evicted to make room for a reconnect Logon.
	require.NoError(t, f.disconnect())
	assertBoundedLifecycleDisconnected(t, f.session)
	store := f.session.boundedOut.store
	before := snapshotBoundedStore(store)
	retained := 0
	for _, entry := range store.entries {
		if entry.sequence != 0 {
			retained++
		}
	}
	require.Equal(t, 1024, retained)
	require.Error(t, f.connectWithAcknowledgement(false))
	select {
	case <-log.full:
	case <-time.After(10 * time.Second):
		t.Fatal("reconnect did not report journal capacity exhaustion")
	}
	// Failed acceptor Logon has no OnLogout callback. The owner barrier still
	// orders all teardown and returns every input and output slot.
	assertBoundedLifecycleDisconnected(t, f.session)
	require.Equal(t, before, snapshotBoundedStore(store))
}
