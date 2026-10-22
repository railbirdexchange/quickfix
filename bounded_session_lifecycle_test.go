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

// OnLogout precedes writer join and inbound drain. The owner must accept a
// subsequent admin command before the disconnected fixture inspects live data.
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
	if f.session.IsConnected() || f.session.readConnection != nil || f.session.messageIn != nil || f.session.writeConnection != nil {
		return errBoundedFullFixture
	}
	if len(f.session.boundedIn.ring.vacant) != len(f.session.boundedIn.ring.slots) || len(f.session.outbound.vacant) != len(f.session.outbound.slots) {
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
	require.Nil(t, s.writeConnection)
	require.Nil(t, s.messageIn)
	assertInboundRingVacant(t, s.boundedIn.ring)
	require.Equal(t, len(s.outbound.slots), len(s.outbound.vacant))
}

// Only an explicitly armed Write is delayed. All successful writes still go
// through real TCP. Channels transfer the gate decision without a new hot lock.
type boundedLifecycleWriteGate struct {
	net.Conn
	armed, entered chan struct{}
	decision       chan error
	cancel         chan struct{}
}

func newBoundedLifecycleWriteGate(connection net.Conn) *boundedLifecycleWriteGate {
	return &boundedLifecycleWriteGate{Conn: connection, armed: make(chan struct{}, 1), entered: make(chan struct{}, 1), decision: make(chan error, 1), cancel: make(chan struct{})}
}
func (gate *boundedLifecycleWriteGate) Write(wire []byte) (int, error) {
	select {
	case <-gate.armed:
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
		case <-gate.cancel:
			return 0, io.ErrClosedPipe
		}
	default:
	}
	return gate.Conn.Write(wire)
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
	savedTime, savedBody := bytes.Clone(original), bytes.Clone(report.bodyBytes)
	// Completed delivery is not the explicit retention acknowledgement.
	require.NoError(t, f.peer.SetLinger(0))
	require.NoError(t, f.disconnect())
	assertBoundedLifecycleDisconnected(t, f.session)
	store := f.session.store.(*BoundedStore)
	retained, ok := store.message(sequence)
	require.True(t, ok)
	savedWire := bytes.Clone(retained)
	nextSender, nextTarget := store.NextSenderMsgSeqNum(), store.NextTargetMsgSeqNum()
	require.Equal(t, f.receiveSequence, nextSender)
	require.Equal(t, f.sendSequence, nextTarget)
	require.NoError(t, f.connectWithAcknowledgement(false))
	f.prepare("2", f.sendSequence)
	f.builder.Body().SetInt(tagBeginSeqNo, sequence)
	f.builder.Body().SetInt(tagEndSeqNo, sequence)
	require.NoError(t, f.writePrepared())
	f.sendSequence++
	replay, err := f.readReport()
	require.NoError(t, err)
	actual, reject := replay.Header.GetInt(tagMsgSeqNum)
	require.Nil(t, reject)
	require.Equal(t, sequence, actual)
	duplicate, reject := replay.Header.GetBytes(tagPossDupFlag)
	require.Nil(t, reject)
	require.Equal(t, "Y", string(duplicate))
	replayedTime, reject := replay.Header.GetBytes(tagOrigSendingTime)
	require.Nil(t, reject)
	require.Equal(t, savedTime, replayedTime)
	require.Equal(t, savedBody, replay.bodyBytes)
	require.NoError(t, f.disconnect())
	assertBoundedLifecycleDisconnected(t, f.session)
	retained, ok = store.message(sequence)
	require.True(t, ok)
	require.Equal(t, savedWire, retained)
	require.Equal(t, nextSender+1, store.NextSenderMsgSeqNum(), "only reconnect Logon consumes a new sequence")
	require.Equal(t, nextTarget+2, store.NextTargetMsgSeqNum())
	require.NoError(t, f.connectWithAcknowledgement(false))
	checkpoint, err := f.acknowledge(false)
	require.NoError(t, err)
	require.Zero(t, checkpoint.retained)
	require.NoError(t, f.ordinaryBatch(1, false))
}

func TestBoundedSessionLifecycleFullOutputFailureIsAtomic(t *testing.T) {
	f := newBoundedFullSession(t, true)
	require.NoError(t, f.prepareSlowConsumer())
	require.NoError(t, f.configure(func(a *boundedFullApplication) { a.session.ResetOnDisconnect = true }))
	_, blocked, err := f.blockSlowConsumer()
	require.NoError(t, err)
	f.slowGate.decision <- io.ErrClosedPipe
	var failure boundedFullAdmission
	select {
	case failure = <-f.application.admissions:
	case <-f.slowTimer.C:
		t.Fatal("blocked send did not return after writer failure")
	}
	require.ErrorIs(t, failure.err, io.ErrClosedPipe)
	require.Zero(t, failure.accepted)
	require.Equal(t, blocked.before, failure.before)
	require.Equal(t, failure.before, failure.after)
	select {
	case <-f.application.preparations:
		t.Fatal("failed blocked send reached ToApp")
	default:
	}
	require.NoError(t, f.disconnect())
	assertBoundedLifecycleDisconnected(t, f.session)
	require.ErrorIs(t, f.writer.err, io.ErrClosedPipe)
	store := f.session.store.(*BoundedStore)
	after := fingerprintBoundedFullStore(store)
	require.Equal(t, failure.before.nextSender, after.nextSender)
	require.Equal(t, failure.before.digest, after.digest, "ResetOnDisconnect must retain the failed writer's committed prefix")
	require.Equal(t, 33, after.entries)
	first, last := store.NextSenderMsgSeqNum()-33, store.NextSenderMsgSeqNum()-1
	for sequence := first; sequence <= last; sequence++ {
		wire, exists := store.message(sequence)
		require.True(t, exists)
		require.NoError(t, f.peerSlot.Parse(wire))
		message := f.peerSlot.Message()
		original, reject := message.Header.GetBytes(tagSendingTime)
		require.Nil(t, reject)
		saved := &f.savedReports[sequence-first]
		saved.sequence = sequence
		saved.timeLength = copy(saved.timestamp[:], original)
		saved.body = append(saved.body[:0], message.bodyBytes...)
	}
	// No failed-writer response reached the peer. Reconnect accepts the higher
	// Logon sequence and explicitly requests the recorded missing interval.
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
		saved := &f.savedReports[sequence-first]
		require.Equal(t, saved.timestamp[:saved.timeLength], original)
		require.Equal(t, saved.body, replay.bodyBytes)
	}
	f.application.commands <- func(a *boundedFullApplication) { a.measureAdmission = false; a.session.ResetOnDisconnect = false }
	checkpoint, err := f.acknowledge(false)
	require.NoError(t, err)
	require.Equal(t, last+2, checkpoint.nextSender)
	require.Zero(t, checkpoint.retained)
	require.NoError(t, f.ordinaryBatch(1, false))
}

func TestBoundedSessionLifecycleFullJournalRefusesReconnect(t *testing.T) {
	f := newBoundedFullSession(t, true)
	for range 32 {
		require.NoError(t, f.unacknowledgedBatch(32))
	}
	// Socket completion never evicts journal history to make reconnect room.
	require.NoError(t, f.disconnect())
	assertBoundedLifecycleDisconnected(t, f.session)
	store := f.session.store.(*BoundedStore)
	before := fingerprintBoundedFullStore(store)
	require.Equal(t, 1024, before.entries)
	require.Error(t, f.connectWithAcknowledgement(false))
	assertBoundedLifecycleDisconnected(t, f.session)
	require.Equal(t, before, fingerprintBoundedFullStore(store))
}
