package quickfix

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type builtLifecycleApplication struct {
	scopedOutboundLifecycleApplication
	built func(SessionID, *Message, []byte)
}

func (application *builtLifecycleApplication) OnMessageBuilt(id SessionID, message *Message, raw []byte) {
	if application.built != nil {
		application.built(id, message, raw)
	}
}

func TestBuiltObserverResolvedAtSessionConstruction(t *testing.T) {
	for _, borrowed := range []bool{false, true} {
		name := "owned"
		if borrowed {
			name = "borrowed"
		}
		t.Run(name, func(t *testing.T) {
			calls := 0
			var observed []byte
			observer := &builtLifecycleApplication{built: func(_ SessionID, _ *Message, raw []byte) {
				calls++
				observed = bytes.Clone(raw)
			}}
			var application Application = observer
			if borrowed {
				application = BorrowApplication(application)
			}
			current, err := (sessionFactory{}).newSession(
				SessionID{BeginString: BeginStringFIX42, SenderCompID: "ISLD", TargetCompID: "TW"},
				NewBoundedStoreFactory(2, 16384), inboundIntegrationSettings(1, 1), NewNullLogFactory(), application)
			require.NoError(t, err)
			defer current.store.Close()
			defer current.outbound.dropPending()
			require.NoError(t, current.queueForSend(scopedOutboundLifecycleOrder("constructed")))
			require.Equal(t, 1, calls)
			require.Equal(t, current.outbound.front().wire, observed)
			require.NoError(t, validateFIXWireBodyLengthAndCheckSum(observed))
		})
	}
}

func TestBuiltObserverFollowsPersistenceAndPrecedesEnqueue(t *testing.T) {
	for _, borrowed := range []bool{false, true} {
		name := "owned"
		if borrowed {
			name = "borrowed"
		}
		t.Run(name, func(t *testing.T) {
			fixture, _, connection := newOutboundLifecycleFixture(t, "D", true)
			sequence := fixture.store.NextSenderMsgSeqNum()
			calls, toAppCalls := 0, 0
			var observed []byte
			application := &builtLifecycleApplication{}
			application.toApp = func(message *Message) error {
				toAppCalls++
				message.Body.SetString(11, "callback-edited-order")
				return nil
			}
			application.built = func(id SessionID, message *Message, raw []byte) {
				calls++
				require.Equal(t, fixture.session.sessionID, id)
				require.Same(t, fixture.message, message, "first send retains the caller's Message identity")
				require.Equal(t, 1, toAppCalls)
				require.Equal(t, sequence+1, fixture.store.NextSenderMsgSeqNum())
				persisted, present := fixture.store.message(sequence)
				require.True(t, present, "observer must follow successful persistence")
				require.Equal(t, persisted, raw)
				require.NoError(t, validateFIXWireBodyLengthAndCheckSum(raw))
				require.Zero(t, fixture.session.outbound.count, "observer must precede pending-queue admission")
				require.Len(t, fixture.session.outbound.vacant, len(fixture.session.outbound.slots)-1)
				select {
				case <-connection.started:
					t.Fatal("write started before the built observer returned")
				default:
				}
				observed = bytes.Clone(raw)
			}
			fixture.session.setApplication(application)
			if borrowed {
				fixture.session.setApplication(BorrowApplication(application))
			}
			outboundLifecycleEnqueue(t, fixture)
			require.Equal(t, 1, calls, "BorrowApplication must forward the hook exactly once")
			require.Equal(t, observed, fixture.session.outbound.front().wire)
			outboundLifecycleFlush(t, fixture.session)
			scopedBackpressureWait(t, connection.started, "write did not start")
			require.Equal(t, 1, calls, "the built event is not a completed-write event")
			connection.allow <- struct{}{}
			outboundLifecycleWake(t, fixture.session)
			require.Equal(t, observed, connection.wire[:connection.used])
			require.Len(t, fixture.session.outbound.vacant, len(fixture.session.outbound.slots))
		})
	}
}

func TestBuiltObserverOwnedRawSurvivesOutputReuse(t *testing.T) {
	fixture := newScopedOutboundBenchmark(t, true, "")
	defer fixture.close()
	var retained, expected []byte
	application := &builtLifecycleApplication{}
	application.built = func(_ SessionID, message *Message, raw []byte) {
		require.Same(t, fixture.message, message)
		if retained == nil {
			retained, expected = raw, bytes.Clone(raw)
		}
	}
	fixture.session.setApplication(application)
	require.NoError(t, fixture.process())
	for range len(fixture.session.outbound.slots) + 1 {
		fixture.message.Body.SetString(11, "replacement-order-after-observation")
		require.NoError(t, fixture.process())
	}
	require.Equal(t, expected, retained, "ordinary observers may retain built bytes across completed sends")
}

func TestBuiltObserverOwnedControlsSurviveReuse(t *testing.T) {
	fixture, _ := newScopedControlFixture(t)
	defer fixture.close()
	var previousAdmin *Message
	var messages []*Message
	var wires, expected [][]byte
	application := &builtLifecycleApplication{}
	application.toAdmin = func(message *Message) { previousAdmin = message }
	application.built = func(_ SessionID, message *Message, raw []byte) {
		require.Same(t, previousAdmin, message, "ToAdmin and built observer must see the same owned control")
		require.NotSame(t, fixture.session.controls.message, message)
		messages = append(messages, message)
		wires, expected = append(wires, raw), append(expected, bytes.Clone(raw))
	}
	fixture.session.setApplication(application)
	for _, id := range []string{"first-heartbeat", "replacement-heartbeat"} {
		require.NoError(t, fixture.session.sendHeartbeat(nil, []byte(id)))
		require.NoError(t, completeScopedControl(fixture))
		require.Equal(t, expected[len(expected)-1], fixture.connection.wire[:fixture.connection.used])
		fixture.connection.used = 0
		fixture.floor++
		require.NoError(t, fixture.store.RetireBefore(fixture.floor))
	}
	require.Len(t, messages, 2)
	require.NotSame(t, messages[0], messages[1])
	// All writes have completed. Overwrite every returned wire slot to expose
	// accidental borrowed delivery even before the ring naturally wraps.
	for range fixture.session.outbound.slots {
		slot := <-fixture.session.outbound.vacant
		clear(slot.wire[:cap(slot.wire)])
		slot.release()
	}
	for index, id := range []string{"first-heartbeat", "replacement-heartbeat"} {
		assertArenaValue(t, &messages[index].Body.FieldMap, tagTestReqID, id)
		require.Equal(t, expected[index], wires[index])
	}
}

func TestBuiltObserverOwnedReplayPreservesWireAndLifetime(t *testing.T) {
	fixture := newScopedOutboundBenchmark(t, true, "")
	defer fixture.close()
	store, err := NewBoundedStore(2, 16384)
	require.NoError(t, err)
	fixture.store, fixture.session.store, fixture.session.outbound.journal = store, store, store
	application := &builtLifecycleApplication{}
	fixture.session.setApplication(application)
	firstWire := completeOutboundOwnershipSend(t, fixture)
	firstBody := bytes.Clone(outboundOwnershipParse(t, fixture, firstWire).bodyBytes)
	firstStored, present := store.message(1)
	require.True(t, present)
	var messages []*Message
	var wires, expected [][]byte
	application.toApp = func(message *Message) error {
		if message != fixture.message {
			message.Body.SetString(11, "replay-callback-edit")
			parties := outboundOwnershipParties()
			party := parties.Add()
			party.SetString(448, "callback-only-party")
			party.SetString(447, "D")
			party.SetInt(452, 28)
			message.Body.SetGroup(parties)
		}
		return nil
	}
	application.built = func(_ SessionID, message *Message, raw []byte) {
		if message == fixture.message {
			return
		}
		messages = append(messages, message)
		wires, expected = append(wires, raw), append(expected, bytes.Clone(raw))
	}
	firstReplay := completeOutboundOwnershipResend(t, fixture, 1)
	require.Len(t, messages, 1)
	require.Equal(t, firstReplay, wires[0])
	require.Equal(t, firstBody, outboundOwnershipParse(t, fixture, firstReplay).bodyBytes,
		"resend observer bytes must preserve original grouped body order despite callback edits")
	require.Equal(t, 2, store.NextSenderMsgSeqNum())
	require.NoError(t, store.RetireBefore(2))
	fixture.message.Body.SetString(11, "second-journal-message")
	completeOutboundOwnershipSend(t, fixture)
	require.NoError(t, store.RetireBefore(3))
	fixture.message.Body.SetString(11, "third-overwrites-first-journal-message")
	thirdWire := completeOutboundOwnershipSend(t, fixture)
	thirdStored, present := store.message(3)
	require.True(t, present)
	require.Same(t, &firstStored[0], &thirdStored[0], "fixture must actually reuse the first journal slot")
	thirdBody := bytes.Clone(outboundOwnershipParse(t, fixture, thirdWire).bodyBytes)
	thirdReplay := completeOutboundOwnershipResend(t, fixture, 3)
	require.Len(t, messages, 2)
	require.NotSame(t, messages[0], messages[1])
	require.Equal(t, thirdReplay, wires[1])
	require.Equal(t, thirdBody, outboundOwnershipParse(t, fixture, thirdReplay).bodyBytes)
	require.Equal(t, 4, store.NextSenderMsgSeqNum(), "replay must not consume sender sequence numbers")
	for index, message := range messages {
		require.Equal(t, expected[index], wires[index])
		assertArenaValue(t, &message.Body.FieldMap, 11, "replay-callback-edit")
		parties := outboundOwnershipParties()
		require.NoError(t, message.Body.GetGroup(parties))
		require.Equal(t, 1, parties.Len())
		assertArenaValue(t, &parties.Get(0).FieldMap, 448, "callback-only-party")
	}
}

func TestBuiltObserverBorrowedFirstSendCopiesOwnInputs(t *testing.T) {
	fixture := newScopedOutboundBenchmarkWithDelivery(t, true, "", true)
	defer fixture.close()
	header, body, trailer, partyID := []byte("HEADER"), []byte("123.45"), []byte("SIGNED"), []byte("PARTY")
	fixture.message.Header.SetBytes(115, header)
	fixture.message.Body.SetBytes(44, body)
	fixture.message.Trailer.SetBytes(89, trailer)
	parties := outboundOwnershipParties()
	party := parties.Add()
	party.SetBytes(448, partyID)
	party.SetString(447, "D")
	party.SetInt(452, 1)
	fixture.message.Body.SetGroup(parties)
	retained, sections := NewMessage(), NewMessage()
	retainedParties := outboundOwnershipParties()
	calls := 0
	application := &builtLifecycleApplication{built: func(_ SessionID, message *Message, _ []byte) {
		calls++
		require.Same(t, fixture.message, message)
		require.Nil(t, message.arena, "first-send observer must cover the cold construction representation")
		message.CopyInto(retained)
		message.Header.CopyInto(&sections.Header.FieldMap)
		message.Body.CopyInto(&sections.Body.FieldMap)
		message.Trailer.CopyInto(&sections.Trailer.FieldMap)
		require.NoError(t, message.Body.GetGroup(retainedParties))
	}}
	fixture.session.setApplication(BorrowApplication(application))
	require.NoError(t, fixture.process())
	require.Equal(t, 1, calls)
	for _, input := range [][]byte{header, body, trailer, partyID} {
		for index := range input {
			input[index] = 'X'
		}
	}
	for _, snapshot := range []*Message{retained, sections} {
		assertArenaValue(t, &snapshot.Header.FieldMap, 115, "HEADER")
		assertArenaValue(t, &snapshot.Body.FieldMap, 44, "123.45")
		assertArenaValue(t, &snapshot.Trailer.FieldMap, 89, "SIGNED")
	}
	assertArenaValue(t, &retainedParties.Get(0).FieldMap, 448, "PARTY")
	assertArenaValue(t, &fixture.message.Body.FieldMap, 44, "XXXXXX")
	for _, fields := range []*FieldMap{&fixture.message.Header.FieldMap, &fixture.message.Body.FieldMap, &fixture.message.Trailer.FieldMap} {
		require.False(t, fields.reusable, "built-observer copy scope escaped the callback")
	}
}

type builtObserverFailingStore struct {
	MessageStore
	failure error
	calls   int
}

func (store *builtObserverFailingStore) SaveMessageAndIncrNextSenderMsgSeqNum(int, []byte) error {
	store.calls++
	return store.failure
}

func TestBuiltObserverNotCalledForFailedSend(t *testing.T) {
	for _, mode := range []string{"callback", "persistence", "output_capacity", "journal_capacity", "frame_capacity"} {
		t.Run(mode, func(t *testing.T) {
			current, _, store := newScopedOutboundLifecycleSession(t)
			calls := 0
			application := &builtLifecycleApplication{built: func(SessionID, *Message, []byte) { calls++ }}
			current.setApplication(BorrowApplication(application))
			message := scopedOutboundLifecycleOrder("rejected-send")
			failure := errors.New("send refused")
			var failedStore *builtObserverFailingStore
			switch mode {
			case "callback":
				application.toApp = func(*Message) error { return failure }
			case "persistence":
				failedStore = &builtObserverFailingStore{MessageStore: store, failure: failure}
				current.store = failedStore
				current.outbound.journal, current.outbound.borrowedStore = nil, nil
			case "output_capacity":
				failure = ErrOutboundCapacity
				for range current.outbound.pending {
					slot, err := current.outbound.reserve(false)
					require.NoError(t, err)
					current.outbound.enqueue(slot)
				}
			case "journal_capacity":
				failure = ErrJournalFull
				for sequence := 1; sequence <= len(store.entries); sequence++ {
					require.NoError(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(sequence, []byte("retained")))
				}
			case "frame_capacity":
				failure = ErrBufferTooSmall
				message.Body.SetString(58, strings.Repeat("x", 1024))
			}
			before := snapshotBoundedStore(store)
			pending, vacant := current.outbound.count, len(current.outbound.vacant)
			require.ErrorIs(t, current.queueForSend(message), failure)
			require.Zero(t, calls)
			require.Equal(t, before, snapshotBoundedStore(store))
			require.Equal(t, pending, current.outbound.count)
			require.Len(t, current.outbound.vacant, vacant)
			if failedStore != nil {
				require.Equal(t, 1, failedStore.calls, "fixture must reach the failing persistence call")
			}
			current.outbound.dropPending()
		})
	}
}

func TestBuiltObserverBorrowedCompletedSendNoAllocations(t *testing.T) {
	fixture := newScopedOutboundBenchmarkWithDelivery(t, true, "", true)
	defer fixture.close()
	calls := 0
	valid := true
	application := &builtLifecycleApplication{built: func(id SessionID, message *Message, raw []byte) {
		calls++
		if id != fixture.session.sessionID || message != fixture.message || len(raw) == 0 || raw[len(raw)-1] != '\x01' {
			valid = false
		}
	}}
	fixture.session.setApplication(BorrowApplication(application))
	for range 2 * len(fixture.session.outbound.slots) {
		require.NoError(t, fixture.process())
	}
	before := calls
	var operationError error
	allocations := testing.AllocsPerRun(100, func() { operationError = fixture.process() })
	require.NoError(t, operationError)
	require.True(t, valid)
	require.Equal(t, before+101, calls, "each completed write, including warmup, must invoke the hook once")
	require.Zero(t, allocations, "borrowed observation must not allocate or omit completed writes")
}
