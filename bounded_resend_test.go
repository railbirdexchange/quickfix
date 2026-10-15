package quickfix

import (
	"bytes"
	"testing"

	"github.com/quickfixgo/quickfix/datadictionary"
	"github.com/stretchr/testify/require"
)

func TestBoundedResendUnavailableRangeFailsBeforeBuilderOrWriter(t *testing.T) {
	store, err := NewBoundedStore(2, 1024)
	require.NoError(t, err)
	require.NoError(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(1, []byte("retained")))
	require.NoError(t, store.RetireBefore(2))
	// No builder, parser, application or writer is installed. Range preflight
	// must reject the request before touching any of those components.
	out := &boundedOutboundSession{store: store}
	for _, request := range [][2]int{{1, 1}, {2, 1}, {2, 2}} {
		require.ErrorIs(t, out.resendRange(request[0], request[1], nil), ErrJournalRangeUnavailable)
		require.Equal(t, 2, store.NextSenderMsgSeqNum())
		require.Equal(t, 1, store.NextTargetMsgSeqNum())
	}
}

func TestBoundedResendChecksStoredSequenceBeforeSending(t *testing.T) {
	store, err := NewBoundedStore(1, 1024)
	require.NoError(t, err)
	message := inboundIntegrationOrder(7, "wrong-sequence")
	require.NoError(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(1, message.build()))
	parser, err := NewMessageSlot(nil, nil, DefaultParseLimits())
	require.NoError(t, err)
	builder, err := NewOutboundMessage(64, 2048)
	require.NoError(t, err)
	out := &boundedOutboundSession{store: store, replay: parser, replayBuilder: builder}
	require.ErrorIs(t, out.resendRange(1, 1, nil), ErrInvalidJournalSequence)
	require.Equal(t, 2, store.NextSenderMsgSeqNum())
	require.Equal(t, 1, store.NextTargetMsgSeqNum())
	require.Nil(t, parser.Message())
	require.False(t, builder.active)
	retained, found := store.message(1)
	require.True(t, found)
	require.Equal(t, message.build(), retained)
}

func boundedResendCapture(t *testing.T, out *boundedOutboundSession, begin, end, count int, reply *Message) []*Message {
	t.Helper()
	connection := &boundedOutboundTestConn{wire: make([]byte, 32*1024), maxWrite: 7}
	writer := newBoundedWriteConnection(connection, out)
	out.writer = writer
	go writer.run()
	err := out.resendRange(begin, end, reply)
	writer.finish()
	out.writer = nil
	require.NoError(t, err)
	reader := newParser(bytes.NewReader(connection.wire[:connection.used]))
	messages := make([]*Message, count)
	for index := range messages {
		wire, err := reader.ReadMessage()
		require.NoError(t, err)
		require.NoError(t, validateFIXWireBodyLengthAndCheckSum(wire.Bytes()))
		message := NewMessage()
		require.NoError(t, ParseMessageWithDataDictionary(message, wire, out.session.transportDataDictionary, out.session.appDataDictionary))
		messages[index] = message
	}
	_, err = reader.ReadMessage()
	require.Error(t, err, "resend must emit exactly the expected frames")
	require.Len(t, out.free, cap(out.free), "completed writes must return every output slot")
	return messages
}

func boundedResendStoreMessage(t *testing.T, out *boundedOutboundSession, sequence int, message *Message) []byte {
	t.Helper()
	message.Header.SetInt(tagMsgSeqNum, sequence)
	wire := message.build()
	require.NoError(t, out.store.SaveMessageAndIncrNextSenderMsgSeqNum(sequence, wire))
	return append([]byte(nil), wire...)
}

func boundedResendHeartbeat(sequence int) *Message {
	message := inboundIntegrationOrder(sequence, "ignored")
	message.Header.SetBytes(tagMsgType, msgTypeHeartbeat)
	return message
}

func TestBoundedResendFullJournalPreservesGroupsAndCallbackHeaders(t *testing.T) {
	out, application := newBoundedOutboundTestController(t, 1, 2048, 8192, 3, 2048)
	dictionary, err := datadictionary.Parse("spec/FIX44.xml")
	require.NoError(t, err)
	out.session.transportDataDictionary, out.session.appDataDictionary = dictionary, dictionary
	out.session.sessionID.BeginString = BeginStringFIX44
	out.session.EnableLastMsgSeqNumProcessed = true
	out.replay, err = NewMessageSlot(dictionary, dictionary, out.session.boundedIn.limits)
	require.NoError(t, err)

	first := boundedResendStoreMessage(t, out, 1, boundedResendHeartbeat(1))
	order := inboundIntegrationNestedOrder()
	order.Header.SetString(tagSenderSubID, "original-route")
	order.Header.SetInt(tagLastMsgSeqNumProcessed, 17)
	originalSendingTime, err := order.Header.GetBytes(tagSendingTime)
	require.NoError(t, err)
	originalSendingTime = append([]byte(nil), originalSendingTime...)
	second := boundedResendStoreMessage(t, out, 2, order)
	third := boundedResendStoreMessage(t, out, 3, boundedResendHeartbeat(3))
	appCalls, adminCalls := 0, 0
	application.toApp = func(builder *OutboundMessage) error {
		appCalls++
		require.NoError(t, builder.Header().SetString(tagSenderSubID, "callback-route"))
		return builder.Body().SetString(55, "IGNORED-RESEND-BODY-EDIT")
	}
	application.toAdmin = func(builder *OutboundMessage) error {
		adminCalls++
		return nil
	}
	reply := NewMessage()
	reply.Header.SetInt(tagMsgSeqNum, 42)
	messages := boundedResendCapture(t, out, 1, 3, 3, reply)
	require.Equal(t, 1, appCalls, "accepted replay callback must not run twice")
	require.Equal(t, 2, adminCalls)
	require.Equal(t, 4, out.store.NextSenderMsgSeqNum())
	require.Equal(t, 1, out.store.NextTargetMsgSeqNum())
	for index, expected := range [][]byte{first, second, third} {
		stored, found := out.store.message(index + 1)
		require.True(t, found)
		require.Equal(t, expected, stored, "replay must not mutate retained bytes")
	}
	for index, expected := range [][2]int{{1, 2}, {3, 4}} {
		message := messages[index*2]
		sequence, err := message.Header.GetInt(tagMsgSeqNum)
		require.NoError(t, err)
		require.Equal(t, expected[0], sequence)
		end, err := message.Body.GetInt(tagNewSeqNo)
		require.NoError(t, err)
		require.Equal(t, expected[1], end)
		lastProcessed, err := message.Header.GetInt(tagLastMsgSeqNumProcessed)
		require.NoError(t, err)
		require.Equal(t, 42, lastProcessed)
	}
	replay := messages[1]
	sequence, err := replay.Header.GetInt(tagMsgSeqNum)
	require.NoError(t, err)
	require.Equal(t, 2, sequence)
	route, err := replay.Header.GetString(tagSenderSubID)
	require.NoError(t, err)
	require.Equal(t, "callback-route", route)
	lastProcessed, err := replay.Header.GetInt(tagLastMsgSeqNumProcessed)
	require.NoError(t, err)
	require.Equal(t, 17, lastProcessed, "replay keeps the original routing context")
	original, err := replay.Header.GetBytes(tagOrigSendingTime)
	require.NoError(t, err)
	require.Equal(t, originalSendingTime, original)
	possDup, err := replay.Header.GetBool(tagPossDupFlag)
	require.NoError(t, err)
	require.True(t, possDup)
	storedMessage := NewMessage()
	require.NoError(t, ParseMessageWithDataDictionary(storedMessage, bytes.NewBuffer(second), dictionary, dictionary))
	require.Equal(t, storedMessage.bodyBytes, replay.bodyBytes, "resend preserves original body and nested group order")
}

func TestBoundedResendVetoConsolidatesGapFill(t *testing.T) {
	out, application := newBoundedOutboundTestController(t, 1, 1024, 4096, 3, 1024)
	boundedResendStoreMessage(t, out, 1, boundedResendHeartbeat(1))
	boundedResendStoreMessage(t, out, 2, inboundIntegrationOrder(2, "veto"))
	boundedResendStoreMessage(t, out, 3, boundedResendHeartbeat(3))
	appCalls, adminCalls := 0, 0
	application.toApp = func(*OutboundMessage) error { appCalls++; return ErrDoNotSend }
	application.toAdmin = func(*OutboundMessage) error { adminCalls++; return nil }
	messages := boundedResendCapture(t, out, 1, 3, 1, nil)
	require.Equal(t, 1, appCalls)
	require.Equal(t, 1, adminCalls)
	sequence, err := messages[0].Header.GetInt(tagMsgSeqNum)
	require.NoError(t, err)
	require.Equal(t, 1, sequence)
	end, err := messages[0].Body.GetInt(tagNewSeqNo)
	require.NoError(t, err)
	require.Equal(t, 4, end)
	require.Equal(t, 4, out.store.NextSenderMsgSeqNum())
}

func TestBoundedResendKnownSparseTailGapFillsThroughRequestedEnd(t *testing.T) {
	out, _ := newBoundedOutboundTestController(t, 1, 1024, 4096, 1, 1024)
	boundedResendStoreMessage(t, out, 1, inboundIntegrationOrder(1, "retained"))
	require.NoError(t, out.store.SetNextSenderMsgSeqNum(5))
	messages := boundedResendCapture(t, out, 1, 4, 2, nil)
	sequence, err := messages[1].Header.GetInt(tagMsgSeqNum)
	require.NoError(t, err)
	require.Equal(t, 2, sequence)
	end, err := messages[1].Body.GetInt(tagNewSeqNo)
	require.NoError(t, err)
	require.Equal(t, 5, end)
	require.Equal(t, 5, out.store.NextSenderMsgSeqNum())
}

func TestBoundedResendPreservesEmptyStoredBody(t *testing.T) {
	out, application := newBoundedOutboundTestController(t, 1, 1024, 4096, 1, 1024)
	message := NewMessage()
	message.Header.SetString(tagBeginString, BeginStringFIX42)
	message.Header.SetString(tagMsgType, "U")
	message.Header.SetString(tagSenderCompID, "ISLD")
	message.Header.SetString(tagTargetCompID, "TW")
	message.Header.SetString(tagSendingTime, "20261015-12:00:00.000")
	boundedResendStoreMessage(t, out, 1, message)
	application.toApp = func(builder *OutboundMessage) error {
		return builder.Body().SetString(55, "MUST-NOT-APPEAR")
	}
	messages := boundedResendCapture(t, out, 1, 1, 1, nil)
	require.False(t, messages[0].Body.Has(55))
	require.Empty(t, messages[0].bodyBytes)
}

func boundedResendIncoming(out *boundedOutboundSession, message *Message) *Message {
	message.Header.SetString(tagBeginString, out.session.sessionID.BeginString)
	message.Header.SetString(tagSenderCompID, out.session.sessionID.TargetCompID)
	message.Header.SetString(tagTargetCompID, out.session.sessionID.SenderCompID)
	return message
}

func TestBoundedResendRequestRetiredRangeDoesNotAdvanceTarget(t *testing.T) {
	out, application := newBoundedOutboundTestController(t, 1, 1024, 4096, 1, 1024)
	boundedResendStoreMessage(t, out, 1, inboundIntegrationOrder(1, "acknowledged"))
	require.NoError(t, out.store.RetireBefore(2))
	out.session.ResetOnDisconnect = true
	adminCalls := 0
	application.toAdmin = func(*OutboundMessage) error { adminCalls++; return nil }
	factory := MessageFactory{}
	request := boundedResendIncoming(out, factory.ResendRequest(1))
	state := inSession{}.FixMsgIn(out.session, request)
	require.IsType(t, latentState{}, state)
	require.Equal(t, 0, adminCalls)
	require.Equal(t, 2, out.store.NextSenderMsgSeqNum())
	require.Equal(t, 1, out.store.NextTargetMsgSeqNum())
	require.True(t, out.session.preserveOnDisconnect)
	require.Empty(t, out.ready)
}

func TestBoundedLogoutJournalFullDoesNotResetOrAdvanceTarget(t *testing.T) {
	out, application := newBoundedOutboundTestController(t, 1, 1024, 4096, 1, 1024)
	retained := boundedResendStoreMessage(t, out, 1, inboundIntegrationOrder(1, "retain-on-failure"))
	out.session.ResetOnLogout = true
	out.session.ResetOnDisconnect = true
	connection := &boundedOutboundTestConn{wire: make([]byte, 1024)}
	startBoundedOutboundTestWriter(t, out, connection)
	adminCalls := 0
	application.toAdmin = func(*OutboundMessage) error { adminCalls++; return nil }
	factory := MessageFactory{}
	logout := boundedResendIncoming(out, factory.Logout())
	state := inSession{}.FixMsgIn(out.session, logout)
	require.IsType(t, latentState{}, state)
	require.Equal(t, 0, adminCalls, "journal reservation fails before outbound callback")
	require.Equal(t, 2, out.store.NextSenderMsgSeqNum())
	require.Equal(t, 1, out.store.NextTargetMsgSeqNum())
	require.True(t, out.session.preserveOnDisconnect)
	wire, found := out.store.message(1)
	require.True(t, found)
	require.Equal(t, retained, wire)
	require.Empty(t, out.ready)
	require.Len(t, out.free, cap(out.free))
}
