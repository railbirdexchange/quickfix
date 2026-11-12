package quickfix

import (
	"bufio"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/internal"
	"github.com/stretchr/testify/require"
)

func TestBufferedLogonRecoveryCancellationPreservesInput(t *testing.T) {
	for _, tc := range []struct {
		name     string
		logonSeq int
		wantApps []int
		wantNext int
	}{
		{"in_sequence", 1, []int{2}, 3},
		{"sequence_gap_is_not_skipped", 3, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := &inboundDrainApp{logout: make(chan struct{}, 1)}
			s, err := (sessionFactory{}).newSession(SessionID{BeginString: "FIX.4.4", SenderCompID: "VENUE", TargetCompID: "DEALER"}, NewMemoryStoreFactory(), NewSessionSettings(), NewNullLogFactory(), app)
			require.NoError(t, err)
			s.EnableNextExpectedMsgSeqNum = true
			// The peer's Logon response and subsequent application message were
			// admitted before EOF, while this initiator awaited its response.
			s.InitiateLogon = true
			s.State = logonState{}
			require.NoError(t, s.store.SetNextSenderMsgSeqNum(5))
			s.stateTimer = internal.NewEventTimer(func() {})
			s.peerTimer = internal.NewEventTimer(func() {})
			defer s.stateTimer.Stop()
			defer s.peerTimer.Stop()
			out := make(chan []byte, 8)
			s.messageOut = out
			in := make(chan fixIn, 2)
			s.messageIn = in
			logon := inboundDrainMessage("A", tc.logonSeq)
			logon.Body.SetInt(tagNextExpectedMsgSeqNum, 3)
			sendReplayInbound(t, in, logon)
			sendReplayInbound(t, in, inboundDrainMessage("D", tc.logonSeq+1))
			close(in)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			s.connectionContext = ctx

			s.drainTransportInput()

			require.Equal(t, tc.wantApps, app.apps)
			require.Equal(t, tc.wantNext, s.store.NextTargetMsgSeqNum())
			require.False(t, s.IsConnected())
			_, open := <-out
			require.False(t, open, "canceled Logon recovery must not emit a gap fill")
		})
	}
}

type inboundDrainApp struct {
	starvationApp
	mu      sync.Mutex
	apps    []int
	admins  []string
	entered chan struct{}
	release chan struct{}
	logout  chan struct{}
}

func (a *inboundDrainApp) FromApp(m *Message, _ SessionID) MessageRejectError {
	n, _ := m.Header.GetInt(tagMsgSeqNum)
	a.mu.Lock()
	a.apps = append(a.apps, n)
	a.mu.Unlock()
	if n == 9 {
		close(a.entered)
		<-a.release
	}
	return nil
}
func (a *inboundDrainApp) FromAdmin(m *Message, _ SessionID) MessageRejectError {
	k, _ := m.Header.GetString(tagMsgType)
	a.mu.Lock()
	a.admins = append(a.admins, k)
	a.mu.Unlock()
	return nil
}
func (a *inboundDrainApp) OnLogout(SessionID) {
	select {
	case a.logout <- struct{}{}:
	default:
	}
}
func inboundDrainMessage(kind string, seq int) *Message {
	m := replayWireMessage(kind, seq)
	if kind == "A" {
		m.Body.SetInt(tagEncryptMethod, 0)
		m.Body.SetInt(tagHeartBtInt, 30)
	}
	if kind == "D" {
		m.Body.SetString(11, "inbound-drain")
		m.Body.SetString(55, "TEST")
		m.Body.SetString(54, "1")
		m.Body.SetString(40, "1")
		m.Body.SetInt(38, 1)
		m.Body.SetField(60, FIXUTCTimestamp{Time: time.Now()})
	}
	return m
}
func TestBufferedInboundEOFProcessedBeforeLogout(t *testing.T) {
	app := &inboundDrainApp{entered: make(chan struct{}), release: make(chan struct{}), logout: make(chan struct{}, 1)}
	s, err := (sessionFactory{}).newSession(SessionID{BeginString: "FIX.4.4", SenderCompID: "VENUE", TargetCompID: "DEALER"}, NewMemoryStoreFactory(), NewSessionSettings(), NewNullLogFactory(), app)
	require.NoError(t, err)
	require.NoError(t, s.store.SetNextTargetMsgSeqNum(8))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan fixIn, 8)
	out := make(chan []byte, 16)
	done := make(chan struct{})
	go func() { s.run(); close(done) }()
	require.NoError(t, s.connectContext(ctx, in, out))
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	readerDone := make(chan struct{})
	go func() { readLoop(ctx, cancel, newParser(bufio.NewReader(reader)), in, nullLog{}); close(readerDone) }()
	_, err = writer.Write(inboundDrainMessage("A", 8).build())
	require.NoError(t, err)
	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("logon stalled")
	}
	_, err = writer.Write(inboundDrainMessage("D", 9).build())
	require.NoError(t, err)
	select {
	case <-app.entered:
	case <-time.After(time.Second):
		t.Fatal("seq9 not entered")
	}
	_, err = writer.Write(append(inboundDrainMessage("D", 10).build(), inboundDrainMessage("5", 11).build()...))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	select {
	case <-readerDone:
	case <-time.After(time.Second):
		t.Fatal("reader did not reach EOF")
	}
	require.Len(t, in, 2, "both complete messages must already be admitted before EOF")
	close(app.release)
	select {
	case <-app.logout:
	case <-time.After(2 * time.Second):
		t.Fatal("logout stalled")
	}
	s.stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("session did not stop")
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	t.Logf("after EOF: FromApp sequences=%v FromAdmin types=%v next-target=%d", app.apps, app.admins, s.store.NextTargetMsgSeqNum())
	require.Equal(t, []int{9, 10}, app.apps, "complete buffered application message must not be discarded at EOF")
	require.Equal(t, []string{"A", "5"}, app.admins, "buffered Logout must reach FromAdmin")
	require.Equal(t, 12, s.store.NextTargetMsgSeqNum())
}

func TestBufferedInboundProcessedAfterReplayCancellation(t *testing.T) {
	for _, tc := range []struct {
		name          string
		queuedRequest bool
		orderSeq      int
		wantApps      []int
		wantAdmins    []string
		wantNext      int
	}{
		{"order_and_logout", false, 3, []int{3}, []string{"A", "2", "5"}, 5},
		{"second_resend_order_and_logout", true, 4, []int{4}, []string{"A", "2", "2", "5"}, 6},
		{"sequence_gap_is_not_skipped", false, 4, nil, []string{"A", "2", "5"}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memory, err := NewMemoryStoreFactory().Create(SessionID{})
			require.NoError(t, err)
			store := &blockingReplayStore{MessageStore: memory, entered: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
			app := &inboundDrainApp{logout: make(chan struct{}, 1)}
			s, in, out, cancel := startReplaySession(t, store, app)
			defer cancel()
			defer close(store.release)
			require.NoError(t, s.queueForSend(starvationMessage(time.Now())))
			receiveReplayWire(t, out)
			sendReplayInbound(t, in, replayRequestMessage(2))
			awaitReplaySignal(t, store.entered)
			if tc.queuedRequest {
				sendReplayInbound(t, in, replayRequestMessage(3))
			}
			sendReplayInbound(t, in, inboundDrainMessage("D", tc.orderSeq))
			sendReplayInbound(t, in, inboundDrainMessage("5", tc.orderSeq+1))
			cancel()
			awaitReplaySignal(t, store.exited)
			awaitReplaySignal(t, app.logout)
			select {
			case _, open := <-out:
				require.False(t, open, "canceled replay must not emit a completion gap fill")
			case <-time.After(time.Second):
				t.Fatal("outbound did not close")
			}
			app.mu.Lock()
			defer app.mu.Unlock()
			require.Equal(t, tc.wantApps, app.apps)
			require.Equal(t, tc.wantAdmins, app.admins)
			require.Equal(t, tc.wantNext, memory.NextTargetMsgSeqNum())
		})
	}
}

// This store fails independently after transport cancellation; that failure must
// not be mistaken for a successfully received/aborted resend response.
type failedDrainReplayStore struct {
	MessageStore
	entered chan struct{}
}

func (s *failedDrainReplayStore) IterateMessagesContext(ctx context.Context, _, _ int, _ func([]byte) error) error {
	close(s.entered)
	<-ctx.Done()
	return errors.New("journal read failed")
}
func TestReplayStorageFailureDuringCancellationDoesNotAdvanceInput(t *testing.T) {
	memory, err := NewMemoryStoreFactory().Create(SessionID{})
	require.NoError(t, err)
	store := &failedDrainReplayStore{MessageStore: memory, entered: make(chan struct{})}
	app := &inboundDrainApp{logout: make(chan struct{}, 1)}
	_, in, out, cancel := startReplaySession(t, store, app)
	defer cancel()
	sendReplayInbound(t, in, replayRequestMessage(2))
	awaitReplaySignal(t, store.entered)
	sendReplayInbound(t, in, inboundDrainMessage("D", 3))
	cancel()
	awaitReplaySignal(t, app.logout)
	select {
	case _, open := <-out:
		require.False(t, open)
	case <-time.After(time.Second):
		t.Fatal("failed session did not close output")
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	require.Empty(t, app.apps)
	require.Equal(t, 2, memory.NextTargetMsgSeqNum())
}
