package quickfix

import (
	"bytes"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/config"
	"github.com/quickfixgo/quickfix/internal"
	"github.com/stretchr/testify/require"
)

type scopedOutboundLifecycleApplication struct {
	toAdmin func(*Message)
	toApp   func(*Message) error
	fromApp func(*Message) MessageRejectError
}

func (*scopedOutboundLifecycleApplication) OnCreate(SessionID) {}
func (*scopedOutboundLifecycleApplication) OnLogon(SessionID)  {}
func (*scopedOutboundLifecycleApplication) OnLogout(SessionID) {}
func (application *scopedOutboundLifecycleApplication) ToAdmin(message *Message, _ SessionID) {
	if application.toAdmin != nil {
		application.toAdmin(message)
	}
}
func (application *scopedOutboundLifecycleApplication) ToApp(message *Message, _ SessionID) error {
	if application.toApp != nil {
		return application.toApp(message)
	}
	return nil
}
func (*scopedOutboundLifecycleApplication) FromAdmin(*Message, SessionID) MessageRejectError {
	return nil
}
func (application *scopedOutboundLifecycleApplication) FromApp(message *Message, _ SessionID) MessageRejectError {
	if application.fromApp != nil {
		return application.fromApp(message)
	}
	return nil
}

func newScopedOutboundLifecycleSession(t *testing.T) (*session, *scopedOutboundLifecycleApplication, *BoundedStore) {
	t.Helper()
	settings := inboundIntegrationSettings(1, 1)
	settings.Set(config.BoundedMaxFrameBytes, "256")
	settings.Set(config.OutboundFrameQueueCapacity, "2")
	application := &scopedOutboundLifecycleApplication{}
	current, err := (sessionFactory{}).newSession(
		SessionID{BeginString: BeginStringFIX42, SenderCompID: "ISLD", TargetCompID: "TW", Qualifier: t.Name()},
		NewBoundedStoreFactory(8, 1024), settings, NewNullLogFactory(), application)
	require.NoError(t, err)
	current.State = inSession{}
	current.HeartBtInt = time.Hour
	current.MaxLatency = time.Hour
	current.stateTimer = internal.NewEventTimer(func() {})
	current.peerTimer = internal.NewEventTimer(func() {})
	t.Cleanup(func() {
		if current.writeConnection != nil || current.readConnection != nil {
			current.onDisconnect()
		}
		current.stateTimer.Stop()
		current.peerTimer.Stop()
		require.NoError(t, current.store.Close())
	})
	return current, application, current.store.(*BoundedStore)
}

func scopedOutboundLifecycleOrder(id string) *Message {
	message := NewMessage()
	message.Header.SetString(tagMsgType, "D")
	message.Body.SetString(11, id)
	return message
}

func assertScopedOutboundVacant(t *testing.T, current *session) {
	t.Helper()
	require.Zero(t, current.outbound.count)
	require.Len(t, current.outbound.vacant, len(current.outbound.slots))
	require.Nil(t, current.writeConnection)
	require.Nil(t, current.readConnection)
	require.Nil(t, current.messageIn)
	require.False(t, current.outbound.resetPending.Load())
}

func TestScopedOutboundWriteFailurePreservesJournalAcrossReconnect(t *testing.T) {
	for _, queueDepth := range []int{0, 2} {
		name := "unbuffered"
		if queueDepth != 0 {
			name = "buffered"
		}
		t.Run(name, func(t *testing.T) {
			current, application, store := newScopedOutboundLifecycleSession(t)
			current.ResetOnDisconnect = true
			require.NoError(t, store.SetNextTargetMsgSeqNum(7))
			failure := errors.New("partial socket write failed")
			connection := &scopedOutboundTestConnection{
				scopedOutboundBenchmarkConnection: scopedOutboundBenchmarkConnection{wire: make([]byte, 1024)},
				maxWrite:                          5, failAt: 2, failure: failure,
			}
			writer := newOutboundWriter(connection, queueDepth, time.Second, current.messageEvent)
			current.writeConnection = writer
			go writer.run()
			first := scopedOutboundLifecycleOrder("first")
			second := scopedOutboundLifecycleOrder("second")
			var callbacks []*Message
			application.toApp = func(message *Message) error {
				callbacks = append(callbacks, message)
				return nil
			}
			require.NoError(t, current.queueForSend(first))
			require.NoError(t, current.queueForSend(second))
			require.Equal(t, []*Message{first, second}, callbacks)
			retained, err := store.GetMessages(1, 2)
			require.NoError(t, err)
			require.Len(t, retained, 2)
			current.sendMutex.Lock()
			current.sendQueued(true)
			current.sendMutex.Unlock()
			select {
			case <-writer.done:
			case <-time.After(2 * time.Second):
				t.Fatal("failed writer did not stop")
			}
			require.ErrorIs(t, writer.err, failure)
			require.Equal(t, 5, connection.used)
			current.Disconnected(current)
			assertScopedOutboundVacant(t, current)
			require.Equal(t, 3, store.NextSenderMsgSeqNum())
			require.Equal(t, 7, store.NextTargetMsgSeqNum())
			after, err := store.GetMessages(1, 2)
			require.NoError(t, err)
			require.Equal(t, retained, after)

			// Reconnect with the same ring. No failed writer or pending frame may
			// retain a slot, and the new writer must receive only the next send.
			current.ResetOnDisconnect = false
			nextConnection := &scopedOutboundBenchmarkConnection{wire: make([]byte, 1024)}
			nextWriter := newOutboundWriter(nextConnection, queueDepth, time.Second, current.messageEvent)
			go nextWriter.run()
			current.onAdmin(connect{messageIn: make(chan fixIn), writer: nextWriter})
			require.True(t, current.IsConnected())
			current.State = inSession{}
			third := scopedOutboundLifecycleOrder("third")
			require.NoError(t, current.queueForSend(third))
			current.sendMutex.Lock()
			current.sendQueued(true)
			current.sendMutex.Unlock()
			current.Disconnected(current)
			assertScopedOutboundVacant(t, current)
			received := NewMessage()
			require.NoError(t, ParseMessage(received, bytes.NewBuffer(nextConnection.wire[:nextConnection.used])))
			sequence, reject := received.Header.GetInt(tagMsgSeqNum)
			require.Nil(t, reject)
			require.Equal(t, 3, sequence)
			id, reject := received.Body.GetString(11)
			require.Nil(t, reject)
			require.Equal(t, "third", id)
			require.Equal(t, 4, store.NextSenderMsgSeqNum())
		})
	}
}

func TestScopedOutboundLogonCapacityDoesNotSendSecondaryLogout(t *testing.T) {
	for _, reset := range []bool{false, true} {
		name := "ordinary_logon"
		if reset {
			name = "reset_logon"
		}
		t.Run(name, func(t *testing.T) {
			current, application, store := newScopedOutboundLifecycleSession(t)
			current.ResetOnDisconnect = true
			require.NoError(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(1, []byte("retained journal message")))
			require.NoError(t, store.SetNextTargetMsgSeqNum(7))
			creation := store.CreationTime()
			var logons, logouts int
			application.toAdmin = func(message *Message) {
				messageType, reject := message.Header.GetString(tagMsgType)
				require.Nil(t, reject)
				switch messageType {
				case "A":
					logons++
					message.Body.SetString(tagText, strings.Repeat("x", 512))
				case "5":
					logouts++
				}
			}
			factory := MessageFactory{}
			factory.SetNextSeqNum(7)
			if reset {
				factory.SetNextSeqNum(1)
			}
			logon := factory.Logon()
			if reset {
				logon.Body.SetBool(tagResetSeqNumFlag, true)
			}
			next := (inSession{}).FixMsgIn(current, logon)
			require.IsType(t, latentState{}, next)
			require.Equal(t, 1, logons)
			require.Zero(t, logouts, "a rejected send must not create another persisted message")
			require.True(t, current.preserveOnDisconnect)
			current.setState(current, next)
			assertScopedOutboundVacant(t, current)
			require.Equal(t, 2, store.NextSenderMsgSeqNum())
			require.Equal(t, 7, store.NextTargetMsgSeqNum())
			require.Equal(t, creation, store.CreationTime())
			retained, err := store.GetMessages(1, 1)
			require.NoError(t, err)
			require.Equal(t, [][]byte{[]byte("retained journal message")}, retained)
		})
	}
}

func TestScopedOutboundShutdownCapacityPreservesTarget(t *testing.T) {
	current, _, store := newScopedOutboundLifecycleSession(t)
	current.ResetOnDisconnect = true
	require.NoError(t, store.SetNextTargetMsgSeqNum(7))
	factory := MessageFactory{}
	factory.SetNextSeqNum(7)
	next := shutdownWithReason(current, factory.Logon(), true, strings.Repeat("x", 512))
	require.IsType(t, latentState{}, next)
	require.True(t, current.preserveOnDisconnect)
	current.setState(current, next)
	assertScopedOutboundVacant(t, current)
	require.Equal(t, 1, store.NextSenderMsgSeqNum())
	require.Equal(t, 7, store.NextTargetMsgSeqNum())
}

func TestScopedOutboundInitiatorResetCapacityJoinsConnection(t *testing.T) {
	current, application, store := newScopedOutboundLifecycleSession(t)
	current.State = latentState{}
	current.InitiateLogon = true
	current.ResetOnLogon = true
	current.ResetOnDisconnect = true
	require.NoError(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(1, []byte("retained before connect")))
	require.NoError(t, store.SetNextTargetMsgSeqNum(7))
	creation := store.CreationTime()
	application.toAdmin = func(message *Message) {
		message.Body.SetString(tagText, strings.Repeat("x", 512))
	}
	connection := &scopedOutboundBenchmarkConnection{wire: make([]byte, 1024)}
	incoming := make(chan fixIn, 1)
	reader, err := startBoundedRead(connection, newParser(connection), current.boundedIn, incoming, nil, time.Time{})
	require.NoError(t, err)
	writer := newOutboundWriter(connection, 0, time.Second, current.messageEvent)
	go writer.run()
	current.onAdmin(connect{messageIn: incoming, reader: reader, writer: writer})
	require.False(t, current.IsConnected())
	assertScopedOutboundVacant(t, current)
	assertInboundRingVacant(t, current.boundedIn.ring)
	select {
	case <-reader.done:
	default:
		t.Fatal("failed connect did not join its reader")
	}
	select {
	case <-writer.done:
	default:
		t.Fatal("failed connect did not join its writer")
	}
	require.Zero(t, connection.used)
	require.Equal(t, 2, store.NextSenderMsgSeqNum())
	require.Equal(t, 7, store.NextTargetMsgSeqNum())
	require.Equal(t, creation, store.CreationTime())
	retained, err := store.GetMessages(1, 1)
	require.NoError(t, err)
	require.Equal(t, [][]byte{[]byte("retained before connect")}, retained)
}

type scopedOutboundCloseErrorConnection struct {
	scopedOutboundBenchmarkConnection
}

func (*scopedOutboundCloseErrorConnection) Close() error {
	return errors.New("connection already closed")
}

func TestScopedOutboundCleanCloseErrorStillResets(t *testing.T) {
	current, _, store := newScopedOutboundLifecycleSession(t)
	current.ResetOnDisconnect = true
	require.NoError(t, store.SetNextTargetMsgSeqNum(7))
	connection := &scopedOutboundCloseErrorConnection{scopedOutboundBenchmarkConnection{wire: make([]byte, 1024)}}
	writer := newOutboundWriter(connection, 1, time.Second, current.messageEvent)
	current.writeConnection = writer
	go writer.run()
	require.NoError(t, current.queueForSend(scopedOutboundLifecycleOrder("complete")))
	current.sendMutex.Lock()
	current.sendQueued(true)
	current.sendMutex.Unlock()
	current.Disconnected(current)
	require.NoError(t, writer.err, "Close errors are not failed socket writes")
	require.Positive(t, connection.used)
	assertScopedOutboundVacant(t, current)
	require.Equal(t, 1, store.NextSenderMsgSeqNum())
	require.Equal(t, 1, store.NextTargetMsgSeqNum())
	require.Zero(t, store.entries[0].sequence)
}

type scopedOutboundDecoratedStore struct {
	*BoundedStore
	saves      int
	resets     int
	resetError error
}

func (store *scopedOutboundDecoratedStore) SaveMessageAndIncrNextSenderMsgSeqNum(sequence int, wire []byte) error {
	store.saves++
	return store.BoundedStore.SaveMessageAndIncrNextSenderMsgSeqNum(sequence, wire)
}

func (store *scopedOutboundDecoratedStore) Reset() error {
	store.resets++
	if store.resetError != nil {
		return store.resetError
	}
	return store.BoundedStore.Reset()
}

func TestScopedOutboundStoreDecoratorKeepsPublicOverrides(t *testing.T) {
	current, _, base := newScopedOutboundLifecycleSession(t)
	decorated := &scopedOutboundDecoratedStore{BoundedStore: base}
	current.store = decorated
	require.NoError(t, current.configureOutbound(inboundIntegrationSettings(1, 1)))
	require.Nil(t, current.outbound.journal, "embedding a store does not authorize bypassing its public overrides")
	require.NoError(t, current.queueForSend(scopedOutboundLifecycleOrder("ordinary")))
	logon := NewMessage()
	logon.Header.SetString(tagMsgType, "A")
	logon.Body.SetBool(tagResetSeqNumFlag, true)
	require.NoError(t, current.queueForSend(logon))
	require.Equal(t, 2, decorated.saves)
	require.Equal(t, 1, decorated.resets)
	current.onDisconnect()
	assertScopedOutboundVacant(t, current)
}

func TestScopedOutboundOrdinaryFromAppCanSendSynchronously(t *testing.T) {
	current, application, store := newScopedOutboundLifecycleSession(t)
	require.NoError(t, registerSession(current))
	t.Cleanup(func() { require.NoError(t, UnregisterSession(current.sessionID)) })
	response := scopedOutboundLifecycleOrder("initial")
	var sendError error
	var observed *Message
	application.toApp = func(message *Message) error {
		observed = message
		return nil
	}
	application.fromApp = func(message *Message) MessageRejectError {
		id, reject := message.Body.GetBytes(11)
		if reject != nil {
			return reject
		}
		response.Body.SetBytes(11, id)
		sendError = SendToTarget(response, current.sessionID)
		return nil
	}
	factory := MessageFactory{}
	request := factory.NewOrderSingle()
	request.Body.SetString(11, "variable-request-id")
	wire := request.build()
	slot := inboundRingTestAcquire(t, current.boundedIn.ring)
	slot.frame = append(slot.frame[:0], wire...)
	done := make(chan struct{})
	go func() {
		current.Incoming(current, fixIn{slot: slot, receiveTime: time.Now()})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("FromApp -> ordinary SendToTarget did not return")
	}
	require.NoError(t, sendError)
	require.Same(t, response, observed)
	require.Equal(t, 2, store.NextTargetMsgSeqNum())
	require.Equal(t, 2, store.NextSenderMsgSeqNum())
	// Return ownership to the test before overwriting the recycled input.
	for range current.boundedIn.ring.slots {
		vacant := <-current.boundedIn.ring.vacant
		if vacant == slot {
			clear(vacant.frame[:cap(vacant.frame)])
		}
		current.boundedIn.ring.vacant <- vacant
	}
	connection := &scopedOutboundBenchmarkConnection{wire: make([]byte, 1024)}
	current.writeConnection = newOutboundWriter(connection, 0, time.Second, current.messageEvent)
	go current.writeConnection.run()
	current.sendMutex.Lock()
	current.sendQueued(true)
	current.sendMutex.Unlock()
	current.Disconnected(current)
	assertScopedOutboundVacant(t, current)
	received := NewMessage()
	require.NoError(t, ParseMessage(received, bytes.NewBuffer(connection.wire[:connection.used])))
	id, reject := received.Body.GetString(11)
	require.Nil(t, reject)
	require.Equal(t, "variable-request-id", id)
}

func TestScopedOutboundResendBeyondRingMakesProgress(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		depth    int
		fail     bool
		messages int
	}{
		{name: "unbuffered", messages: 5},
		{name: "buffered", depth: 2, messages: 5},
		{name: "writer_failure", fail: true, messages: 5},
		{name: "writer_failure_within_ring", fail: true, messages: 2},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			current, application, store := newScopedOutboundLifecycleSession(t)
			for sequence := 1; sequence <= scenario.messages; sequence++ {
				message := scopedOutboundLifecycleOrder(strconv.Itoa(sequence))
				current.fillDefaultHeader(message, nil)
				message.Header.SetInt(tagMsgSeqNum, sequence)
				require.NoError(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(sequence, message.build()))
			}
			var callbacks int
			application.toApp = func(message *Message) error {
				callbacks++
				message.Header.SetString(tagSenderSubID, "replayed")
				message.Body.SetString(11, "body callback edit is not replayed")
				return nil
			}
			connection := &scopedOutboundTestConnection{scopedOutboundBenchmarkConnection: scopedOutboundBenchmarkConnection{wire: make([]byte, 2048)}}
			if scenario.fail {
				connection.maxWrite, connection.failAt, connection.failure = 5, 2, errors.New("resend write failed")
			}
			current.writeConnection = newOutboundWriter(connection, scenario.depth, time.Second, current.messageEvent)
			go current.writeConnection.run()
			factory := MessageFactory{}
			request := factory.ResendRequest(1)
			done := make(chan sessionState, 1)
			go func() { done <- (inSession{}).FixMsgIn(current, request) }()
			var next sessionState
			select {
			case next = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("resend larger than the ring did not complete")
			}
			if scenario.fail {
				require.IsType(t, latentState{}, next)
				require.Equal(t, 1, store.NextTargetMsgSeqNum())
			} else {
				require.IsType(t, inSession{}, next)
				require.Equal(t, scenario.messages, callbacks)
				require.Equal(t, 2, store.NextTargetMsgSeqNum())
			}
			current.Disconnected(current)
			assertScopedOutboundVacant(t, current)
			require.Equal(t, scenario.messages+1, store.NextSenderMsgSeqNum())
			if scenario.fail {
				return
			}
			parser := newParser(bytes.NewReader(connection.wire[:connection.used]))
			for sequence := 1; sequence <= scenario.messages; sequence++ {
				wire, err := parser.ReadMessage()
				require.NoError(t, err)
				message := NewMessage()
				require.NoError(t, ParseMessage(message, wire))
				actual, reject := message.Header.GetInt(tagMsgSeqNum)
				require.Nil(t, reject)
				require.Equal(t, sequence, actual)
				id, reject := message.Body.GetString(11)
				require.Nil(t, reject)
				require.Equal(t, strconv.Itoa(sequence), id)
				subID, reject := message.Header.GetString(tagSenderSubID)
				require.Nil(t, reject)
				require.Equal(t, "replayed", subID)
				duplicate, reject := message.Header.GetBool(tagPossDupFlag)
				require.Nil(t, reject)
				require.True(t, duplicate)
				require.True(t, message.Header.Has(tagOrigSendingTime))
			}
		})
	}
}

func TestScopedOutboundLogoutCapacityPreservesStoreAndTarget(t *testing.T) {
	for _, resetOnLogout := range []bool{false, true} {
		t.Run(strconv.FormatBool(resetOnLogout), func(t *testing.T) {
			current, application, store := newScopedOutboundLifecycleSession(t)
			current.ResetOnLogout, current.ResetOnDisconnect = resetOnLogout, true
			require.NoError(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(1, []byte("retained before logout")))
			require.NoError(t, store.SetNextTargetMsgSeqNum(7))
			creation := store.CreationTime()
			application.toAdmin = func(message *Message) { message.Body.SetString(tagText, strings.Repeat("x", 512)) }
			factory := MessageFactory{}
			factory.SetNextSeqNum(7)
			next := (inSession{}).FixMsgIn(current, factory.Logout())
			require.IsType(t, latentState{}, next)
			require.True(t, current.preserveOnDisconnect)
			current.setState(current, next)
			assertScopedOutboundVacant(t, current)
			require.Equal(t, 2, store.NextSenderMsgSeqNum())
			require.Equal(t, 7, store.NextTargetMsgSeqNum())
			require.Equal(t, creation, store.CreationTime())
			retained, err := store.GetMessages(1, 1)
			require.NoError(t, err)
			require.Equal(t, [][]byte{[]byte("retained before logout")}, retained)
		})
	}
}

func TestScopedOutboundGapFillCapacityPreservesTargetWithoutPersistence(t *testing.T) {
	current, application, store := newScopedOutboundLifecycleSession(t)
	current.DisableMessagePersist, current.ResetOnDisconnect = true, true
	require.NoError(t, store.SetNextSenderMsgSeqNum(2))
	require.NoError(t, store.SetNextTargetMsgSeqNum(7))
	application.toAdmin = func(message *Message) { message.Body.SetString(tagText, strings.Repeat("x", 512)) }
	factory := MessageFactory{}
	factory.SetNextSeqNum(7)
	next := (inSession{}).FixMsgIn(current, factory.ResendRequest(1))
	require.IsType(t, latentState{}, next)
	require.True(t, current.preserveOnDisconnect)
	current.setState(current, next)
	assertScopedOutboundVacant(t, current)
	require.Equal(t, 2, store.NextSenderMsgSeqNum())
	require.Equal(t, 7, store.NextTargetMsgSeqNum())
}

func TestScopedOutboundSessionEndCapacityPreservesStore(t *testing.T) {
	current, application, store := newScopedOutboundLifecycleSession(t)
	current.ResetOnDisconnect = true
	now := time.Date(2026, time.October, 16, 12, 0, 0, 0, time.UTC)
	creation := now.Add(-2 * time.Hour)
	store.SetCreationTime(creation)
	require.NoError(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(1, []byte("retained at session end")))
	require.NoError(t, store.SetNextTargetMsgSeqNum(7))
	var err error
	current.SessionTime, err = internal.NewUTCTimeRange(internal.NewTimeOfDay(9, 0, 0), internal.NewTimeOfDay(11, 0, 0), nil)
	require.NoError(t, err)
	var callbacks int
	application.toAdmin = func(message *Message) {
		callbacks++
		message.Body.SetString(tagText, strings.Repeat("x", 512))
	}
	current.CheckSessionTime(current, now)
	require.IsType(t, notSessionTime{}, current.State)
	require.Equal(t, 1, callbacks)
	assertScopedOutboundVacant(t, current)
	current.CheckSessionTime(current, now.Add(time.Second))
	require.Equal(t, 1, callbacks, "remaining outside the session must not create another send")
	require.Equal(t, 2, store.NextSenderMsgSeqNum())
	require.Equal(t, 7, store.NextTargetMsgSeqNum())
	require.Equal(t, creation, store.CreationTime())
	retained, err := store.GetMessages(1, 1)
	require.NoError(t, err)
	require.Equal(t, [][]byte{[]byte("retained at session end")}, retained)
}

func TestScopedOutboundSessionRolloverCapacityPreservesStore(t *testing.T) {
	current, application, store := newScopedOutboundLifecycleSession(t)
	current.ResetOnDisconnect = true
	now := time.Date(2026, time.October, 16, 10, 0, 0, 0, time.UTC)
	creation := now.AddDate(0, 0, -1)
	store.SetCreationTime(creation)
	require.NoError(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(1, []byte("retained before rollover")))
	require.NoError(t, store.SetNextTargetMsgSeqNum(7))
	var err error
	current.SessionTime, err = internal.NewUTCTimeRange(internal.NewTimeOfDay(9, 0, 0), internal.NewTimeOfDay(11, 0, 0), nil)
	require.NoError(t, err)
	var callbacks int
	application.toAdmin = func(message *Message) {
		callbacks++
		message.Body.SetString(tagText, strings.Repeat("x", 512))
	}
	current.CheckSessionTime(current, now)
	require.Equal(t, 1, callbacks)
	require.False(t, current.IsConnected())
	assertScopedOutboundVacant(t, current)
	current.CheckSessionTime(current, now.Add(time.Second))
	current.CheckSessionTime(current, now.Add(2*time.Second))
	require.Equal(t, 1, callbacks)
	require.True(t, current.outbound.scheduledResetPending.Load())
	require.Equal(t, 2, store.NextSenderMsgSeqNum())
	require.Equal(t, 7, store.NextTargetMsgSeqNum())
	require.Equal(t, creation, store.CreationTime())
	retained, err := store.GetMessages(1, 1)
	require.NoError(t, err)
	require.Equal(t, [][]byte{[]byte("retained before rollover")}, retained)
}

func TestScopedOutboundScheduledResetCapacityRetainsRetryBoundary(t *testing.T) {
	current, application, store := newScopedOutboundLifecycleSession(t)
	current.ResetOnDisconnect = true
	boundary := time.Date(2026, time.October, 16, 10, 0, 0, 0, time.UTC)
	before, after := boundary.Add(-time.Second), boundary.Add(time.Second)
	current.EnableResetSeqTime = true
	current.ResetSeqTime = boundary
	current.lastCheckedResetSeqTime = before
	creation := store.CreationTime()
	require.NoError(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(1, []byte("retained before scheduled reset")))
	require.NoError(t, store.SetNextTargetMsgSeqNum(7))
	connection := &scopedOutboundBenchmarkConnection{wire: make([]byte, 1024)}
	current.writeConnection = newOutboundWriter(connection, 1, time.Second, current.messageEvent)
	go current.writeConnection.run()
	oversized := true
	var callbacks int
	application.toAdmin = func(message *Message) {
		callbacks++
		if oversized {
			message.Body.SetString(tagText, strings.Repeat("x", 512))
		}
	}
	for _, checkTime := range []time.Time{after, after.Add(time.Second)} {
		current.CheckResetTime(current, checkTime)
		require.True(t, current.IsConnected(), "failed scheduled reset must remain eligible for retry")
		require.Equal(t, before, current.lastCheckedResetSeqTime)
		require.Equal(t, 2, store.NextSenderMsgSeqNum())
		require.Equal(t, 7, store.NextTargetMsgSeqNum())
		require.Equal(t, creation, store.CreationTime())
		require.Zero(t, current.outbound.count)
		require.Len(t, current.outbound.vacant, len(current.outbound.slots))
		retained, err := store.GetMessages(1, 1)
		require.NoError(t, err)
		require.Equal(t, [][]byte{[]byte("retained before scheduled reset")}, retained)
	}
	require.Equal(t, 2, callbacks)
	oversized = false
	retry := after.Add(2 * time.Second)
	current.CheckResetTime(current, retry)
	require.Equal(t, 3, callbacks)
	require.Equal(t, retry, current.lastCheckedResetSeqTime)
	require.Equal(t, 2, store.NextSenderMsgSeqNum())
	require.Equal(t, 1, store.NextTargetMsgSeqNum())
	current.CheckResetTime(current, retry.Add(time.Second))
	require.Equal(t, 3, callbacks, "successful retry must consume the reset boundary exactly once")
	current.ResetOnDisconnect = false
	current.Disconnected(current)
	assertScopedOutboundVacant(t, current)
	message := NewMessage()
	require.NoError(t, ParseMessage(message, bytes.NewBuffer(connection.wire[:connection.used])))
	messageType, reject := message.Header.GetString(tagMsgType)
	require.Nil(t, reject)
	require.Equal(t, "A", messageType)
	reset, reject := message.Body.GetBool(tagResetSeqNumFlag)
	require.Nil(t, reject)
	require.True(t, reset)
}

func TestScopedOutboundRolloverResetSurvivesFailedNextLogon(t *testing.T) {
	for _, initiator := range []bool{false, true} {
		role := "acceptor"
		if initiator {
			role = "initiator"
		}
		for _, failure := range []string{"encode", "store_reset"} {
			t.Run(role+"/"+failure, func(t *testing.T) {
				current, application, store := newScopedOutboundLifecycleSession(t)
				decorated := &scopedOutboundDecoratedStore{BoundedStore: store}
				current.store = decorated
				require.NoError(t, current.configureOutbound(inboundIntegrationSettings(1, 1)))
				current.ResetOnDisconnect = true
				now := time.Date(2026, time.October, 16, 10, 0, 0, 0, time.UTC)
				creation := now.AddDate(0, 0, -1)
				store.SetCreationTime(creation)
				require.NoError(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(1, []byte("retained until reset commits")))
				require.NoError(t, store.SetNextTargetMsgSeqNum(7))
				var err error
				current.SessionTime, err = internal.NewUTCTimeRange(internal.NewTimeOfDay(9, 0, 0), internal.NewTimeOfDay(11, 0, 0), nil)
				require.NoError(t, err)
				oversized := true
				application.toAdmin = func(message *Message) {
					if oversized {
						message.Body.SetString(tagText, strings.Repeat("x", 512))
					}
				}
				current.CheckSessionTime(current, now)
				require.False(t, current.IsConnected())
				require.True(t, current.outbound.scheduledResetPending.Load())
				current.InitiateLogon = initiator
				if failure == "store_reset" {
					oversized = false
					decorated.resetError = errors.New("reset did not commit")
				}

				connectAttempt := func() *scopedOutboundBenchmarkConnection {
					connection := &scopedOutboundBenchmarkConnection{wire: make([]byte, 1024)}
					incoming := make(chan fixIn, 1)
					reader, err := startBoundedRead(connection, newParser(connection), current.boundedIn, incoming, nil, time.Time{})
					require.NoError(t, err)
					writer := newOutboundWriter(connection, 1, time.Second, current.messageEvent)
					go writer.run()
					current.onAdmin(connect{messageIn: incoming, reader: reader, writer: writer})
					if !initiator {
						factory := MessageFactory{}
						request := factory.Logon()
						request.Body.SetInt(tagHeartBtInt, 60)
						next := (logonState{}).FixMsgIn(current, request)
						current.setState(current, next)
					}
					return connection
				}
				failedConnection := connectAttempt()
				require.False(t, current.IsConnected())
				assertScopedOutboundVacant(t, current)
				assertInboundRingVacant(t, current.boundedIn.ring)
				require.Zero(t, failedConnection.used)
				require.True(t, current.outbound.scheduledResetPending.Load())
				current.CheckSessionTime(current, now.Add(time.Second))
				require.Equal(t, 2, store.NextSenderMsgSeqNum())
				require.Equal(t, 7, store.NextTargetMsgSeqNum())
				require.Equal(t, creation, store.CreationTime())
				retained, err := store.GetMessages(1, 1)
				require.NoError(t, err)
				require.Equal(t, [][]byte{[]byte("retained until reset commits")}, retained)

				oversized, decorated.resetError = false, nil
				completedConnection := connectAttempt()
				require.True(t, current.IsConnected())
				require.False(t, current.outbound.scheduledResetPending.Load())
				require.False(t, current.outbound.resetPending.Load())
				require.Equal(t, 2, store.NextSenderMsgSeqNum())
				if initiator {
					require.Equal(t, 1, store.NextTargetMsgSeqNum())
				} else {
					require.Equal(t, 2, store.NextTargetMsgSeqNum())
				}
				current.ResetOnDisconnect = false
				current.Disconnected(current)
				assertScopedOutboundVacant(t, current)
				message := NewMessage()
				require.NoError(t, ParseMessage(message, bytes.NewBuffer(completedConnection.wire[:completedConnection.used])))
				messageType, reject := message.Header.GetString(tagMsgType)
				require.Nil(t, reject)
				require.Equal(t, "A", messageType)
				sequence, reject := message.Header.GetInt(tagMsgSeqNum)
				require.Nil(t, reject)
				require.Equal(t, 1, sequence)
				reset, reject := message.Body.GetBool(tagResetSeqNumFlag)
				require.Nil(t, reject)
				require.True(t, reset)
			})
		}
	}
}

func TestScopedOutboundSuccessfulScheduleTransitionsRemainUnchanged(t *testing.T) {
	for _, rollover := range []bool{false, true} {
		name := "session_end"
		if rollover {
			name = "new_range"
		}
		t.Run(name, func(t *testing.T) {
			current, _, store := newScopedOutboundLifecycleSession(t)
			current.ResetOnDisconnect = true
			now := time.Date(2026, time.October, 16, 12, 0, 0, 0, time.UTC)
			creation := now.Add(-2 * time.Hour)
			if rollover {
				now = now.Add(-2 * time.Hour)
				creation = now.AddDate(0, 0, -1)
			}
			store.SetCreationTime(creation)
			require.NoError(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(1, []byte("old session history")))
			require.NoError(t, store.SetNextTargetMsgSeqNum(7))
			var err error
			current.SessionTime, err = internal.NewUTCTimeRange(internal.NewTimeOfDay(9, 0, 0), internal.NewTimeOfDay(11, 0, 0), nil)
			require.NoError(t, err)
			connection := &scopedOutboundBenchmarkConnection{wire: make([]byte, 1024)}
			current.writeConnection = newOutboundWriter(connection, 1, time.Second, current.messageEvent)
			go current.writeConnection.run()
			current.CheckSessionTime(current, now)
			if rollover {
				require.IsType(t, latentState{}, current.State)
			} else {
				require.IsType(t, notSessionTime{}, current.State)
			}
			assertScopedOutboundVacant(t, current)
			require.False(t, current.outbound.scheduledResetPending.Load())
			require.Equal(t, 1, store.NextSenderMsgSeqNum())
			require.Equal(t, 1, store.NextTargetMsgSeqNum())
			message := NewMessage()
			require.NoError(t, ParseMessage(message, bytes.NewBuffer(connection.wire[:connection.used])))
			messageType, reject := message.Header.GetString(tagMsgType)
			require.Nil(t, reject)
			require.Equal(t, "5", messageType)
			sequence, reject := message.Header.GetInt(tagMsgSeqNum)
			require.Nil(t, reject)
			require.Equal(t, 2, sequence, "Logout must retain its pre-reset sender sequence")
		})
	}
}

func TestScopedOutboundRetainedFramesRespectReplayLimits(t *testing.T) {
	for _, kind := range []string{"fields", "group_entries", "group_depth"} {
		t.Run(kind, func(t *testing.T) {
			current, _, store := newScopedOutboundLifecycleSession(t)
			settings := inboundIntegrationSettings(1, 1)
			settings.Set(config.OutboundFrameQueueCapacity, "2")
			settings.Set(config.BoundedMaxFields, "64")
			var accepted, rejected *Message
			var expected error
			if kind == "fields" {
				settings.Set(config.BoundedMaxFields, "10")
				accepted, rejected = scopedOutboundLifecycleOrder("accepted"), scopedOutboundLifecycleOrder("rejected")
				accepted.Body.SetString(12, "second")
				rejected.Body.SetString(12, "second")
				rejected.Body.SetString(13, "third")
				expected = ErrFieldCapacity
			} else {
				wire, dictionary := boundedOwnershipFixture(t, true)
				current.sessionID.BeginString = BeginStringFIX44
				current.transportDataDictionary, current.appDataDictionary = dictionary, dictionary
				accepted, rejected = NewMessage(), NewMessage()
				require.NoError(t, ParseMessageWithDataDictionary(accepted, bytes.NewBuffer(wire), dictionary, dictionary))
				require.NoError(t, ParseMessageWithDataDictionary(rejected, bytes.NewBuffer(wire), dictionary, dictionary))
				if kind == "group_entries" {
					settings.Set(config.BoundedMaxGroupEntries, "2")
					for index, message := range []*Message{accepted, rejected} {
						// Isolate the Parties count from the fixture's independent
						// NoTradingSessions group when testing the total entry limit.
						message.Body.Remove(386)
						subIDs := NewRepeatingGroup(802, GroupTemplate{GroupElement(523), GroupElement(803)})
						parties := NewRepeatingGroup(453, GroupTemplate{GroupElement(448), GroupElement(447), GroupElement(452), subIDs})
						party := parties.Add()
						party.SetString(448, "party")
						party.SetString(447, "D")
						party.SetInt(452, 1)
						for range index + 1 {
							subID := subIDs.Add()
							subID.SetString(523, "sub")
							subID.SetInt(803, 1)
						}
						party.SetGroup(subIDs)
						message.Body.SetGroup(parties)
					}
					expected = ErrGroupCapacity
				} else {
					settings.Set(config.BoundedMaxGroupDepth, "2")
					for index, message := range []*Message{accepted, rejected} {
						subIDs := NewRepeatingGroup(804, GroupTemplate{GroupElement(545), GroupElement(805)})
						parties := NewRepeatingGroup(539, GroupTemplate{GroupElement(524), GroupElement(525), GroupElement(538), subIDs})
						allocations := NewRepeatingGroup(78, GroupTemplate{GroupElement(79), parties})
						party := parties.Add()
						party.SetString(524, "nested")
						party.SetString(525, "D")
						party.SetInt(538, 1)
						if index != 0 {
							subID := subIDs.Add()
							subID.SetString(545, "sub")
							subID.SetInt(805, 1)
							party.SetGroup(subIDs)
						}
						allocation := allocations.Add()
						allocation.SetString(79, "allocation")
						allocation.SetGroup(parties)
						message.Body.SetGroup(allocations)
					}
					expected = ErrGroupDepth
				}
			}
			require.NoError(t, current.configureBoundedInbound(settings))
			require.NoError(t, current.configureOutbound(settings))
			require.NoError(t, store.SetNextTargetMsgSeqNum(7))
			require.NoError(t, current.queueForSend(accepted))
			retained, err := store.GetMessages(1, 1)
			require.NoError(t, err)
			require.Len(t, retained, 1)
			parser, err := NewMessageSlot(current.transportDataDictionary, current.appDataDictionary, current.boundedIn.limits)
			require.NoError(t, err)
			require.NoError(t, parser.Parse(retained[0]), "accepted frame must fit the same replay metadata")
			if kind == "fields" {
				require.Len(t, parser.Message().fields, 10)
			}
			creation, pending, vacant := store.CreationTime(), current.outbound.count, len(current.outbound.vacant)
			require.ErrorIs(t, current.queueForSend(rejected), expected)
			require.Equal(t, 2, store.NextSenderMsgSeqNum())
			require.Equal(t, 7, store.NextTargetMsgSeqNum())
			require.Equal(t, creation, store.CreationTime())
			require.Equal(t, pending, current.outbound.count)
			require.Len(t, current.outbound.vacant, vacant)
			after, err := store.GetMessages(1, 1)
			require.NoError(t, err)
			require.Equal(t, retained, after)
			current.onDisconnect()
			assertScopedOutboundVacant(t, current)
		})
	}
}

type scopedOutboundGracefulConnection struct {
	net.Conn
	writeStarted    chan struct{}
	allowWrite      chan struct{}
	readInterrupted chan struct{}
	closed          chan struct{}
	closeOnce       sync.Once
}

func (connection *scopedOutboundGracefulConnection) Write(wire []byte) (int, error) {
	close(connection.writeStarted)
	select {
	case <-connection.allowWrite:
		return connection.Conn.Write(wire)
	case <-connection.closed:
		return 0, net.ErrClosed
	}
}

func (connection *scopedOutboundGracefulConnection) SetReadDeadline(deadline time.Time) error {
	err := connection.Conn.SetReadDeadline(deadline)
	close(connection.readInterrupted)
	return err
}

func (connection *scopedOutboundGracefulConnection) Close() error {
	connection.closeOnce.Do(func() { close(connection.closed) })
	return connection.Conn.Close()
}

func TestScopedOutboundGracefulLogoutCompletesBeforeSocketClose(t *testing.T) {
	current, _, store := newScopedOutboundLifecycleSession(t)
	local, peer := net.Pipe()
	connection := &scopedOutboundGracefulConnection{Conn: local, writeStarted: make(chan struct{}),
		allowWrite: make(chan struct{}), readInterrupted: make(chan struct{}), closed: make(chan struct{})}
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = connection.Close()
		_ = peer.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Logout cleanup did not finish")
		}
	})
	incoming := make(chan fixIn, 1)
	reader, err := startBoundedRead(connection, newParser(connection), current.boundedIn, incoming, nil, time.Time{})
	require.NoError(t, err)
	current.messageIn, current.readConnection = incoming, reader
	writer := newOutboundWriter(connection, 0, time.Second, current.messageEvent)
	current.writeConnection = writer
	go writer.run()
	go func() {
		factory := MessageFactory{}
		next := (inSession{}).FixMsgIn(current, factory.Logout())
		current.setState(current, next)
		close(done)
	}()
	for _, reached := range []<-chan struct{}{connection.writeStarted, connection.readInterrupted} {
		select {
		case <-reached:
		case <-time.After(2 * time.Second):
			t.Fatal("Logout did not reach blocked write and read cancellation")
		}
	}
	select {
	case <-connection.closed:
		t.Fatal("socket closed before accepted Logout could complete")
	case <-done:
		t.Fatal("disconnect returned before accepted Logout completed")
	default:
	}
	close(connection.allowWrite)
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(2*time.Second)))
	wire, err := newParser(peer).ReadMessage()
	require.NoError(t, err)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("completed Logout did not release disconnect")
	}
	require.NoError(t, writer.err)
	assertScopedOutboundVacant(t, current)
	assertInboundRingVacant(t, current.boundedIn.ring)
	require.Equal(t, 2, store.NextSenderMsgSeqNum())
	require.Equal(t, 2, store.NextTargetMsgSeqNum())
	message := NewMessage()
	require.NoError(t, ParseMessage(message, wire))
	messageType, reject := message.Header.GetString(tagMsgType)
	require.Nil(t, reject)
	require.Equal(t, "5", messageType)
	select {
	case <-connection.closed:
	default:
		t.Fatal("completed disconnect did not close the socket")
	}
}
