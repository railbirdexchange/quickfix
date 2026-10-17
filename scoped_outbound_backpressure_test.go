package quickfix

import (
	"bytes"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The writer owns the connection fields. Test goroutines control only channels;
// wire bytes are inspected after the writer's completion barrier.
type scopedBackpressureConnection struct {
	scopedOutboundBenchmarkConnection
	entered, release, closing, closeRelease chan struct{}
	writeOnce, releaseOnce, closeOnce       sync.Once
	failure                                 error
}

func (connection *scopedBackpressureConnection) Write(wire []byte) (int, error) {
	connection.writeOnce.Do(func() { close(connection.entered) })
	<-connection.release
	if connection.failure != nil {
		return 0, connection.failure
	}
	return connection.scopedOutboundBenchmarkConnection.Write(wire)
}

func (connection *scopedBackpressureConnection) Close() error {
	if connection.closing != nil {
		connection.closeOnce.Do(func() { close(connection.closing) })
		<-connection.closeRelease
	}
	return nil
}

func (connection *scopedBackpressureConnection) unblock() {
	connection.releaseOnce.Do(func() { close(connection.release) })
}

func scopedBackpressureWait(t *testing.T, done <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal(label)
	}
}

func newScopedBackpressureWriter(t *testing.T, current *session, depth int, failure error) (*outboundWriter, *scopedBackpressureConnection) {
	t.Helper()
	connection := &scopedBackpressureConnection{
		scopedOutboundBenchmarkConnection: scopedOutboundBenchmarkConnection{wire: make([]byte, 8192)},
		entered:                           make(chan struct{}), release: make(chan struct{}), failure: failure,
	}
	writer := newOutboundWriter(connection, depth, time.Second, current.messageEvent)
	current.writeConnection = writer
	current.setState(current, inSession{})
	t.Cleanup(connection.unblock)
	go writer.run()
	return writer, connection
}

func finishScopedBackpressureWriter(t *testing.T, current *session, writer *outboundWriter) {
	t.Helper()
	current.sendMutex.Lock()
	err := current.flushOutbound(true)
	current.sendMutex.Unlock()
	require.NoError(t, err)
	require.NoError(t, writer.finish())
	current.writeConnection = nil
	require.Len(t, current.outbound.vacant, len(current.outbound.slots))
}

func TestScopedOutboundBackpressureOrdinaryBurst(t *testing.T) {
	for _, depth := range []int{0, 2} {
		for _, fromApp := range []bool{false, true} {
			t.Run(strconv.Itoa(depth)+"/from_app="+strconv.FormatBool(fromApp), func(t *testing.T) {
				current, application, store := newScopedOutboundLifecycleSession(t)
				require.NoError(t, registerSession(current))
				t.Cleanup(func() { require.NoError(t, UnregisterSession(current.sessionID)) })
				writer, connection := newScopedBackpressureWriter(t, current, depth, nil)
				connection.unblock()
				var sendError error
				burst := func() {
					for index := 0; index < 6; index++ {
						if sendError = SendToTarget(scopedOutboundLifecycleOrder("burst-"+strconv.Itoa(index)), current.sessionID); sendError != nil {
							return
						}
					}
				}
				done := make(chan struct{})
				if fromApp {
					application.fromApp = func(*Message) MessageRejectError { burst(); return nil }
					factory := MessageFactory{}
					wire := factory.NewOrderSingle().build()
					slot := inboundRingTestAcquire(t, current.boundedIn.ring)
					slot.frame = append(slot.frame[:0], wire...)
					go func() { current.Incoming(current, fixIn{slot: slot, receiveTime: time.Now()}); close(done) }()
				} else {
					go func() { burst(); close(done) }()
				}
				scopedBackpressureWait(t, done, "ordinary burst requires session dispatch to progress")
				require.NoError(t, sendError)
				require.Equal(t, 7, store.NextSenderMsgSeqNum())
				if fromApp {
					require.Equal(t, 2, store.NextTargetMsgSeqNum())
				}
				finishScopedBackpressureWriter(t, current, writer)
				for index := 0; index < 6; index++ {
					require.Contains(t, string(connection.wire[:connection.used]), "11=burst-"+strconv.Itoa(index)+"\x01")
				}
				require.Equal(t, 6, bytes.Count(connection.wire[:connection.used], []byte("8=FIX.4.2\x01")))
			})
		}
	}
}

func TestScopedOutboundBackpressureIgnoresStolenSessionWake(t *testing.T) {
	current, application, store := newScopedOutboundLifecycleSession(t)
	writer, connection := newScopedBackpressureWriter(t, current, 2, nil)
	var callbacks atomic.Int32
	application.toApp = func(*Message) error { callbacks.Add(1); return nil }
	for index := 0; index < 3; index++ {
		require.NoError(t, current.queueForSend(scopedOutboundLifecycleOrder(strconv.Itoa(index))))
	}
	current.sendMutex.Lock()
	require.NoError(t, current.flushOutbound(true))
	current.sendMutex.Unlock()
	scopedBackpressureWait(t, connection.entered, "writer did not enter gated write")
	require.Empty(t, current.outbound.vacant)
	before := fingerprintBoundedFullStore(store)
	stop, stolen := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stolen)
		for {
			select {
			case <-current.messageEvent:
			case <-stop:
				return
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-stolen })
	entered, result := make(chan struct{}), make(chan error, 1)
	go func() {
		current.resendMutex.RLock()
		defer current.resendMutex.RUnlock()
		current.sendMutex.Lock()
		defer current.sendMutex.Unlock()
		close(entered)
		slot, err := current.prepareOutbound(scopedOutboundLifecycleOrder("after-wait"), nil, false)
		if err == nil {
			current.outbound.enqueue(slot)
		}
		result <- err
	}()
	scopedBackpressureWait(t, entered, "sender did not acquire admission ownership")
	select {
	case err := <-result:
		t.Fatalf("sender returned while all output slots were writer-owned: %v", err)
	case <-time.After(time.Millisecond):
	}
	require.Equal(t, int32(3), callbacks.Load())
	require.Equal(t, before.nextSender, store.NextSenderMsgSeqNum())
	connection.unblock()
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("stolen session notification stranded a vacant slot")
	}
	require.Equal(t, int32(4), callbacks.Load())
	finishScopedBackpressureWriter(t, current, writer)
}

func TestScopedOutboundBackpressureFailurePrecedesWriterJoin(t *testing.T) {
	for _, depth := range []int{0, 2} {
		t.Run(strconv.Itoa(depth), func(t *testing.T) {
			current, application, store := newScopedOutboundLifecycleSession(t)
			writer, connection := newScopedBackpressureWriter(t, current, depth, io.ErrClosedPipe)
			connection.closing, connection.closeRelease = make(chan struct{}), make(chan struct{})
			t.Cleanup(func() { close(connection.closeRelease) })
			var callbacks atomic.Int32
			application.toApp = func(*Message) error { callbacks.Add(1); return nil }
			// With no writer buffering the third send blocks on a permit while
			// draining pending entries. With buffering the fourth waits vacant.
			count := 2
			if depth != 0 {
				count = 3
			}
			for index := 0; index < count; index++ {
				require.NoError(t, current.queueForSend(scopedOutboundLifecycleOrder(strconv.Itoa(index))))
			}
			if depth != 0 {
				current.sendMutex.Lock()
				require.NoError(t, current.flushOutbound(true))
				current.sendMutex.Unlock()
			}
			before := fingerprintBoundedFullStore(store)
			result := make(chan error, 1)
			go func() { result <- current.queueForSend(scopedOutboundLifecycleOrder("must-not-commit")) }()
			scopedBackpressureWait(t, connection.entered, "writer did not enter gated write")
			connection.unblock()
			scopedBackpressureWait(t, connection.closing, "writer did not reach gated Close")
			select {
			case <-writer.done:
				t.Fatal("done published before connection owner joined")
			default:
			}
			select {
			case err := <-result:
				require.ErrorIs(t, err, io.ErrClosedPipe)
			case <-time.After(2 * time.Second):
				t.Fatal("failed write did not wake blocked admission before Close completed")
			}
			require.Equal(t, int32(count), callbacks.Load())
			require.Equal(t, before, fingerprintBoundedFullStore(store))
		})
	}
}

func TestScopedOutboundBackpressurePreservesImmediateBoundaries(t *testing.T) {
	for _, scenario := range []string{"prelogon", "offline", "reset_pending", "scheduled_reset", "reset_logon", "journal_full", "replacement"} {
		t.Run(scenario, func(t *testing.T) {
			current, application, store := newScopedOutboundLifecycleSession(t)
			_, connection := newScopedBackpressureWriter(t, current, 2, nil)
			current.outboundLoggedOn.Store(false)
			var callbacks atomic.Int32
			application.toApp = func(*Message) error { callbacks.Add(1); return nil }
			application.toAdmin = func(*Message) { callbacks.Add(1) }
			require.NoError(t, current.queueForSend(scopedOutboundLifecycleOrder("first")))
			require.NoError(t, current.queueForSend(scopedOutboundLifecycleOrder("second")))
			message, expected := scopedOutboundLifecycleOrder("rejected"), ErrOutboundCapacity
			current.outboundLoggedOn.Store(true)
			switch scenario {
			case "prelogon":
				current.outboundLoggedOn.Store(false)
			case "offline":
				writer := current.writeConnection
				current.writeConnection = nil
				t.Cleanup(func() { current.writeConnection = writer })
			case "reset_pending":
				current.stageLogonReset()
				expected = ErrOutboundResetPending
			case "scheduled_reset":
				current.outbound.scheduledResetPending.Store(true)
				expected = ErrOutboundResetPending
			case "reset_logon":
				message.Header.SetString(tagMsgType, "A")
				message.Body.SetBool(tagResetSeqNumFlag, true)
			case "journal_full":
				for sequence := 3; sequence <= 8; sequence++ {
					require.NoError(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(sequence, []byte("retained")))
				}
				expected = ErrJournalFull
			case "replacement":
				progress, err := current.outbound.reserve(true)
				require.NoError(t, err)
				t.Cleanup(progress.release)
			}
			before := fingerprintBoundedFullStore(store)
			result := make(chan error, 1)
			go func() {
				if scenario == "replacement" {
					result <- current.dropAndSendInReplyTo(message, nil)
				} else {
					result <- current.queueForSend(message)
				}
			}()
			select {
			case err := <-result:
				require.ErrorIs(t, err, expected)
			case <-time.After(2 * time.Second):
				t.Fatal("finite admission boundary unexpectedly waited for writer progress")
			}
			require.Equal(t, before, fingerprintBoundedFullStore(store))
			require.Equal(t, int32(2), callbacks.Load())
			require.Equal(t, 2, current.outbound.count)
			select {
			case <-connection.entered:
				t.Fatal("rejected admission flushed queued bytes")
			default:
			}
		})
	}
}

type scopedBackpressureLogoutApplication struct {
	*scopedOutboundLifecycleApplication
	onLogout func()
}

func (application *scopedBackpressureLogoutApplication) OnLogout(SessionID) { application.onLogout() }

func TestScopedOutboundBackpressureClearsPublicationBeforeOnLogout(t *testing.T) {
	current, application, _ := newScopedOutboundLifecycleSession(t)
	current.setState(current, inSession{})
	require.True(t, current.outboundLoggedOn.Load())
	require.NoError(t, current.queueForSend(scopedOutboundLifecycleOrder("first")))
	require.NoError(t, current.queueForSend(scopedOutboundLifecycleOrder("second")))
	called := false
	current.application = &scopedBackpressureLogoutApplication{application, func() {
		called = true
		require.False(t, current.outboundLoggedOn.Load())
		require.ErrorIs(t, current.queueForSend(scopedOutboundLifecycleOrder("logout-callback")), ErrOutboundCapacity)
	}}
	current.handleDisconnectState(current)
	require.True(t, called)
	require.False(t, current.outboundLoggedOn.Load())
}
