package quickfix

import (
	"bytes"
	"context"
	"sync"
	"syscall"
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
	logout      chan struct{}
	replayReady chan struct{}
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

func awaitReplaySignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("expected replay lifecycle signal")
	}
}

func TestReplayRemainsSynchronousThroughPeerLogout(t *testing.T) {
	memory, err := NewMemoryStoreFactory().Create(SessionID{})
	require.NoError(t, err)
	store := &blockingReplayStore{MessageStore: memory, entered: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
	app := &replayTestApp{logout: make(chan struct{}, 2)}
	s, in, out, cancel := startReplaySession(t, store, app)
	defer cancel()
	defer func() {
		select {
		case <-store.release:
		default:
			close(store.release)
		}
	}()
	require.NoError(t, s.queueForSend(starvationMessage(time.Now())))
	receiveReplayWire(t, out)
	sendReplayInbound(t, in, replayRequestMessage(2))
	awaitReplaySignal(t, store.entered)
	sendReplayInbound(t, in, replayWireMessage("5", 3))
	select {
	case <-store.exited:
		t.Fatal("Logout interrupted synchronous replay")
	case raw := <-out:
		t.Fatalf("output overtook blocked replay: %q", raw)
	case <-time.After(100 * time.Millisecond):
	}
	close(store.release)
	require.Equal(t, "4", messageKind(t, receiveReplayWire(t, out)))
	replay := receiveReplayWire(t, out)
	require.Equal(t, "B", messageKind(t, replay))
	require.True(t, replay.Header.Has(tagPossDupFlag))
	require.Equal(t, "5", messageKind(t, receiveReplayWire(t, out)))
	awaitReplaySignal(t, app.logout)
}

func TestDisconnectCancelsSynchronousStoreRead(t *testing.T) {
	memory, err := NewMemoryStoreFactory().Create(SessionID{})
	require.NoError(t, err)
	store := &blockingReplayStore{MessageStore: memory, entered: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
	app := &replayTestApp{logout: make(chan struct{}, 2)}
	s, in, out, cancel := startReplaySession(t, store, app)
	defer cancel()
	defer close(store.release)
	require.NoError(t, s.queueForSend(starvationMessage(time.Now())))
	receiveReplayWire(t, out)
	sendReplayInbound(t, in, replayRequestMessage(2))
	awaitReplaySignal(t, store.entered)
	cancel()
	awaitReplaySignal(t, store.exited)
	awaitReplaySignal(t, app.logout)
	select {
	case _, ok := <-out:
		require.False(t, ok, "aborted replay emitted output")
	case <-time.After(time.Second):
		t.Fatal("outbound not closed")
	}
	// Output closure synchronizes the metadata reads.
	require.Equal(t, 3, memory.NextSenderMsgSeqNum())
	messages, err := memory.GetMessages(1, 2)
	require.NoError(t, err)
	require.Len(t, messages, 2)
}

func TestDisconnectReleasesReplayOutputWithoutLeakingIntoReconnect(t *testing.T) {
	memory, err := NewMemoryStoreFactory().Create(SessionID{})
	require.NoError(t, err)
	app := &replayTestApp{logout: make(chan struct{}, 2), replayReady: make(chan struct{}, 2)}
	s, in, out, cancel := startReplaySession(t, memory, app)
	defer cancel()
	require.NoError(t, s.queueForSend(starvationMessage(time.Now())))
	receiveReplayWire(t, out)
	request := replayRequestMessage(2)
	request.Body.SetInt(tagBeginSeqNo, 2) // Avoid gapfill before ToApp notification.
	sendReplayInbound(t, in, request)
	awaitReplaySignal(t, app.replayReady)
	cancel()
	awaitReplaySignal(t, app.logout)
	select {
	case _, ok := <-out:
		require.False(t, ok)
	case <-time.After(time.Second):
		t.Fatal("replay still owns output")
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	in2 := make(chan fixIn, 8)
	out2 := make(chan []byte)
	require.NoError(t, s.connectContext(ctx2, in2, out2))
	logon := replayWireMessage("A", 2) // Interrupted request has not advanced target sequence.
	logon.Body.SetInt(tagEncryptMethod, 0)
	logon.Body.SetInt(tagHeartBtInt, 30)
	sendReplayInbound(t, in2, logon)
	response := receiveReplayWire(t, out2)
	require.Equal(t, "A", messageKind(t, response))
	seq, e := response.Header.GetInt(tagMsgSeqNum)
	require.Nil(t, e)
	require.Equal(t, 3, seq)
	cancel() // Old lifetime cannot cancel the replacement.
	require.NoError(t, s.queueForSend(starvationMessage(time.Now())))
	live := receiveReplayWire(t, out2)
	require.Equal(t, "B", messageKind(t, live))
	require.False(t, live.Header.Has(tagPossDupFlag))
}

// Hide the optional context interface.
type legacyCancelStore struct {
	MessageStore
	cancel context.CancelFunc
}

func (s *legacyCancelStore) IterateMessages(begin, end int, cb func([]byte) error) error {
	return s.MessageStore.IterateMessages(begin, end, func(raw []byte) error {
		err := cb(raw)
		s.cancel()
		return err
	})
}
func TestLegacyStoreCancellationDoesNotCompleteGapFill(t *testing.T) {
	memory, err := NewMemoryStoreFactory().Create(SessionID{})
	require.NoError(t, err)
	store := &legacyCancelStore{MessageStore: memory}
	app := &replayTestApp{logout: make(chan struct{}, 2)}
	_, in, out, cancel := startReplaySession(t, store, app)
	defer cancel()
	store.cancel = cancel
	// Completing this admin-only range would emit a gap fill.
	sendReplayInbound(t, in, replayRequestMessage(2))
	awaitReplaySignal(t, app.logout)
	select {
	case _, ok := <-out:
		require.False(t, ok, "canceled range emitted completion gapfill")
	case <-time.After(time.Second):
		t.Fatal("replay did not unwind")
	}
}

func TestTerminalWriterUnwindsSynchronousReplay(t *testing.T) {
	memory, err := NewMemoryStoreFactory().Create(SessionID{})
	require.NoError(t, err)
	app := &replayTestApp{logout: make(chan struct{}, 2)}
	s, in, out, cancel := startReplaySession(t, memory, app)
	defer cancel()
	require.NoError(t, s.queueForSend(starvationMessage(time.Now())))
	receiveReplayWire(t, out)
	ctx := s.connectionContext
	writer := &replayErrorWriter{err: syscall.EPIPE, written: make(chan []byte, 2)}
	done := make(chan struct{})
	go func() { writeLoop(ctx, cancel, writer, out, nullLog{}); close(done) }()
	sendReplayInbound(t, in, replayRequestMessage(2))
	awaitReplaySignal(t, done)
	awaitReplaySignal(t, app.logout)
	require.Len(t, writer.written, 1, "writer sent more of the abandoned range")
	select {
	case _, ok := <-out:
		require.False(t, ok)
	case <-time.After(time.Second):
		t.Fatal("replay did not release output")
	}
}
