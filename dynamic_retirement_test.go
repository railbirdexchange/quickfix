package quickfix

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/config"
	"github.com/quickfixgo/quickfix/internal"
	"github.com/stretchr/testify/require"
)

type retirementTestFactory struct {
	mu                           sync.Mutex
	created, closed              int
	createStarted, createRelease chan struct{}
	closeStarted, closeRelease   chan struct{}
	closeErr                     error
	targetStarted, targetRelease chan struct{}
	onClose                      func()
}

type retirementTestStore struct {
	MessageStore
	factory *retirementTestFactory
}

func (f *retirementTestFactory) Create(id SessionID) (MessageStore, error) {
	f.mu.Lock()
	f.created++
	f.mu.Unlock()
	if f.createStarted != nil {
		f.createStarted <- struct{}{}
	}
	if f.createRelease != nil {
		<-f.createRelease
	}
	s, err := NewMemoryStoreFactory().Create(id)
	return &retirementTestStore{MessageStore: s, factory: f}, err
}

func (s *retirementTestStore) Close() error {
	f := s.factory
	if f.onClose != nil {
		f.onClose()
	}
	if f.closeStarted != nil {
		f.closeStarted <- struct{}{}
	}
	if f.closeRelease != nil {
		<-f.closeRelease
	}
	f.mu.Lock()
	f.closed++
	f.mu.Unlock()
	return f.closeErr
}

func (s *retirementTestStore) SetNextTargetMsgSeqNum(next int) error {
	if s.factory.targetStarted != nil {
		s.factory.targetStarted <- struct{}{}
	}
	if s.factory.targetRelease != nil {
		<-s.factory.targetRelease
	}
	return s.MessageStore.SetNextTargetMsgSeqNum(next)
}

func (f *retirementTestFactory) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created, f.closed
}

func retirementTestID(t *testing.T) SessionID {
	id := SessionID{BeginString: BeginStringFIX44, SenderCompID: t.Name(), TargetCompID: "PEER"}
	t.Cleanup(func() {
		_ = UnregisterSession(id)
		sessionsLock.Lock()
		delete(sessionReservations, id) // A quarantined test identity cannot survive its fixture.
		sessionsLock.Unlock()
	})
	return id
}

func newRetirementTestAcceptor(t *testing.T, factory MessageStoreFactory) *Acceptor {
	log, err := NewNullLogFactory().Create()
	require.NoError(t, err)
	a := &Acceptor{
		settings: NewSettings(), app: retirementBenchmarkApplication{}, storeFactory: factory,
		logFactory: NewNullLogFactory(), globalLog: log, dynamicSessions: true,
		dynamicSessionChan: make(chan *session), dynamicSessionStop: make(chan struct{}),
	}
	a.sessionGroup.Add(1)
	go func() { defer a.sessionGroup.Done(); a.dynamicSessionsLoop() }()
	t.Cleanup(a.Stop)
	return a
}

func waitRetirementSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("lifecycle operation did not complete")
	}
}

// These owner tests deliberately have no socket. Complete the connection
// setup ownership explicitly after handoff to the real dynamic run loop.
func startRetirementTestSession(a *Acceptor, id SessionID) (*session, error) {
	s, err := a.startDynamicSession(id)
	if err == nil {
		a.dynamicSetupGroup.Done()
	}
	return s, err
}

func TestDynamicRetirementEarlyUnregisterKeepsIdentityUntilStoreClose(t *testing.T) {
	id := retirementTestID(t)
	f := &retirementTestFactory{closeStarted: make(chan struct{}, 1), closeRelease: make(chan struct{})}
	a := newRetirementTestAcceptor(t, f)
	s, err := startRetirementTestSession(a, id)
	require.NoError(t, err)
	require.NoError(t, UnregisterSession(id))
	s.stop()
	waitRetirementSignal(t, f.closeStarted)
	_, err = startRetirementTestSession(a, id)
	require.ErrorIs(t, err, errDuplicateSessionID)
	created, closed := f.counts()
	require.Equal(t, 1, created)
	require.Zero(t, closed)
	close(f.closeRelease)
	a.Stop()
	_, closed = f.counts()
	require.Equal(t, 1, closed)
	replacement, err := (sessionFactory{}).createSession(id, NewMemoryStoreFactory(), NewSessionSettings(), NewNullLogFactory(), retirementBenchmarkApplication{})
	require.NoError(t, err)
	require.NoError(t, replacement.closeStore())
}

func TestDynamicRetirementRejectsDelayedSendAndReset(t *testing.T) {
	id := retirementTestID(t)
	f := &retirementTestFactory{}
	a := newRetirementTestAcceptor(t, f)
	s, err := startRetirementTestSession(a, id)
	require.NoError(t, err)
	// This is the pointer obtained by a sender before it was descheduled.
	delayed, ok := lookupSession(id)
	require.True(t, ok)
	s.stop()
	a.Stop()
	message := NewMessage()
	message.Header.SetString(tagMsgType, "0")
	require.ErrorIs(t, delayed.queueForSend(message), errUnknownSession)
	require.ErrorIs(t, delayed.dropAndReset(), errUnknownSession)
	require.Equal(t, 1, s.store.NextSenderMsgSeqNum())
	require.Equal(t, 1, s.store.NextTargetMsgSeqNum())
	require.False(t, message.Header.Has(tagMsgSeqNum))
}

func TestDynamicRetirementCloseFailureQuarantinesIdentityButCompletes(t *testing.T) {
	id := retirementTestID(t)
	f := &retirementTestFactory{closeErr: errors.New("writer has not joined")}
	a := newRetirementTestAcceptor(t, f)
	s, err := startRetirementTestSession(a, id)
	require.NoError(t, err)
	s.stop()
	done := make(chan struct{})
	go func() { a.Stop(); close(done) }()
	waitRetirementSignal(t, done)
	_, ok := lookupSession(id)
	require.False(t, ok)
	_, err = (sessionFactory{}).createSession(id, f, NewSessionSettings(), NewNullLogFactory(), retirementBenchmarkApplication{})
	require.ErrorIs(t, err, errDuplicateSessionID)
	created, closed := f.counts()
	require.Equal(t, 1, created)
	require.Equal(t, 1, closed)
}

func TestDynamicRetirementShutdownDuringStoreConstruction(t *testing.T) {
	id := retirementTestID(t)
	f := &retirementTestFactory{createStarted: make(chan struct{}, 1), createRelease: make(chan struct{})}
	a := newRetirementTestAcceptor(t, f)
	constructed := make(chan struct{})
	go func() { _, _ = startRetirementTestSession(a, id); close(constructed) }()
	waitRetirementSignal(t, f.createStarted)
	_, err := (sessionFactory{}).createSession(id, f, NewSessionSettings(), NewNullLogFactory(), retirementBenchmarkApplication{})
	require.ErrorIs(t, err, errDuplicateSessionID)
	stopped := make(chan struct{})
	go func() { a.Stop(); close(stopped) }()
	waitRetirementSignal(t, a.dynamicSessionStop)
	close(f.createRelease)
	waitRetirementSignal(t, constructed)
	waitRetirementSignal(t, stopped)
	created, closed := f.counts()
	require.Equal(t, 1, created)
	require.Equal(t, 1, closed)
	_, ok := lookupSession(id)
	require.False(t, ok)
	sessionsLock.RLock()
	_, reserved := sessionReservations[id]
	sessionsLock.RUnlock()
	require.False(t, reserved)
}

func TestDynamicRetirementShutdownWaitsForAcknowledgedConnection(t *testing.T) {
	id := retirementTestID(t)
	f := &retirementTestFactory{}
	a := newRetirementTestAcceptor(t, f)
	// Deliberately pause the connection handler immediately after handoff.
	s, err := a.startDynamicSession(id)
	require.NoError(t, err)
	stopped := make(chan struct{})
	go func() { a.Stop(); close(stopped) }()
	waitRetirementSignal(t, a.dynamicSessionStop)
	_, closed := f.counts()
	require.Zero(t, closed)
	_, err = (sessionFactory{}).createSession(id, f, NewSessionSettings(), NewNullLogFactory(), retirementBenchmarkApplication{})
	require.ErrorIs(t, err, errDuplicateSessionID)
	a.sessionAddr.Store(id, &sessionAddress{owner: s, address: &net.TCPAddr{Port: 1234}})
	connected := make(chan struct{})
	go func() {
		_ = s.connectWithWriter(nil, nil, nil)
		a.dynamicSetupGroup.Done()
		close(connected)
	}()
	waitRetirementSignal(t, connected)
	waitRetirementSignal(t, stopped)
	_, closed = f.counts()
	require.Equal(t, 1, closed)
	_, exists := a.RemoteAddr(id)
	require.False(t, exists)
}

func TestDynamicRetirementWaitsForCompletedSocketWrite(t *testing.T) {
	id := retirementTestID(t)
	f := &retirementTestFactory{closeStarted: make(chan struct{}, 1)}
	a := newRetirementTestAcceptor(t, f)
	s, err := a.startDynamicSession(id)
	require.NoError(t, err)
	connection := &scopedBackpressureConnection{
		scopedOutboundBenchmarkConnection: scopedOutboundBenchmarkConnection{wire: make([]byte, 64)},
		entered:                           make(chan struct{}), release: make(chan struct{}),
	}
	t.Cleanup(connection.unblock)
	readServer, readPeer := net.Pipe()
	t.Cleanup(func() { _ = readServer.Close(); _ = readPeer.Close() })
	inbound := make(chan fixIn, 1)
	reader, err := startBoundedRead(readServer, newParser(readServer), s.boundedIn, inbound, nil, time.Now())
	require.NoError(t, err)
	writer := newOutboundWriter(connection, 1, time.Second, s.messageEvent)
	var readerDoneAtClose, writerDoneAtClose bool
	f.onClose = func() {
		select {
		case <-reader.done:
			readerDoneAtClose = true
		default:
		}
		select {
		case <-writer.done:
			writerDoneAtClose = true
		default:
		}
	}
	go writer.run()
	require.NoError(t, s.connectWithWriter(inbound, reader, writer))
	a.dynamicSetupGroup.Done()
	slot, err := s.outbound.reserve(false)
	require.NoError(t, err)
	slot.wire = append(slot.wire, "owned wire"...)
	<-writer.permits
	writer.ready <- slot
	waitRetirementSignal(t, connection.entered)
	s.stop()
	select {
	case <-f.closeStarted:
		t.Fatal("store closed while socket still owned the frame")
	case <-time.After(25 * time.Millisecond):
	}
	connection.unblock()
	waitRetirementSignal(t, f.closeStarted)
	a.Stop()
	require.True(t, readerDoneAtClose, "store Close preceded reader completion")
	require.True(t, writerDoneAtClose, "store Close preceded writer completion")
	require.Equal(t, "owned wire", string(connection.wire[:connection.used]))
	require.Len(t, s.outbound.vacant, len(s.outbound.slots))
	require.Len(t, s.boundedIn.ring.vacant, len(s.boundedIn.ring.slots))
	_, closed := f.counts()
	require.Equal(t, 1, closed)
}

func TestDynamicRetirementStopsTimeoutsAndReleasesFiringCallback(t *testing.T) {
	id := retirementTestID(t)
	a := newRetirementTestAcceptor(t, &retirementTestFactory{})
	s, err := a.sessionFactory.createOwnedSession(id, a.storeFactory, NewSessionSettings(), a.logFactory, a.app, true)
	require.NoError(t, err)
	s.logonTimeoutTimer = time.AfterFunc(time.Hour, func() {})
	s.logoutTimeoutTimer = time.AfterFunc(time.Hour, func() {})
	t.Cleanup(func() { s.logonTimeoutTimer.Stop(); s.logoutTimeoutTimer.Stop() })
	a.dynamicSessionChan <- s
	s.stop()
	a.Stop()
	require.False(t, s.logonTimeoutTimer.Stop(), "run must already have stopped the logon timer")
	require.False(t, s.logoutTimeoutTimer.Stop(), "run must already have stopped the logout timer")
	// Force a callback that has already fired to attempt delivery after its
	// run has retired. It must exit even though there is no event receiver.
	fired, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	timer := time.AfterFunc(0, func() {
		close(fired)
		<-release
		deliverSessionTimeout(s.sessionEvent, s.runStop, internal.LogoutTimeout)
		close(returned)
	})
	defer timer.Stop()
	waitRetirementSignal(t, fired)
	close(release)
	waitRetirementSignal(t, returned)
}

func TestDynamicRetirementWaitsForTargetSequenceMutation(t *testing.T) {
	id := retirementTestID(t)
	f := &retirementTestFactory{targetStarted: make(chan struct{}, 1), targetRelease: make(chan struct{}), closeStarted: make(chan struct{}, 1)}
	a := newRetirementTestAcceptor(t, f)
	s, err := startRetirementTestSession(a, id)
	require.NoError(t, err)
	updated := make(chan struct{})
	go func() { _ = SetNextTargetMsgSeqNum(id, 7); close(updated) }()
	waitRetirementSignal(t, f.targetStarted)
	s.stop()
	select {
	case <-f.closeStarted:
		t.Fatal("store closed while target mutation still owned it")
	case <-time.After(25 * time.Millisecond):
	}
	close(f.targetRelease)
	waitRetirementSignal(t, updated)
	a.Stop()
	require.Equal(t, 7, s.store.NextTargetMsgSeqNum())
	require.ErrorIs(t, SetNextTargetMsgSeqNum(id, 8), errUnknownSession)
}

func TestDynamicRetirementDoesNotDeleteReplacementAddress(t *testing.T) {
	id := retirementTestID(t)
	old, replacement := &session{sessionID: id}, &session{sessionID: id}
	a := &Acceptor{}
	address := &net.TCPAddr{Port: 1234}
	a.sessionAddr.Store(id, &sessionAddress{owner: replacement, address: address})
	a.removeSessionAddress(old)
	got, ok := a.RemoteAddr(id)
	require.True(t, ok)
	require.Equal(t, address, got)
	a.removeSessionAddress(replacement)
	_, ok = a.RemoteAddr(id)
	require.False(t, ok)
}

func TestSessionConstructionFailureHoldsReservationThroughClose(t *testing.T) {
	for _, failedClose := range []bool{false, true} {
		t.Run(map[bool]string{false: "closed", true: "quarantined"}[failedClose], func(t *testing.T) {
			id := retirementTestID(t)
			f := &retirementTestFactory{closeStarted: make(chan struct{}, 1), closeRelease: make(chan struct{})}
			if failedClose {
				f.closeErr = errors.New("incomplete close")
			}
			settings := NewSessionSettings()
			settings.Set(config.OutboundFrameQueueCapacity, "0")
			done := make(chan struct{})
			go func() {
				_, _ = (sessionFactory{}).createSession(id, f, settings, NewNullLogFactory(), retirementBenchmarkApplication{})
				close(done)
			}()
			waitRetirementSignal(t, f.closeStarted)
			_, err := (sessionFactory{}).createSession(id, f, NewSessionSettings(), NewNullLogFactory(), retirementBenchmarkApplication{})
			require.ErrorIs(t, err, errDuplicateSessionID)
			close(f.closeRelease)
			waitRetirementSignal(t, done)
			created, closed := f.counts()
			require.Equal(t, 1, created)
			require.Equal(t, 1, closed)
			sessionsLock.RLock()
			_, reserved := sessionReservations[id]
			sessionsLock.RUnlock()
			require.Equal(t, failedClose, reserved)
		})
	}
}
