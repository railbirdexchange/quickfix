package quickfix

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/internal"
	"github.com/stretchr/testify/require"
)

func newSessionSendFairnessFixture(t *testing.T, bounded bool) (*session, *scopedOutboundLifecycleApplication, *BoundedStore, *scopedOutboundBenchmarkConnection, <-chan []byte) {
	t.Helper()
	current, application, store := newScopedOutboundLifecycleSession(t)
	var err error
	current.SessionTime, err = internal.NewUTCTimeRange(internal.NewTimeOfDay(0, 0, 0), internal.NewTimeOfDay(0, 0, 0), nil)
	require.NoError(t, err)
	var connection *scopedOutboundBenchmarkConnection
	var legacyOutput chan []byte
	if bounded {
		connection = &scopedOutboundBenchmarkConnection{wire: make([]byte, 8192)}
		current.writeConnection = newOutboundWriter(connection, 2, time.Second, current.messageEvent)
		go current.writeConnection.run()
	} else {
		current.outbound = nil
		legacyOutput = make(chan []byte, 8)
		current.messageOut = legacyOutput
	}
	current.setState(current, inSession{})
	// Establish the current scheduled range before an external sender owns
	// the send lock. Steady-state schedule checks must not wait for that sender.
	current.CheckSessionTime(current, time.Now())
	return current, application, store, connection, legacyOutput
}

func requireSessionSendFairnessOutput(t *testing.T, current *session, connection *scopedOutboundBenchmarkConnection, legacyOutput <-chan []byte, ids ...string) {
	t.Helper()
	var stream []byte
	if connection != nil {
		require.NoError(t, current.writeConnection.finish(), "completed writes are required before inspecting peer bytes")
		current.writeConnection = nil
		require.Zero(t, current.outbound.count)
		require.Len(t, current.outbound.vacant, len(current.outbound.slots))
		stream = connection.wire[:connection.used]
	} else {
		require.Empty(t, current.toSend)
		for range ids {
			select {
			case wire := <-legacyOutput:
				stream = append(stream, wire...)
			default:
				t.Fatal("accepted legacy send did not reach its output channel")
			}
		}
		require.Empty(t, current.messageOut)
	}
	framer := newParser(bytes.NewReader(stream))
	for index, id := range ids {
		wire, err := framer.ReadMessage()
		require.NoError(t, err)
		message := NewMessage()
		require.NoError(t, ParseMessage(message, wire))
		actual, reject := message.Body.GetString(11)
		require.Nil(t, reject)
		require.Equal(t, id, actual, "accepted output order changed")
		sequence, reject := message.Header.GetInt(tagMsgSeqNum)
		require.Nil(t, reject)
		require.Equal(t, index+1, sequence)
	}
	_, err := framer.ReadMessage()
	require.ErrorIs(t, err, io.EOF, "unexpected extra output")
}

func TestSessionSendFairnessSlowCallbackDoesNotBlockInbound(t *testing.T) {
	for _, bounded := range []bool{false, true} {
		for _, rejectSend := range []bool{false, true} {
			name := "legacy"
			if bounded {
				name = "bounded"
			}
			if rejectSend {
				name += "/rejected_callback"
			} else {
				name += "/accepted_callback"
			}
			t.Run(name, func(t *testing.T) {
				current, application, store, connection, legacyOutput := newSessionSendFairnessFixture(t, bounded)
				require.NoError(t, current.queueForSend(scopedOutboundLifecycleOrder("already-pending")))
				entered, release, sent := make(chan struct{}), make(chan struct{}), make(chan struct{})
				result := make(chan error, 1)
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				failure := errors.New("ToApp declined the second send")
				application.toApp = func(*Message) error {
					close(entered)
					<-release
					if rejectSend {
						return failure
					}
					return nil
				}
				fromApp := make(chan struct{}, 1)
				application.fromApp = func(*Message) MessageRejectError { fromApp <- struct{}{}; return nil }
				go func() {
					defer close(sent)
					result <- current.queueForSend(scopedOutboundLifecycleOrder("second-send"))
				}()
				t.Cleanup(func() { unblock(); scopedBackpressureWait(t, sent, "blocked sender did not exit") })
				scopedBackpressureWait(t, entered, "sender did not enter ToApp")
				// Consume the only existing wake while ToApp owns sendMutex. A
				// rejected callback will not enqueue anything to produce a new wake.
				outboundLifecycleWake(t, current)
				wire := inboundIntegrationOrder(1, "inbound-during-ToApp").build()
				incoming := fixIn{bytes: bytes.NewBuffer(wire), receiveTime: time.Now()}
				if bounded {
					incoming.slot = inboundRingTestAcquire(t, current.boundedIn.ring)
					incoming.slot.frame = append(incoming.slot.frame[:0], wire...)
					incoming.bytes = nil
				}
				flushed, dispatched := make(chan struct{}), make(chan struct{})
				go func() {
					current.SendAppMessages(current)
					close(flushed)
					current.Incoming(current, incoming)
					close(dispatched)
				}()
				t.Cleanup(func() { unblock(); scopedBackpressureWait(t, dispatched, "session dispatch did not exit") })
				scopedBackpressureWait(t, flushed, "session flush waited for the slow ToApp callback")
				scopedBackpressureWait(t, fromApp, "scheduled inbound processing waited for the send lock")
				scopedBackpressureWait(t, dispatched, "inbound dispatch did not complete")
				select {
				case <-sent:
					t.Fatal("fixture released ToApp before proving inbound progress")
				default:
				}
				require.Equal(t, 2, store.NextTargetMsgSeqNum())
				unblock()
				scopedBackpressureWait(t, sent, "sender did not finish after callback release")
				if rejectSend {
					require.ErrorIs(t, <-result, failure)
				} else {
					require.NoError(t, <-result)
				}
				if bounded {
					require.Zero(t, current.outbound.count, "the releasing sender must flush deferred output before another sender can take the lock")
				} else {
					require.Empty(t, current.toSend, "the releasing sender must flush even when ToApp rejects its own message")
				}
				outboundLifecycleWake(t, current)
				current.SendAppMessages(current)
				ids := []string{"already-pending"}
				if !rejectSend {
					ids = append(ids, "second-send")
				}
				requireSessionSendFairnessOutput(t, current, connection, legacyOutput, ids...)
				require.Equal(t, len(ids)+1, store.NextSenderMsgSeqNum())
				require.IsType(t, inSession{}, current.State)
			})
		}
	}
}

func TestSessionSendFairnessOtherLockOwnerRestoresConsumedWake(t *testing.T) {
	for _, bounded := range []bool{false, true} {
		name := "legacy"
		if bounded {
			name = "bounded"
		}
		t.Run(name, func(t *testing.T) {
			current, _, store, connection, legacyOutput := newSessionSendFairnessFixture(t, bounded)
			require.NoError(t, current.queueForSend(scopedOutboundLifecycleOrder("pending-before-other-owner")))
			current.sendMutex.Lock()
			locked := true
			started := false
			returned := make(chan struct{})
			defer func() {
				if locked {
					current.unlockSend()
				}
				if started {
					scopedBackpressureWait(t, returned, "session flush did not exit after lock release")
				}
			}()
			outboundLifecycleWake(t, current)
			started = true
			go func() { current.SendAppMessages(current); close(returned) }()
			scopedBackpressureWait(t, returned, "session flush waited for a non-sender lock owner")
			require.Empty(t, current.messageEvent, "failed lock acquisition must not self-notify and spin")
			current.unlockSend()
			locked = false
			outboundLifecycleWake(t, current)
			current.SendAppMessages(current)
			requireSessionSendFairnessOutput(t, current, connection, legacyOutput, "pending-before-other-owner")
			require.Equal(t, 2, store.NextSenderMsgSeqNum())
		})
	}
}

func TestSessionSendFairnessBlockedWriterWaitsForCompletionWake(t *testing.T) {
	current, _, store := newScopedOutboundLifecycleSession(t)
	writer, connection := newScopedBackpressureWriter(t, current, 1, nil)
	for _, id := range []string{"first", "second", "third"} {
		require.NoError(t, current.queueForSend(scopedOutboundLifecycleOrder(id)))
	}
	scopedBackpressureWait(t, connection.entered, "writer did not enter its blocked write")
	outboundLifecycleWake(t, current)
	current.SendAppMessages(current)
	require.Equal(t, 1, current.outbound.count, "the pending third message must await a writer permit")
	require.Empty(t, current.messageEvent, "blocked output must not continually wake the session")
	connection.unblock()
	outboundLifecycleWake(t, current)
	current.SendAppMessages(current)
	// The independent writer's completion has restored progress; finishing
	// through the existing barrier proves every accepted frame was written.
	finishScopedBackpressureWriter(t, current, writer)
	framer := newParser(bytes.NewReader(connection.wire[:connection.used]))
	for index, id := range []string{"first", "second", "third"} {
		wire, err := framer.ReadMessage()
		require.NoError(t, err)
		message := NewMessage()
		require.NoError(t, ParseMessage(message, wire))
		actual, reject := message.Body.GetString(11)
		require.Nil(t, reject)
		require.Equal(t, id, actual)
		sequence, reject := message.Header.GetInt(tagMsgSeqNum)
		require.Nil(t, reject)
		require.Equal(t, index+1, sequence)
	}
	_, err := framer.ReadMessage()
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, 4, store.NextSenderMsgSeqNum())
}
