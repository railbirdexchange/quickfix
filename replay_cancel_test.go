package quickfix

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type blockingReplayStore struct {
	MessageStore
	entered chan struct{}
	release chan struct{}
	exited  chan struct{}
	once    sync.Once
}

func (s *blockingReplayStore) IterateMessages(begin, end int, cb func([]byte) error) error {
	return s.IterateMessagesContext(context.Background(), begin, end, cb)
}
func (s *blockingReplayStore) IterateMessagesContext(ctx context.Context, begin, end int, cb func([]byte) error) error {
	s.once.Do(func() { close(s.entered) })
	defer close(s.exited)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
		return s.MessageStore.IterateMessages(begin, end, cb)
	}
}

type replayStoreFactory struct{ store MessageStore }

func (f replayStoreFactory) Create(SessionID) (MessageStore, error) { return f.store, nil }

type replayTestApp struct {
	starvationApp
	logout         chan struct{}
	replayReady    chan struct{}
	logoutReady    chan struct{}
	rejectedLogout chan struct{}
	resendAccepted chan struct{}
	requestGate    chan struct{}
}

func (a *replayTestApp) OnLogout(SessionID) {
	select {
	case a.logout <- struct{}{}:
	default:
	}
}

func replayWireMessage(kind string, seq int) *Message {
	m := NewMessage()
	m.Header.SetString(tagBeginString, "FIX.4.4")
	m.Header.SetString(tagSenderCompID, "DEALER")
	m.Header.SetString(tagTargetCompID, "VENUE")
	m.Header.SetString(tagMsgType, kind)
	m.Header.SetInt(tagMsgSeqNum, seq)
	m.Header.SetField(tagSendingTime, FIXUTCTimestamp{Time: time.Now()})
	return m
}
func TestReplayStopsWhileStorageReadIsBlocked(t *testing.T) {
	for _, reason := range []string{"peer_logout", "disconnect", "local_logout"} {
		t.Run(reason, func(t *testing.T) {
			memory, err := NewMemoryStoreFactory().Create(SessionID{})
			require.NoError(t, err)
			store := &blockingReplayStore{MessageStore: memory, entered: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
			app := &replayTestApp{starvationApp: starvationApp{loggedOn: make(chan SessionID, 1)}, logout: make(chan struct{}, 2)}
			port := freeTCPPort(t)
			acceptor, err := NewAcceptor(app, replayStoreFactory{store}, starvationSettings(t, port, "VENUE", "DEALER", "acceptor"), NewNullLogFactory())
			require.NoError(t, err)
			require.NoError(t, acceptor.Start())
			conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			require.NoError(t, err)
			t.Cleanup(func() { close(store.release); _ = conn.Close(); acceptor.Stop() })
			require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
			parser := newParser(bufio.NewReader(conn))
			logon := replayWireMessage("A", 1)
			logon.Body.SetInt(tagEncryptMethod, 0)
			logon.Body.SetInt(tagHeartBtInt, 30)
			_, err = conn.Write(logon.build())
			require.NoError(t, err)
			_, err = parser.ReadMessage()
			require.NoError(t, err)
			id := <-app.loggedOn
			require.NoError(t, SendToTarget(starvationMessage(time.Now()), id))
			_, err = parser.ReadMessage()
			require.NoError(t, err)
			resend := replayWireMessage("2", 2)
			resend.Body.SetInt(tagBeginSeqNo, 1)
			resend.Body.SetInt(tagEndSeqNo, 0)
			_, err = conn.Write(resend.build())
			require.NoError(t, err)
			select {
			case <-store.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("replay did not enter store")
			}
			switch reason {
			case "disconnect":
				require.NoError(t, conn.Close())
			case "peer_logout":
				_, err = conn.Write(replayWireMessage("5", 3).build())
				require.NoError(t, err)
			case "local_logout":
				go acceptor.Stop()
			}
			select {
			case <-store.exited:
			case <-time.After(time.Second):
				t.Fatal("replay did not cancel blocked storage read")
			}
			if reason != "disconnect" {
				raw, err := parser.ReadMessage()
				require.NoError(t, err)
				msg := NewMessage()
				require.NoError(t, ParseMessage(msg, raw))
				kind, e := msg.Header.GetString(tagMsgType)
				require.Nil(t, e)
				require.Equal(t, "5", kind)
				if reason == "local_logout" {
					_, err = conn.Write(replayWireMessage("5", 3).build())
					require.NoError(t, err)
				}
			}
			select {
			case <-app.logout:
			case <-time.After(3 * time.Second):
				t.Fatal("logout cleanup did not run")
			}
		})
	}
}

// With no receiver on messageOut, this exercises the same handoff backpressure
// as a writer blocked in TCP Write, without relying on socket buffer sizes.
func TestLogoutInterruptsReplayOutboundHandoff(t *testing.T) {
	memory, err := NewMemoryStoreFactory().Create(SessionID{})
	require.NoError(t, err)
	app := &replayTestApp{starvationApp: starvationApp{loggedOn: make(chan SessionID, 1)}, logout: make(chan struct{}, 2), replayReady: make(chan struct{}, 8), logoutReady: make(chan struct{}, 1)}
	s, in, out, cancel := startReplaySession(t, memory, app)
	defer cancel()
	require.NoError(t, s.queueForSend(starvationMessage(time.Now())))
	receiveReplayWire(t, out)
	sendReplayInbound(t, in, replayRequestMessage(2))
	select {
	case <-app.replayReady:
	case <-time.After(time.Second):
		t.Fatal("replay never reached outbound handoff")
	}
	// Multiple requests must not recursively block subsequent Logout.
	sendReplayInbound(t, in, replayRequestMessage(3))
	sendReplayInbound(t, in, replayRequestMessage(4))
	sendReplayInbound(t, in, replayWireMessage("5", 5))
	select {
	case <-app.logoutReady:
	case <-time.After(time.Second):
		t.Fatal("Logout blocked behind replay output")
	}
	msg := receiveReplayWire(t, out)
	kind, e := msg.Header.GetString(tagMsgType)
	require.Nil(t, e)
	require.Equal(t, "5", kind)
	select {
	case <-app.logout:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not finish")
	}
	_, ok := <-out
	require.False(t, ok, "replay output followed Logout")
}

func TestHealthyReplayKeepsLiveOutputAfterHistory(t *testing.T) {
	memory, err := NewMemoryStoreFactory().Create(SessionID{})
	require.NoError(t, err)
	app := &replayTestApp{starvationApp: starvationApp{loggedOn: make(chan SessionID, 1)}, logout: make(chan struct{}, 2), replayReady: make(chan struct{}, 8)}
	s, in, out, cancel := startReplaySession(t, memory, app)
	defer cancel()
	for range 2 {
		require.NoError(t, s.queueForSend(starvationMessage(time.Now())))
		receiveReplayWire(t, out)
	}
	sendReplayInbound(t, in, replayRequestMessage(2))
	select {
	case <-app.replayReady:
	case <-time.After(time.Second):
		t.Fatal("replay did not prepare history")
	}
	require.NoError(t, s.queueForSend(starvationMessage(time.Now())))
	for _, want := range []struct {
		kind string
		seq  int
		dup  bool
	}{{"4", 1, true}, {"B", 2, true}, {"B", 3, true}, {"B", 4, false}} {
		msg := receiveReplayWire(t, out)
		kind, e := msg.Header.GetString(tagMsgType)
		require.Nil(t, e)
		require.Equal(t, want.kind, kind)
		seq, e := msg.Header.GetInt(tagMsgSeqNum)
		require.Nil(t, e)
		require.Equal(t, want.seq, seq)
		require.Equal(t, want.dup, msg.Header.Has(tagPossDupFlag))
	}
	sendReplayInbound(t, in, replayWireMessage("5", 3))
	require.Equal(t, "5", messageKind(t, receiveReplayWire(t, out)))
}
func replayRequestMessage(seq int) *Message {
	m := replayWireMessage("2", seq)
	m.Body.SetInt(tagBeginSeqNo, 1)
	m.Body.SetInt(tagEndSeqNo, 0)
	return m
}
func sendReplayInbound(t *testing.T, in chan<- fixIn, m *Message) {
	t.Helper()
	select {
	case in <- fixIn{bytes: bytes.NewBuffer(m.build()), receiveTime: time.Now()}:
	case <-time.After(time.Second):
		t.Fatal("inbound blocked")
	}
}
func receiveReplayWire(t *testing.T, out <-chan []byte) *Message {
	t.Helper()
	select {
	case raw, ok := <-out:
		require.True(t, ok, "disconnected before expected output")
		m := NewMessage()
		require.NoError(t, ParseMessage(m, bytes.NewBuffer(raw)))
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("output stalled")
		return nil
	}
}
func messageKind(t *testing.T, m *Message) string {
	t.Helper()
	kind, e := m.Header.GetString(tagMsgType)
	require.Nil(t, e)
	return kind
}
func startReplaySession(t *testing.T, store MessageStore, app Application) (*session, chan fixIn, chan []byte, context.CancelFunc) {
	t.Helper()
	s, err := (sessionFactory{}).newSession(SessionID{BeginString: "FIX.4.4", SenderCompID: "VENUE", TargetCompID: "DEALER"}, replayStoreFactory{store}, NewSessionSettings(), NewNullLogFactory(), app)
	require.NoError(t, err)
	s.LogoutTimeout = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.run(); close(done) }()
	in := make(chan fixIn, 8)
	out := make(chan []byte)
	require.NoError(t, s.connectContext(ctx, in, out))
	t.Cleanup(func() {
		cancel()
		s.stop()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("session leaked")
		}
	})
	m := replayWireMessage("A", 1)
	m.Body.SetInt(tagEncryptMethod, 0)
	m.Body.SetInt(tagHeartBtInt, 30)
	sendReplayInbound(t, in, m)
	require.Equal(t, "A", messageKind(t, receiveReplayWire(t, out)))
	return s, in, out, cancel
}

func (a *replayTestApp) ToApp(msg *Message, _ SessionID) error {
	if msg.Header.Has(tagPossDupFlag) && a.replayReady != nil {
		a.replayReady <- struct{}{}
	}
	return nil
}
func (a *replayTestApp) ToAdmin(msg *Message, _ SessionID) {
	kind, _ := msg.Header.GetString(tagMsgType)
	if kind == "5" && a.logoutReady != nil {
		a.logoutReady <- struct{}{}
	}
}

func TestInvalidLogoutDoesNotCancelReplay(t *testing.T) {
	memory, err := NewMemoryStoreFactory().Create(SessionID{})
	require.NoError(t, err)
	store := &blockingReplayStore{MessageStore: memory, entered: make(chan struct{}), exited: make(chan struct{}), release: make(chan struct{})}
	app := &replayTestApp{starvationApp: starvationApp{loggedOn: make(chan SessionID, 1)}, logout: make(chan struct{}, 2), rejectedLogout: make(chan struct{}, 1)}
	s, in, out, cancel := startReplaySession(t, store, app)
	defer cancel()
	require.NoError(t, s.queueForSend(starvationMessage(time.Now())))
	receiveReplayWire(t, out)
	sendReplayInbound(t, in, replayRequestMessage(2))
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("no replay")
	}
	invalid := replayWireMessage("5", 3)
	invalid.Body.SetString(tagText, "reject")
	sendReplayInbound(t, in, invalid)
	select {
	case <-app.rejectedLogout:
	case <-time.After(time.Second):
		t.Fatal("validation blocked")
	}
	select {
	case <-store.exited:
		t.Fatal("rejected Logout canceled replay")
	default:
	}
	sendReplayInbound(t, in, replayWireMessage("5", 4))
	require.Equal(t, "3", messageKind(t, receiveReplayWire(t, out)))
	require.Equal(t, "5", messageKind(t, receiveReplayWire(t, out)))
}
func (a *replayTestApp) FromAdmin(msg *Message, _ SessionID) MessageRejectError {
	if kind, _ := msg.Header.GetString(tagMsgType); kind == "2" && a.resendAccepted != nil {
		a.resendAccepted <- struct{}{}
		if a.requestGate != nil {
			<-a.requestGate
		}
	}
	if text, _ := msg.Body.GetString(tagText); text == "reject" && a.rejectedLogout != nil {
		a.rejectedLogout <- struct{}{}
		return ValueIsIncorrect(tagText)
	}
	return nil
}
func TestMemoryReplayCancellation(t *testing.T) {
	for _, scenario := range []string{"before_sparse_range", "after_last_callback"} {
		t.Run(scenario, func(t *testing.T) {
			store, err := NewMemoryStoreFactory().Create(SessionID{})
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			end := int(^uint(0) >> 1)
			if scenario == "before_sparse_range" {
				cancel()
			} else {
				end = 1
				require.NoError(t, store.SaveMessage(1, []byte("stored")))
			}
			err = store.(ContextMessageStore).IterateMessagesContext(ctx, 1, end, func([]byte) error { cancel(); return nil })
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}

func TestPendingReplayDoesNotPrecedeOriginalLiveMessage(t *testing.T) {
	memory, err := NewMemoryStoreFactory().Create(SessionID{})
	require.NoError(t, err)
	app := &replayTestApp{starvationApp: starvationApp{loggedOn: make(chan SessionID, 1)}, logout: make(chan struct{}, 2), replayReady: make(chan struct{}, 16), resendAccepted: make(chan struct{}, 8)}
	s, in, out, cancel := startReplaySession(t, memory, app)
	defer cancel()
	require.NoError(t, s.queueForSend(starvationMessage(time.Now())))
	receiveReplayWire(t, out)
	sendReplayInbound(t, in, replayRequestMessage(2))
	<-app.replayReady
	require.NoError(t, s.queueForSend(starvationMessage(time.Now()))) // sequence 3, not yet on wire
	sendReplayInbound(t, in, replayRequestMessage(3))
	<-app.resendAccepted
	<-app.resendAccepted
	for _, want := range []struct {
		kind string
		seq  int
		dup  bool
	}{{"4", 1, true}, {"B", 2, true}, {"B", 3, false}, {"4", 1, true}, {"B", 2, true}, {"B", 3, true}} {
		m := receiveReplayWire(t, out)
		require.Equal(t, want.kind, messageKind(t, m))
		seq, e := m.Header.GetInt(tagMsgSeqNum)
		require.Nil(t, e)
		require.Equal(t, want.seq, seq)
		require.Equal(t, want.dup, m.Header.Has(tagPossDupFlag), "original must precede its replay")
	}
	sendReplayInbound(t, in, replayWireMessage("5", 4))
	receiveReplayWire(t, out)
}

type resetCheckingReplayStore struct {
	*blockingReplayStore
	operations chan bool
}

func (s *resetCheckingReplayStore) checkWorker() {
	select {
	case <-s.entered:
		select {
		case <-s.exited:
			s.operations <- false
		default:
			s.operations <- true
		}
	default:
	}
}
func (s *resetCheckingReplayStore) Reset() error   { s.checkWorker(); return s.MessageStore.Reset() }
func (s *resetCheckingReplayStore) Refresh() error { s.checkWorker(); return s.MessageStore.Refresh() }
func TestLogonJoinsReplayBeforeStoreReset(t *testing.T) {
	for _, action := range []string{"reset_logon", "refresh_logon", "public_reset"} {
		t.Run(action, func(t *testing.T) {
			memory, err := NewMemoryStoreFactory().Create(SessionID{})
			require.NoError(t, err)
			store := &resetCheckingReplayStore{blockingReplayStore: &blockingReplayStore{MessageStore: memory, entered: make(chan struct{}), exited: make(chan struct{}), release: make(chan struct{})}, operations: make(chan bool, 4)}
			app := &replayTestApp{starvationApp: starvationApp{loggedOn: make(chan SessionID, 1)}, logout: make(chan struct{}, 2)}
			s, in, out, cancel := startReplaySession(t, store, app)
			defer cancel()
			// RefreshOnLogon is immutable configuration; set it before sending work.
			if action == "refresh_logon" {
				s.RefreshOnLogon = true
			}
			require.NoError(t, s.queueForSend(starvationMessage(time.Now())))
			receiveReplayWire(t, out)
			sendReplayInbound(t, in, replayRequestMessage(2))
			<-store.entered
			if action == "public_reset" {
				require.NoError(t, registerSession(s))
				defer UnregisterSession(s.sessionID)
				resetDone := make(chan error, 1)
				go func() { resetDone <- ResetSession(s.sessionID) }()
				require.Equal(t, "5", messageKind(t, receiveReplayWire(t, out)))
				select {
				case err := <-resetDone:
					require.NoError(t, err)
				case <-time.After(time.Second):
					t.Fatal("public reset stalled")
				}
			} else {
				m := replayWireMessage("A", 3)
				m.Body.SetInt(tagEncryptMethod, 0)
				m.Body.SetInt(tagHeartBtInt, 30)
				if action == "reset_logon" {
					m.Header.SetInt(tagMsgSeqNum, 1)
					m.Body.SetField(tagResetSeqNumFlag, FIXBoolean(true))
				}
				sendReplayInbound(t, in, m)
				require.Equal(t, "A", messageKind(t, receiveReplayWire(t, out)))
			}
			select {
			case overlap := <-store.operations:
				require.False(t, overlap, "store reset/refresh raced active replay")
			case <-time.After(time.Second):
				t.Fatal("reset/refresh did not execute")
			}
		})
	}
}

func TestFirstReplayDoesNotPrecedeQueuedOriginal(t *testing.T) {
	memory, err := NewMemoryStoreFactory().Create(SessionID{})
	require.NoError(t, err)
	gate := make(chan struct{})
	app := &replayTestApp{starvationApp: starvationApp{loggedOn: make(chan SessionID, 1)}, logout: make(chan struct{}, 2), resendAccepted: make(chan struct{}, 1), requestGate: gate}
	s, in, out, cancel := startReplaySession(t, memory, app)
	defer cancel()
	require.NoError(t, s.queueForSend(starvationMessage(time.Now())))
	receiveReplayWire(t, out)
	sendReplayInbound(t, in, replayRequestMessage(2))
	<-app.resendAccepted
	require.NoError(t, s.queueForSend(starvationMessage(time.Now())))
	close(gate)
	original := receiveReplayWire(t, out)
	require.Equal(t, "B", messageKind(t, original))
	require.False(t, original.Header.Has(tagPossDupFlag), "replay overtook queued original")
	for range 3 {
		receiveReplayWire(t, out)
	}
	sendReplayInbound(t, in, replayWireMessage("5", 3))
	receiveReplayWire(t, out)
}
