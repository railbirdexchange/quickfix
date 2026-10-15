package quickfix

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/config"
	"github.com/quickfixgo/quickfix/internal"
	"github.com/stretchr/testify/require"
)

type boundedOutboundSessionApplication struct {
	*boundedOutboundTestApplication
	from func(*MessageSlot) MessageRejectError
}

func (application *boundedOutboundSessionApplication) FromAdminBorrowed(slot *MessageSlot, _ SessionID) MessageRejectError {
	if application.from != nil {
		return application.from(slot)
	}
	return nil
}

func (application *boundedOutboundSessionApplication) FromAppBorrowed(slot *MessageSlot, id SessionID) MessageRejectError {
	return application.FromAdminBorrowed(slot, id)
}

func boundedOutboundSessionTestInbound(t *testing.T, out *boundedOutboundSession) *boundedOutboundSessionApplication {
	t.Helper()
	s := out.session
	ring, err := newInboundRing(nil, nil, s.boundedIn.limits, 1, 1)
	require.NoError(t, err)
	validator, err := NewBoundedValidator(ValidatorSettings{}, nil, nil, s.boundedIn.limits.MaxFields)
	require.NoError(t, err)
	application := &boundedOutboundSessionApplication{boundedOutboundTestApplication: out.application.(*boundedOutboundTestApplication)}
	s.application, out.application = application, application
	s.boundedIn.ring, s.boundedIn.validator, s.boundedIn.application = ring, validator, application
	for index := range ring.slots {
		ring.slots[index].parsed.outbound = out
	}
	s.sessionEvent = make(chan internal.Event, 1)
	return application
}

func boundedOutboundSessionTestLogon(sequence int) *Message {
	message := NewMessage()
	message.Header.SetString(tagBeginString, BeginStringFIX44)
	message.Header.SetString(tagMsgType, "A")
	message.Header.SetInt(tagMsgSeqNum, sequence)
	message.Header.SetString(tagSenderCompID, "TARGET")
	message.Header.SetString(tagTargetCompID, "SENDER")
	message.Header.SetField(tagSendingTime, FIXUTCTimestamp{Time: time.Now().UTC(), Precision: Millis})
	message.Body.SetString(tagEncryptMethod, "0")
	message.Body.SetInt(tagHeartBtInt, 60)
	message.Body.SetBool(tagResetSeqNumFlag, true)
	return message
}

func TestBoundedOutboundDisconnectPreservesUnobservedPartialWriteFailure(t *testing.T) {
	out, _ := newBoundedOutboundTestController(t, 2, 1024, 4096, 8, 1024)
	boundedOutboundSessionTestInbound(t, out)
	require.NoError(t, out.store.SaveMessageAndIncrNextSenderMsgSeqNum(1, []byte("previously retained")))
	require.NoError(t, out.store.SetNextTargetMsgSeqNum(7))
	out.session.ResetOnDisconnect = true
	release := make(chan struct{})
	connection := &boundedOutboundTestConn{wire: make([]byte, 1024), writeError: io.ErrClosedPipe, errorBytes: 3, entered: make(chan struct{}, 1), release: release}
	writer := startBoundedOutboundTestWriter(t, out, connection)
	require.NoError(t, sendBoundedOutboundTestMessage(t, out, "committed-before-partial-write"))
	boundedConnectionTestWait(t, connection.entered, "partial socket write")
	before := snapshotBoundedStore(out.store)
	close(release)
	// The session loop has not read completed: teardown is the first observer
	// of the socket failure and must preserve the committed resend journal.
	out.session.onDisconnect()
	boundedConnectionTestWait(t, writer.done, "failed writer join")
	require.ErrorIs(t, writer.result, io.ErrClosedPipe)
	require.Equal(t, 3, connection.used)
	require.Equal(t, before, snapshotBoundedStore(out.store))
	require.Nil(t, out.writer)
	assertBoundedOutboundSlotsReturned(t, out)
	assertInboundRingVacant(t, out.session.boundedIn.ring)
}

func TestBoundedOutboundResetLogonUsesProspectiveTargetOne(t *testing.T) {
	for _, sequence := range []int{0, -1} {
		out, _ := newBoundedOutboundTestController(t, 2, 1024, 4096, 8, 1024)
		boundedOutboundSessionTestInbound(t, out)
		require.NoError(t, out.store.SaveMessageAndIncrNextSenderMsgSeqNum(1, []byte("retained")))
		require.NoError(t, out.store.SetNextTargetMsgSeqNum(9))
		before := snapshotBoundedStore(out.store)
		err := out.session.handleLogon(boundedOutboundSessionTestLogon(sequence))
		tooLow, ok := err.(targetTooLow)
		require.True(t, ok, "reset logon sequence %d: %v", sequence, err)
		require.Equal(t, targetTooLow{ReceivedTarget: sequence, ExpectedTarget: 1}, tooLow)
		require.False(t, out.resetPending)
		require.Equal(t, before, snapshotBoundedStore(out.store))
		assertBoundedOutboundSlotsReturned(t, out)
	}
}

func TestBoundedOutboundRejectedResetLogonDoesNotResetLogout(t *testing.T) {
	out, base := newBoundedOutboundTestController(t, 2, 1024, 4096, 8, 1024)
	boundedOutboundSessionTestInbound(t, out)
	retained := []byte("must survive rejected reset logon")
	require.NoError(t, out.store.SaveMessageAndIncrNextSenderMsgSeqNum(1, retained))
	require.NoError(t, out.store.SetNextTargetMsgSeqNum(9))
	creation := out.store.CreationTime()
	connection := &boundedOutboundTestConn{wire: make([]byte, 2048)}
	startBoundedOutboundTestWriter(t, out, connection)
	var sentTypes []string
	base.toAdmin = func(message *OutboundMessage) error {
		kind, reject := message.Message().Header.GetBytes(tagMsgType)
		require.Nil(t, reject)
		sentTypes = append(sentTypes, string(kind))
		return nil
	}
	message := boundedOutboundSessionTestLogon(1)
	message.Header.SetString(tagSenderCompID, "WRONG")
	next := (inSession{}).FixMsgIn(out.session, message)
	require.IsType(t, logoutState{}, next)
	completeBoundedOutboundTestWrite(t, out)
	require.Equal(t, []string{"5"}, sentTypes)
	require.False(t, out.resetPending)
	require.Equal(t, 3, out.store.NextSenderMsgSeqNum())
	require.Equal(t, 9, out.store.NextTargetMsgSeqNum())
	require.Equal(t, creation, out.store.CreationTime())
	previous, ok := out.store.message(1)
	require.True(t, ok)
	require.Equal(t, retained, previous)
	logout, ok := out.store.message(2)
	require.True(t, ok)
	parsed := NewMessage()
	require.NoError(t, ParseMessage(parsed, bytes.NewBuffer(logout)))
	sequence, reject := parsed.Header.GetInt(tagMsgSeqNum)
	require.Nil(t, reject)
	require.Equal(t, 2, sequence)
}

func TestBoundedOutboundCallbackScopeAndUnfinishedBuilder(t *testing.T) {
	for _, name := range []string{"discarded", "capacity error", "nested begin"} {
		t.Run(name, func(t *testing.T) {
			out, _ := newBoundedOutboundTestController(t, 2, 1024, 4096, 8, 1024)
			application := boundedOutboundSessionTestInbound(t, out)
			slot := inboundRingTestAcquire(t, out.session.boundedIn.ring)
			message := NewMessage()
			message.Header.SetString(tagBeginString, BeginStringFIX44)
			message.Header.SetString(tagMsgType, "D")
			message.Body.SetString(11, "incoming")
			inboundRingTestParse(t, slot, message.build())
			defer slot.release()
			_, err := slot.parsed.BeginOutbound([]byte("D"))
			require.ErrorIs(t, err, ErrBoundedOutboundScope)
			require.ErrorIs(t, slot.parsed.RetireOutboundBefore(1), ErrBoundedOutboundScope)
			before := snapshotBoundedStore(out.store)
			var borrowed *OutboundMessage
			application.from = func(current *MessageSlot) MessageRejectError {
				require.Same(t, slot.parsed, current)
				var err error
				borrowed, err = current.BeginOutbound([]byte("D"))
				require.NoError(t, err)
				switch name {
				case "discarded":
					require.NoError(t, borrowed.Body().SetString(11, "unfinished"))
				case "capacity error":
					require.ErrorIs(t, borrowed.Body().SetString(11, strings.Repeat("x", 4097)), ErrOutboundBuilderCapacity)
				case "nested begin":
					_, err := current.BeginOutbound([]byte("D"))
					require.ErrorIs(t, err, ErrBoundedOutboundBusy)
				}
				return nil // Deliberately leave the builder unfinished.
			}
			reject := out.session.fromCallback(slot.parsed.Message())
			if name == "discarded" {
				require.Nil(t, reject)
			} else {
				failure, ok := reject.(*boundedOutboundReject)
				require.True(t, ok)
				want := ErrOutboundBuilderCapacity
				if name == "nested begin" {
					want = ErrBoundedOutboundBusy
				}
				require.ErrorIs(t, failure.err, want)
			}
			require.Nil(t, out.activeSlot)
			require.False(t, borrowed.active)
			require.ErrorIs(t, borrowed.Send(), ErrBoundedOutboundScope)
			_, err = slot.parsed.BeginOutbound([]byte("D"))
			require.ErrorIs(t, err, ErrBoundedOutboundScope)
			require.Equal(t, before, snapshotBoundedStore(out.store))
			assertBoundedOutboundSlotsReturned(t, out)
			// A later callback receives a clean builder even after an unfinished
			// error; no old capability remains active between the two calls.
			application.from = func(current *MessageSlot) MessageRejectError {
				builder, err := current.BeginOutbound([]byte("D"))
				require.NoError(t, err)
				require.Nil(t, builder.Err())
				builder.Abort()
				return nil
			}
			require.Nil(t, out.session.fromCallback(slot.parsed.Message()))
		})
	}
}

func TestBoundedOutboundInitiatorResetOnLogonCapacityIsAtomic(t *testing.T) {
	for _, fixture := range []struct {
		name                    string
		builder, frame, journal int
		expand                  bool
	}{
		{"builder", 32, 1024, 1024, false},
		{"frame", 4096, 256, 1024, true},
		{"journal", 4096, 1024, 16, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			out, application := newBoundedOutboundTestController(t, 2, fixture.frame, fixture.builder, 1, fixture.journal)
			boundedOutboundSessionTestInbound(t, out)
			require.NoError(t, out.store.SaveMessageAndIncrNextSenderMsgSeqNum(1, []byte("retained")))
			require.NoError(t, out.store.SetNextTargetMsgSeqNum(7))
			before := snapshotBoundedStore(out.store)
			if fixture.expand {
				application.toAdmin = func(message *OutboundMessage) error {
					return message.Body().SetString(58, strings.Repeat("x", 512))
				}
			}
			s := out.session
			s.State = latentState{}
			s.InitiateLogon, s.ResetOnLogon, s.ResetOnDisconnect = true, true, true
			connection := &boundedOutboundTestConn{wire: make([]byte, 2048)}
			writer := startBoundedOutboundTestWriter(t, out, connection)
			readConnection := boundedConnectionTestPipe(t)
			messageIn := make(chan fixIn, 1)
			reader, err := startBoundedRead(readConnection, newParser(readConnection), s.boundedIn, messageIn, nil, time.Time{})
			require.NoError(t, err)
			t.Cleanup(func() {
				select {
				case <-reader.stop:
				default:
					reader.stopAndDrain(messageIn)
				}
			})
			messageOut := make(chan []byte, 1)
			s.onAdmin(connect{messageIn: messageIn, messageOut: messageOut, reader: reader, writer: writer})
			boundedConnectionTestWait(t, writer.done, "writer teardown after failed reset logon")
			boundedConnectionTestWait(t, reader.done, "reader teardown after failed reset logon")
			require.False(t, s.IsConnected())
			require.Nil(t, s.messageIn)
			require.Nil(t, s.messageOut)
			require.Nil(t, s.readConnection)
			require.Nil(t, out.writer)
			require.False(t, out.resetPending)
			require.False(t, out.builder.active)
			require.Zero(t, connection.calls)
			require.Equal(t, -1, out.store.reservedIndex)
			require.Equal(t, before, snapshotBoundedStore(out.store))
			_, open := <-messageOut
			require.False(t, open)
			assertBoundedOutboundSlotsReturned(t, out)
			assertInboundRingVacant(t, s.boundedIn.ring)
		})
	}
}

func TestBoundedOutboundInSessionLogonCapacityDoesNotSendLogout(t *testing.T) {
	out, application := newBoundedOutboundTestController(t, 2, 256, 4096, 1, 1024)
	boundedOutboundSessionTestInbound(t, out)
	require.NoError(t, out.store.SaveMessageAndIncrNextSenderMsgSeqNum(1, []byte("retained before reset logon")))
	require.NoError(t, out.store.SetNextTargetMsgSeqNum(7))
	before := snapshotBoundedStore(out.store)
	out.session.ResetOnDisconnect = true
	connection := &boundedOutboundTestConn{wire: make([]byte, 2048)}
	writer := startBoundedOutboundTestWriter(t, out, connection)
	var callbackTypes []string
	application.toAdmin = func(message *OutboundMessage) error {
		kind, reject := message.Message().Header.GetBytes(tagMsgType)
		require.Nil(t, reject)
		callbackTypes = append(callbackTypes, string(kind))
		return message.Body().SetString(58, strings.Repeat("x", 512))
	}
	next := (inSession{}).FixMsgIn(out.session, boundedOutboundSessionTestLogon(1))
	require.IsType(t, latentState{}, next)
	out.session.stateMachine.setState(out.session, next)
	boundedConnectionTestWait(t, writer.done, "writer join after operational logon failure")
	require.Equal(t, []string{"A"}, callbackTypes, "capacity failure must not trigger a second Logout send")
	require.Zero(t, connection.calls)
	require.False(t, out.resetPending)
	require.Equal(t, before, snapshotBoundedStore(out.store))
	assertBoundedOutboundSlotsReturned(t, out)
	assertInboundRingVacant(t, out.session.boundedIn.ring)
}

func TestBoundedOutboundToAdminCanCancelExplicitReset(t *testing.T) {
	out, application := newBoundedOutboundTestController(t, 2, 1024, 4096, 8, 1024)
	boundedOutboundSessionTestInbound(t, out)
	retained := []byte("keep previous epoch")
	require.NoError(t, out.store.SaveMessageAndIncrNextSenderMsgSeqNum(1, retained))
	require.NoError(t, out.store.SetNextTargetMsgSeqNum(7))
	creation := out.store.CreationTime()
	connection := &boundedOutboundTestConn{wire: make([]byte, 2048)}
	startBoundedOutboundTestWriter(t, out, connection)
	application.toAdmin = func(message *OutboundMessage) error { return message.Body().SetBool(tagResetSeqNumFlag, false) }
	builder, err := out.begin(msgTypeLogon)
	require.NoError(t, err)
	require.NoError(t, builder.Body().SetBool(tagResetSeqNumFlag, true))
	require.NoError(t, out.sendBuilder(nil, nil, 0))
	completeBoundedOutboundTestWrite(t, out)
	require.Equal(t, 3, out.store.NextSenderMsgSeqNum())
	require.Equal(t, 7, out.store.NextTargetMsgSeqNum())
	require.Equal(t, creation, out.store.CreationTime())
	previous, ok := out.store.message(1)
	require.True(t, ok)
	require.Equal(t, retained, previous)
	current, ok := out.store.message(2)
	require.True(t, ok)
	parsed := NewMessage()
	require.NoError(t, ParseMessage(parsed, bytes.NewBuffer(current)))
	sequence, reject := parsed.Header.GetInt(tagMsgSeqNum)
	require.Nil(t, reject)
	require.Equal(t, 2, sequence)
	flag, reject := parsed.Body.GetBool(tagResetSeqNumFlag)
	require.Nil(t, reject)
	require.False(t, flag)
}

func TestBoundedOutboundToAdminInvalidResetFlagIsAtomic(t *testing.T) {
	for _, initiallyReset := range []bool{false, true} {
		t.Run(strconv.FormatBool(initiallyReset), func(t *testing.T) {
			out, application := newBoundedOutboundTestController(t, 2, 1024, 4096, 8, 1024)
			boundedOutboundSessionTestInbound(t, out)
			startBoundedOutboundTestWriter(t, out, &boundedOutboundTestConn{wire: make([]byte, 2048)})
			require.NoError(t, out.store.SaveMessageAndIncrNextSenderMsgSeqNum(1, []byte("retained")))
			require.NoError(t, out.store.SetNextTargetMsgSeqNum(7))
			before := snapshotBoundedStore(out.store)
			application.toAdmin = func(message *OutboundMessage) error {
				return message.Body().SetString(tagResetSeqNumFlag, "invalid")
			}
			builder, err := out.begin(msgTypeLogon)
			require.NoError(t, err)
			require.NoError(t, builder.Body().SetBool(tagResetSeqNumFlag, initiallyReset))
			err = out.sendBuilder(nil, nil, 0)
			var reject MessageRejectError
			require.ErrorAs(t, err, &reject)
			require.Equal(t, rejectReasonIncorrectDataFormatForValue, reject.RejectReason())
			require.NotNil(t, reject.RefTagID())
			require.Equal(t, tagResetSeqNumFlag, *reject.RefTagID())
			require.Equal(t, before, snapshotBoundedStore(out.store))
			require.Equal(t, -1, out.store.reservedIndex)
			require.False(t, out.builder.active)
			assertBoundedOutboundSlotsReturned(t, out)
		})
	}
}

func TestBoundedOutboundToAdminCancelResetWithFullJournalIsAtomic(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(strconv.FormatBool(remove), func(t *testing.T) {
			out, application := newBoundedOutboundTestController(t, 2, 1024, 4096, 1, 1024)
			boundedOutboundSessionTestInbound(t, out)
			startBoundedOutboundTestWriter(t, out, &boundedOutboundTestConn{wire: make([]byte, 2048)})
			require.NoError(t, out.store.SaveMessageAndIncrNextSenderMsgSeqNum(1, []byte("retained")))
			require.NoError(t, out.store.SetNextTargetMsgSeqNum(7))
			before := snapshotBoundedStore(out.store)
			called := false
			application.toAdmin = func(message *OutboundMessage) error {
				called = true
				if remove {
					return message.Body().Remove(tagResetSeqNumFlag)
				}
				return message.Body().SetBool(tagResetSeqNumFlag, false)
			}
			builder, err := out.begin(msgTypeLogon)
			require.NoError(t, err)
			require.NoError(t, builder.Body().SetBool(tagResetSeqNumFlag, true))
			require.ErrorIs(t, out.sendBuilder(nil, nil, 0), ErrJournalFull)
			require.True(t, called, "explicit reset permits callback despite the old full journal")
			require.Equal(t, before, snapshotBoundedStore(out.store))
			require.Equal(t, -1, out.store.reservedIndex)
			require.False(t, out.builder.active)
			assertBoundedOutboundSlotsReturned(t, out)
		})
	}
}

type boundedOutboundSessionStoreWrapper struct{ MessageStore }

func TestBoundedOutboundConfigurationRequiresExplicitSupport(t *testing.T) {
	for _, name := range []string{"inbound", "application", "memory store", "store wrapper", "persistence"} {
		t.Run(name, func(t *testing.T) {
			out, _ := newBoundedOutboundTestController(t, 2, 1024, 4096, 8, 1024)
			boundedOutboundSessionTestInbound(t, out)
			s := out.session
			s.boundedOut = nil
			switch name {
			case "inbound":
				s.boundedIn = nil
			case "application":
				s.application = &inboundRecordingApplication{t: t}
			case "memory store":
				s.store = &memoryStore{}
			case "store wrapper":
				s.store = boundedOutboundSessionStoreWrapper{MessageStore: out.store}
			case "persistence":
				s.DisableMessagePersist = true
			}
			settings := NewSessionSettings()
			settings.Set(config.BoundedOutbound, "Y")
			require.ErrorIs(t, s.configureBoundedOutbound(settings, s.application), ErrBoundedOutboundConfiguration)
			require.Nil(t, s.boundedOut)
		})
	}
	for _, disabled := range []bool{false, true} {
		s := &session{}
		settings := NewSessionSettings()
		if disabled {
			settings.Set(config.BoundedOutbound, "N")
		}
		require.NoError(t, s.configureBoundedOutbound(settings, nil))
		require.Nil(t, s.boundedOut)
	}
}

func TestBoundedOutboundConfigurationLimitsAndRingCapabilities(t *testing.T) {
	for _, fixture := range []struct {
		key, value string
		expected   error
	}{
		{config.BoundedOutboundQueueDepth, "0", ErrInvalidOutboundLimits},
		{config.BoundedOutboundQueueDepth, "-1", ErrInvalidOutboundLimits},
		{config.BoundedOutboundQueueDepth, strconv.Itoa(int(^uint(0) >> 1)), ErrInvalidOutboundLimits},
		{config.BoundedWriteTimeout, "0s", ErrInvalidOutboundLimits},
		{config.BoundedWriteTimeout, "-1s", ErrInvalidOutboundLimits},
		{config.BoundedBuilderBytes, "0", ErrInvalidOutboundMessageLimits},
		{config.BoundedBuilderBytes, "-1", ErrInvalidOutboundMessageLimits},
		{config.BoundedBuilderBytes, strconv.Itoa(int(^uint(0) >> 1)), ErrInvalidOutboundMessageLimits},
		{config.BoundedOutboundQueueDepth, "invalid", nil},
		{config.BoundedWriteTimeout, "invalid", nil},
		{config.BoundedBuilderBytes, "invalid", nil},
		{config.BoundedOutbound, "invalid", nil},
	} {
		t.Run(fixture.key+"="+fixture.value, func(t *testing.T) {
			out, _ := newBoundedOutboundTestController(t, 2, 1024, 4096, 8, 1024)
			boundedOutboundSessionTestInbound(t, out)
			s := out.session
			s.boundedOut = nil
			settings := NewSessionSettings()
			settings.Set(config.BoundedOutbound, "Y")
			settings.Set(fixture.key, fixture.value)
			err := s.configureBoundedOutbound(settings, s.application)
			require.Error(t, err)
			if fixture.expected != nil {
				require.ErrorIs(t, err, fixture.expected)
			}
			require.Nil(t, s.boundedOut)
		})
	}
	out, _ := newBoundedOutboundTestController(t, 2, 1024, 4096, 8, 1024)
	boundedOutboundSessionTestInbound(t, out)
	s := out.session
	s.boundedOut = nil
	settings := NewSessionSettings()
	settings.Set(config.BoundedOutbound, "Y")
	settings.Set(config.BoundedOutboundQueueDepth, "3")
	settings.Set(config.BoundedBuilderBytes, "2048")
	settings.Set(config.BoundedWriteTimeout, "17ms")
	require.NoError(t, s.configureBoundedOutbound(settings, s.application))
	require.NotNil(t, s.boundedOut)
	require.Equal(t, 3, cap(s.boundedOut.ready))
	require.Equal(t, 4, cap(s.boundedOut.free))
	require.Equal(t, 2048, cap(s.boundedOut.builder.arena))
	require.Equal(t, 17*time.Millisecond, s.boundedOut.writeTimeout)
	for index := range s.boundedIn.ring.slots {
		slot := s.boundedIn.ring.slots[index].parsed
		require.Same(t, s.boundedOut, slot.outbound)
		_, err := slot.BeginOutbound([]byte("D"))
		require.ErrorIs(t, err, ErrBoundedOutboundScope)
	}
}
