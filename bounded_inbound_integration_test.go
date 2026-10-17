package quickfix

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/config"
	"github.com/quickfixgo/quickfix/datadictionary"
	"github.com/stretchr/testify/require"
)

type inboundCallbackRecord struct {
	sequence int
	id       string
}

// These recorders copy values so the same helper can exercise both ordinary
// owned delivery and explicit BorrowApplication delivery.
type inboundRecordingApplication struct {
	t             *testing.T
	borrowed      []inboundCallbackRecord
	borrowedAdmin []int
	received      []time.Time
	fromApp       func(*Message) MessageRejectError
	toAdmin       func(*Message)
	resends       [][2]int
	logouts       int
}

func (*inboundRecordingApplication) OnCreate(SessionID) {}
func (*inboundRecordingApplication) OnLogon(SessionID)  {}
func (a *inboundRecordingApplication) OnLogout(SessionID) {
	a.logouts++
}
func (a *inboundRecordingApplication) ToAdmin(message *Message, _ SessionID) {
	if a.toAdmin != nil {
		a.toAdmin(message)
	}
	messageType, err := message.Header.GetString(tagMsgType)
	require.Nil(a.t, err)
	if messageType == string(msgTypeResendRequest) {
		begin, err := message.Body.GetInt(tagBeginSeqNo)
		require.Nil(a.t, err)
		end, err := message.Body.GetInt(tagEndSeqNo)
		require.Nil(a.t, err)
		a.resends = append(a.resends, [2]int{begin, end})
	}
}
func (*inboundRecordingApplication) ToApp(*Message, SessionID) error { return nil }
func (a *inboundRecordingApplication) FromApp(message *Message, _ SessionID) MessageRejectError {
	require.NotNil(a.t, message)
	sequence, err := message.Header.GetInt(tagMsgSeqNum)
	require.Nil(a.t, err)
	id, err := message.Body.GetString(11)
	require.Nil(a.t, err)
	a.borrowed = append(a.borrowed, inboundCallbackRecord{sequence, id})
	a.received = append(a.received, message.ReceiveTime)
	if a.fromApp != nil {
		return a.fromApp(message)
	}
	return nil
}
func (a *inboundRecordingApplication) FromAdmin(message *Message, _ SessionID) MessageRejectError {
	sequence, err := message.Header.GetInt(tagMsgSeqNum)
	require.Nil(a.t, err)
	a.borrowedAdmin = append(a.borrowedAdmin, sequence)
	return nil
}

type inboundLegacyLog struct {
	incoming []string
}

func (l *inboundLegacyLog) OnIncoming(value []byte) { l.incoming = append(l.incoming, string(value)) }
func (*inboundLegacyLog) OnOutgoing([]byte)         {}
func (*inboundLegacyLog) OnEvent(string)            {}
func (*inboundLegacyLog) OnEventf(string, ...interface{}) {
}

type inboundRecordingLog struct {
	inboundLegacyLog
	borrowed        []string
	messageArgument interface{}
}

func (l *inboundRecordingLog) OnIncomingBorrowed(value []byte) {
	l.borrowed = append(l.borrowed, string(value))
}
func (*inboundRecordingLog) OnOutgoingBorrowed([]byte) {}
func (l *inboundRecordingLog) OnEventf(format string, arguments ...interface{}) {
	if format == "Invalid Session State: Received Msg %s while waiting for Logon" {
		l.messageArgument = arguments[0]
	}
}

func inboundIntegrationSettings(queue, stash int) *SessionSettings {
	settings := NewSessionSettings()
	settings.Set(config.InChanCapacity, strconv.Itoa(queue))
	settings.Set(config.BoundedInboundStashCapacity, strconv.Itoa(stash))
	settings.Set(config.BoundedMaxFrameBytes, "1024")
	settings.Set(config.BoundedMaxFields, "32")
	settings.Set(config.BoundedMaxGroupEntries, "8")
	settings.Set(config.BoundedMaxGroupDepth, "4")
	return settings
}

func newInboundIntegrationSession(t *testing.T, queue, stash int) (*session, *inboundRecordingApplication, *inboundRecordingLog, *memoryStore) {
	t.Helper()
	application := &inboundRecordingApplication{t: t}
	log := &inboundRecordingLog{}
	store := &memoryStore{}
	require.NoError(t, store.Reset())
	s := &session{
		sessionID:   SessionID{BeginString: BeginStringFIX42, SenderCompID: "ISLD", TargetCompID: "TW"},
		application: application,
		log:         log,
		store:       store,
		messageOut:  make(chan []byte, 64),
	}
	s.State = inSession{}
	s.InChanCapacity = queue
	s.MaxLatency = 120 * time.Second
	s.HeartBtInt = time.Minute
	s.Validator = NewValidator(ValidatorSettings{}, nil, nil)
	require.NoError(t, s.configureBoundedInbound(inboundIntegrationSettings(queue, stash)))
	t.Cleanup(func() {
		if s.IsConnected() {
			s.Disconnected(s)
		}
	})
	return s, application, log, store
}

func inboundIntegrationOrder(sequence int, id string) *Message {
	factory := MessageFactory{}
	factory.SetNextSeqNum(sequence)
	message := factory.NewOrderSingle()
	message.Body.SetString(11, id)
	return message
}

func inboundIntegrationNestedOrder() *Message {
	message := inboundIntegrationOrder(3, "nested")
	message.Header.SetString(tagBeginString, BeginStringFIX44)
	subIDs := NewRepeatingGroup(804, GroupTemplate{GroupElement(545)})
	parties := NewRepeatingGroup(539, GroupTemplate{GroupElement(524), subIDs})
	allocations := NewRepeatingGroup(78, GroupTemplate{GroupElement(79), parties})
	subIDs.Add().SetString(545, "sub-id")
	party := parties.Add()
	party.SetString(524, "nested-party")
	party.SetGroup(subIDs)
	allocation := allocations.Add()
	allocation.SetString(79, "allocation")
	allocation.SetGroup(parties)
	message.Body.SetGroup(allocations)
	return message
}

func inboundIntegrationConfigureGroupLimit(t *testing.T, s *session, key string) {
	t.Helper()
	dictionary, err := datadictionary.Parse("spec/FIX44.xml")
	require.NoError(t, err)
	assertInboundRingVacant(t, s.boundedIn.ring)
	s.sessionID.BeginString = BeginStringFIX44
	s.appDataDictionary, s.transportDataDictionary = dictionary, dictionary
	s.Validator = NewValidator(ValidatorSettings{}, dictionary, dictionary)
	settings := inboundIntegrationSettings(0, 1)
	settings.Set(key, "2")
	require.NoError(t, s.configureBoundedInbound(settings))
}

func inboundIntegrationDeliver(t *testing.T, s *session, message *Message) {
	t.Helper()
	inboundIntegrationDeliverAt(t, s, message, time.Now())
}

func inboundIntegrationDeliverAt(t *testing.T, s *session, message *Message, received time.Time) {
	t.Helper()
	wire := message.build()
	slot := inboundRingTestAcquire(t, s.boundedIn.ring)
	require.LessOrEqual(t, len(wire), cap(slot.frame))
	slot.frame = slot.frame[:len(wire)]
	copy(slot.frame, wire)
	slot.receiveTime = received
	s.Incoming(s, fixIn{slot: slot, receiveTime: received})
}

func TestBoundedInboundSessionReplay(t *testing.T) {
	s, application, log, store := newInboundIntegrationSession(t, 1, 3)
	for _, sequence := range []int{2, 3, 4} {
		inboundIntegrationDeliver(t, s, inboundIntegrationOrder(sequence, strconv.Itoa(sequence)))
	}
	require.Equal(t, 1, store.NextTargetMsgSeqNum())
	require.Empty(t, application.borrowed)
	require.Len(t, s.boundedIn.ring.vacant, len(s.boundedIn.ring.slots)-3)
	inboundIntegrationDeliver(t, s, inboundIntegrationOrder(1, "1"))
	require.Equal(t, []inboundCallbackRecord{{1, "1"}, {2, "2"}, {3, "3"}, {4, "4"}}, application.borrowed)
	require.Equal(t, 5, store.NextTargetMsgSeqNum())
	require.IsType(t, inSession{}, s.State)
	require.Len(t, log.borrowed, 4, "replay must not log the same incoming frame again")
	assertInboundRingVacant(t, s.boundedIn.ring)
}

func TestBoundedInboundSessionDuplicateReplacement(t *testing.T) {
	s, application, _, store := newInboundIntegrationSession(t, 0, 1)
	inboundIntegrationDeliver(t, s, inboundIntegrationOrder(2, "original"))
	inboundIntegrationDeliver(t, s, inboundIntegrationOrder(2, "replacement"))
	require.Len(t, s.boundedIn.ring.vacant, len(s.boundedIn.ring.slots)-1)
	inboundIntegrationDeliver(t, s, inboundIntegrationOrder(1, "first"))
	require.Equal(t, []inboundCallbackRecord{{1, "first"}, {2, "replacement"}}, application.borrowed)
	require.Equal(t, 3, store.NextTargetMsgSeqNum())
	assertInboundRingVacant(t, s.boundedIn.ring)
}

func TestBoundedInboundSessionGapFillDiscard(t *testing.T) {
	s, application, _, store := newInboundIntegrationSession(t, 1, 2)
	inboundIntegrationDeliver(t, s, inboundIntegrationOrder(3, "discard-three"))
	inboundIntegrationDeliver(t, s, inboundIntegrationOrder(4, "discard-four"))
	factory := MessageFactory{}
	gapFill := factory.SequenceReset(5)
	gapFill.Body.SetBool(tagGapFillFlag, true)
	inboundIntegrationDeliver(t, s, gapFill)
	require.Equal(t, 5, store.NextTargetMsgSeqNum())
	require.Empty(t, application.borrowed)
	require.Equal(t, []int{1}, application.borrowedAdmin)
	assertInboundRingVacant(t, s.boundedIn.ring)
	inboundIntegrationDeliver(t, s, inboundIntegrationOrder(5, "after-gap"))
	require.Equal(t, []inboundCallbackRecord{{5, "after-gap"}}, application.borrowed)
}

func TestBoundedInboundSessionChunkedRecovery(t *testing.T) {
	s, application, _, store := newInboundIntegrationSession(t, 1, 2)
	s.ResendRequestChunkSize = 2
	inboundIntegrationDeliver(t, s, inboundIntegrationOrder(5, "5"))
	s.State = pendingTimeout{s.State}
	inboundIntegrationDeliver(t, s, inboundIntegrationOrder(6, "6"))
	for sequence := 1; sequence <= 4; sequence++ {
		inboundIntegrationDeliver(t, s, inboundIntegrationOrder(sequence, strconv.Itoa(sequence)))
	}
	require.Equal(t, [][2]int{{1, 2}, {3, 0}}, application.resends)
	require.Equal(t, []inboundCallbackRecord{{1, "1"}, {2, "2"}, {3, "3"}, {4, "4"}, {5, "5"}, {6, "6"}}, application.borrowed)
	require.Equal(t, 7, store.NextTargetMsgSeqNum())
	assertInboundRingVacant(t, s.boundedIn.ring)
}

func TestBoundedInboundSessionPendingTimeoutExtendedRange(t *testing.T) {
	s, application, _, store := newInboundIntegrationSession(t, 1, 2)
	inboundIntegrationDeliver(t, s, inboundIntegrationOrder(5, "5"))
	s.State = pendingTimeout{s.State}
	inboundIntegrationDeliver(t, s, inboundIntegrationOrder(10, "10"))
	require.Len(t, application.resends, 1, "a timeout wrapper must preserve the active recovery")
	for sequence := 1; sequence <= 4; sequence++ {
		inboundIntegrationDeliver(t, s, inboundIntegrationOrder(sequence, strconv.Itoa(sequence)))
	}
	require.Equal(t, 6, store.NextTargetMsgSeqNum())
	require.Equal(t, [][2]int{{1, 0}, {6, 0}}, application.resends)
	state, recovering := boundedResendState(s.State)
	require.True(t, recovering)
	require.Equal(t, 9, state.resendRangeEnd)
	require.Len(t, s.boundedIn.ring.vacant, len(s.boundedIn.ring.slots)-1)
	for sequence := 6; sequence <= 9; sequence++ {
		inboundIntegrationDeliver(t, s, inboundIntegrationOrder(sequence, strconv.Itoa(sequence)))
	}
	require.Len(t, application.borrowed, 10)
	for index, record := range application.borrowed {
		require.Equal(t, inboundCallbackRecord{index + 1, strconv.Itoa(index + 1)}, record)
	}
	require.Equal(t, 11, store.NextTargetMsgSeqNum())
	require.IsType(t, inSession{}, s.State)
	assertInboundRingVacant(t, s.boundedIn.ring)
}

func TestBoundedInboundSessionMalformedRetainedFrameReleasedOnce(t *testing.T) {
	s, _, _, _ := newInboundIntegrationSession(t, 0, 2)
	inboundIntegrationDeliver(t, s, inboundIntegrationOrder(2, "first-stash"))
	malformed := inboundIntegrationOrder(3, "current-stash")
	malformed.Body.SetString(tagGapFillFlag, "X")
	inboundIntegrationDeliver(t, s, malformed)
	require.False(t, s.IsConnected())
	assertInboundRingVacant(t, s.boundedIn.ring)
}

func TestBoundedInboundSessionCapacityPreservesStore(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, *session)
		fail    func(*testing.T, *session)
	}{
		{
			name: "stash",
			prepare: func(t *testing.T, s *session) {
				inboundIntegrationDeliver(t, s, inboundIntegrationOrder(4, "retained"))
			},
			fail: func(t *testing.T, s *session) {
				inboundIntegrationDeliver(t, s, inboundIntegrationOrder(5, "overflow"))
			},
		},
		{
			name: "fields",
			fail: func(t *testing.T, s *session) {
				message := inboundIntegrationOrder(3, "too-many-fields")
				for tag := Tag(1000); tag < 1032; tag++ {
					message.Body.SetString(tag, "x")
				}
				inboundIntegrationDeliver(t, s, message)
			},
		},
		{
			name: "group-entries",
			prepare: func(t *testing.T, s *session) {
				inboundIntegrationConfigureGroupLimit(t, s, config.BoundedMaxGroupEntries)
			},
			fail: func(t *testing.T, s *session) {
				inboundIntegrationDeliver(t, s, inboundIntegrationNestedOrder())
			},
		},
		{
			name: "group-depth",
			prepare: func(t *testing.T, s *session) {
				inboundIntegrationConfigureGroupLimit(t, s, config.BoundedMaxGroupDepth)
			},
			fail: func(t *testing.T, s *session) {
				inboundIntegrationDeliver(t, s, inboundIntegrationNestedOrder())
			},
		},
		{
			name: "frame-read",
			fail: func(_ *testing.T, s *session) {
				s.Incoming(s, fixIn{readError: ErrFrameCapacity})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, application, _, store := newInboundIntegrationSession(t, 0, 1)
			s.ResetOnDisconnect = true
			require.NoError(t, store.SetNextSenderMsgSeqNum(7))
			require.NoError(t, store.SetNextTargetMsgSeqNum(3))
			require.NoError(t, store.SaveMessage(6, []byte("existing-persistence")))
			if test.prepare != nil {
				test.prepare(t, s)
			}
			beforeSender, beforeTarget := store.NextSenderMsgSeqNum(), store.NextTargetMsgSeqNum()
			beforeCreation := store.CreationTime()
			beforeMessages := make(map[int]string, len(store.messageMap))
			for sequence, wire := range store.messageMap {
				beforeMessages[sequence] = string(wire)
			}
			beforeResends := len(application.resends)
			test.fail(t, s)
			require.False(t, s.IsConnected())
			require.Equal(t, beforeSender, store.NextSenderMsgSeqNum())
			require.Equal(t, beforeTarget, store.NextTargetMsgSeqNum())
			require.Equal(t, beforeCreation, store.CreationTime())
			require.Len(t, store.messageMap, len(beforeMessages))
			for sequence, wire := range beforeMessages {
				require.Equal(t, wire, string(store.messageMap[sequence]))
			}
			require.Len(t, application.resends, beforeResends)
			require.Empty(t, application.borrowed)
			assertInboundRingVacant(t, s.boundedIn.ring)
			// The capacity exception applies only to this disconnect. A later
			// ordinary disconnect must still honor the configured reset policy.
			s.State = inSession{}
			s.messageOut = make(chan []byte, 64)
			s.Disconnected(s)
			require.Equal(t, 1, store.NextSenderMsgSeqNum())
			require.Equal(t, 1, store.NextTargetMsgSeqNum())
			require.Empty(t, store.messageMap)
		})
	}
	t.Run("zero-stash", func(t *testing.T) {
		s, application, _, store := newInboundIntegrationSession(t, 0, 0)
		s.ResetOnDisconnect = true
		inboundIntegrationDeliver(t, s, inboundIntegrationOrder(2, "no-capacity"))
		require.False(t, s.IsConnected())
		require.Equal(t, 1, store.NextSenderMsgSeqNum())
		require.Equal(t, 1, store.NextTargetMsgSeqNum())
		require.Empty(t, store.messageMap)
		require.Empty(t, application.resends)
		assertInboundRingVacant(t, s.boundedIn.ring)
	})
}

func TestBoundedInboundSessionCallbackLifetimes(t *testing.T) {
	s, application, log, _ := newInboundIntegrationSession(t, 0, 0)
	s.application = BorrowApplication(application)
	application.fromApp = func(message *Message) MessageRejectError {
		require.Same(t, s.boundedIn.parsed.Message(), message)
		require.NotNil(t, message.inbound)
		require.Len(t, s.boundedIn.ring.vacant, len(s.boundedIn.ring.slots)-1)
		return nil
	}
	for sequence := 1; sequence <= 4; sequence++ {
		inboundIntegrationDeliver(t, s, inboundIntegrationOrder(sequence, strconv.Itoa(sequence)))
		require.Nil(t, s.boundedIn.parsed.Message(), "scratch remains live after complete session processing")
		require.Nil(t, s.boundedIn.parsed.message.rawMessage)
	}
	factory := MessageFactory{}
	factory.SetNextSeqNum(5)
	inboundIntegrationDeliver(t, s, factory.Heartbeat())
	require.Equal(t, []inboundCallbackRecord{{1, "1"}, {2, "2"}, {3, "3"}, {4, "4"}}, application.borrowed)
	require.Equal(t, []int{5}, application.borrowedAdmin)
	require.Empty(t, log.incoming)
	require.Len(t, log.borrowed, 5)
	require.Contains(t, log.borrowed[0], "11=1\x01")
	assertInboundRingVacant(t, s.boundedIn.ring)
}

func TestScopedInboundRejectKeepsMessageUntilReplyCompletes(t *testing.T) {
	s, application, _, store := newInboundIntegrationSession(t, 0, 0)
	s.application = BorrowApplication(application)
	callbackReturned, replyObserved := false, false
	application.fromApp = func(*Message) MessageRejectError {
		callbackReturned = true
		return NewMessageRejectError("application reject", rejectReasonValueIsIncorrect, nil)
	}
	application.toAdmin = func(reply *Message) {
		require.True(t, callbackReturned)
		active := s.boundedIn.parsed.Message()
		require.NotNil(t, active, "callback return released scratch before reject construction")
		sequence, reject := active.Header.GetInt(tagMsgSeqNum)
		require.Nil(t, reject)
		require.Equal(t, 1, sequence)
		reference, reject := reply.Body.GetInt(tagRefSeqNum)
		require.Nil(t, reject)
		require.Equal(t, 1, reference)
		replyObserved = true
	}
	inboundIntegrationDeliver(t, s, inboundIntegrationOrder(1, "reject-me"))
	require.True(t, replyObserved)
	require.Equal(t, 2, store.NextTargetMsgSeqNum())
	require.Nil(t, s.boundedIn.parsed.Message())
	assertInboundRingVacant(t, s.boundedIn.ring)
}

type inboundRetainingLog struct {
	inboundLegacyLog
	frames [][]byte
}

func (log *inboundRetainingLog) OnIncoming(frame []byte) {
	log.frames = append(log.frames, frame)
}

func TestScopedInboundOrdinaryLoggerReceivesOwnedBytes(t *testing.T) {
	s, _, _, _ := newInboundIntegrationSession(t, 0, 0)
	log := &inboundRetainingLog{}
	s.log = log
	require.NoError(t, s.configureBoundedInbound(inboundIntegrationSettings(0, 0)))
	require.Nil(t, s.boundedIn.log)
	var expected []string
	for sequence := 1; sequence <= 4; sequence++ {
		message := inboundIntegrationOrder(sequence, strconv.Itoa(sequence))
		expected = append(expected, string(message.build()))
		inboundIntegrationDeliver(t, s, message)
	}
	for range s.boundedIn.ring.slots {
		slot := inboundRingTestAcquire(t, s.boundedIn.ring)
		for index := range slot.frame[:cap(slot.frame)] {
			slot.frame[:cap(slot.frame)][index] = 'X'
		}
		slot.release()
	}
	require.Len(t, log.frames, len(expected))
	for index, frame := range log.frames {
		require.Equal(t, expected[index], string(frame), "ordinary logger retained a recycled frame")
	}
	assertInboundRingVacant(t, s.boundedIn.ring)
}

func TestScopedInboundReplayPreservesReceiveTime(t *testing.T) {
	s, application, log, _ := newInboundIntegrationSession(t, 0, 2)
	first := time.Date(2026, time.October, 15, 12, 0, 0, 123, time.UTC)
	replacement, missing := first.Add(time.Second), first.Add(2*time.Second)
	inboundIntegrationDeliverAt(t, s, inboundIntegrationOrder(2, "original"), first)
	inboundIntegrationDeliverAt(t, s, inboundIntegrationOrder(2, "replacement"), replacement)
	inboundIntegrationDeliverAt(t, s, inboundIntegrationOrder(1, "missing"), missing)
	require.Equal(t, []inboundCallbackRecord{{1, "missing"}, {2, "replacement"}}, application.borrowed)
	require.Equal(t, []time.Time{missing, replacement}, application.received)
	require.Len(t, log.borrowed, 3, "replay must not duplicate incoming logging")
	assertInboundRingVacant(t, s.boundedIn.ring)
}

func TestBoundedInboundSessionDiagnosticLoggerRetainsOwnedArgument(t *testing.T) {
	s, application, log, _ := newInboundIntegrationSession(t, 0, 0)
	s.State = logonState{}
	message := inboundIntegrationOrder(1, "invalid-before-logon")
	wire := string(message.build())
	inboundIntegrationDeliver(t, s, message)
	require.False(t, s.IsConnected())
	argument, owned := log.messageArgument.(string)
	require.True(t, owned, "legacy event logger must receive an owned string, not a borrowed Message")
	require.Equal(t, wire, argument)
	assertInboundRingVacant(t, s.boundedIn.ring)

	s.State = inSession{}
	s.messageOut = make(chan []byte, 64)
	for sequence := 1; sequence <= 3; sequence++ {
		inboundIntegrationDeliver(t, s, inboundIntegrationOrder(sequence, "overwrite-slot"))
	}
	require.Len(t, application.borrowed, 3)
	require.Equal(t, wire, log.messageArgument)
	assertInboundRingVacant(t, s.boundedIn.ring)
}

func TestBoundedInboundSessionDisconnectCleanup(t *testing.T) {
	s, application, _, store := newInboundIntegrationSession(t, 2, 1)
	inboundIntegrationDeliver(t, s, inboundIntegrationOrder(2, "discard-on-disconnect"))
	incoming := make(chan fixIn, 2)
	for range 2 {
		incoming <- fixIn{slot: inboundRingTestAcquire(t, s.boundedIn.ring)}
	}
	s.messageIn = incoming
	s.Disconnected(s)
	require.Nil(t, s.messageIn)
	require.Nil(t, s.messageOut)
	require.Equal(t, 1, application.logouts)
	require.Equal(t, 1, store.NextTargetMsgSeqNum())
	assertInboundRingVacant(t, s.boundedIn.ring)

	// A new connection uses the same fixed ring, with no prior stash surviving.
	s.State = inSession{}
	s.messageOut = make(chan []byte, 64)
	inboundIntegrationDeliver(t, s, inboundIntegrationOrder(1, "reconnected-one"))
	require.Equal(t, []inboundCallbackRecord{{1, "reconnected-one"}}, application.borrowed)
	inboundIntegrationDeliver(t, s, inboundIntegrationOrder(2, "reconnected-two"))
	require.Equal(t, []inboundCallbackRecord{{1, "reconnected-one"}, {2, "reconnected-two"}}, application.borrowed)
	assertInboundRingVacant(t, s.boundedIn.ring)
}

func TestBoundedInboundSessionDisconnectJoinsReader(t *testing.T) {
	for _, queue := range []int{0, 1} {
		t.Run(strconv.Itoa(queue), func(t *testing.T) {
			s, _, _, _ := newInboundIntegrationSession(t, queue, 1)
			connection, peer := net.Pipe()
			defer connection.Close()
			defer peer.Close()
			incoming := make(chan fixIn, queue)
			first := inboundIntegrationOrder(2, "retained").build()
			reader, err := startBoundedRead(connection, newParser(connection), s.boundedIn, incoming, first, time.Now())
			require.NoError(t, err)
			s.messageIn, s.readConnection = incoming, reader
			select {
			case message := <-incoming:
				s.Incoming(s, message)
			case <-time.After(5 * time.Second):
				t.Fatal("reader did not transfer the first frame")
			}
			wire := append(inboundIntegrationOrder(3, "queued").build(), inboundIntegrationOrder(4, "reader-owned").build()...)
			writeDone := make(chan error, 1)
			go func() {
				_, err := peer.Write(wire)
				writeDone <- err
			}()
			select {
			case err := <-writeDone:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("reader did not consume the next frames")
			}
			s.Disconnected(s)
			select {
			case <-reader.done:
			default:
				t.Fatal("disconnect returned before the reader stopped")
			}
			require.Nil(t, s.readConnection)
			assertInboundRingVacant(t, s.boundedIn.ring)
		})
	}
}
