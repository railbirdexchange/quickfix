package quickfix

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type outboundRetainedScalar struct {
	tag                 Tag
	value, wire         []byte
	wantValue, wantWire string
}

func retainOutboundScalar(t *testing.T, fields *FieldMap, tag Tag) outboundRetainedScalar {
	t.Helper()
	value, reject := fields.GetBytes(tag)
	require.Nil(t, reject)
	field, present := fields.lookup(tag)
	require.True(t, present)
	return outboundRetainedScalar{tag: tag, value: value, wire: field[0].bytes, wantValue: string(value), wantWire: string(field[0].bytes)}
}

func TestOutboundLegacyRetainedGettersSurviveRepeatedSends(t *testing.T) {
	for _, messageType := range []string{"D", "0"} {
		name := "ToApp"
		if messageType == "0" {
			name = "ToAdmin"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newScopedOutboundBenchmark(t, true, messageType)
			defer fixture.close()
			fixture.session.EnableLastMsgSeqNumProcessed = true
			fixture.session.timestampPrecision = Nanos
			var retained []outboundRetainedScalar
			calls := 0
			capture := func(message *Message) {
				require.Same(t, fixture.message, message, "ordinary callbacks keep the original caller's message")
				calls++
				for _, tag := range []Tag{tagMsgSeqNum, tagSendingTime, tagLastMsgSeqNumProcessed} {
					retained = append(retained, retainOutboundScalar(t, &message.Header.FieldMap, tag))
				}
			}
			application := &scopedOutboundLifecycleApplication{
				toAdmin: capture,
				toApp: func(message *Message) error {
					capture(message)
					return nil
				},
			}
			fixture.session.application = application
			for iteration, targetSequence := range []int{7, 111, 10001} {
				require.NoError(t, fixture.store.SetNextTargetMsgSeqNum(targetSequence))
				fixture.message.Body.SetString(11, strings.Repeat(strconv.Itoa(iteration+1), (iteration+1)*13))
				require.NoError(t, fixture.process())
				retained = append(retained,
					retainOutboundScalar(t, &fixture.message.Header.FieldMap, tagBodyLength),
					retainOutboundScalar(t, &fixture.message.Trailer.FieldMap, tagCheckSum))
				for _, value := range retained {
					require.Equal(t, value.wantValue, string(value.value), "getter tag %d changed after send %d", value.tag, iteration+1)
					require.Equal(t, value.wantWire, string(value.wire), "encoded tag %d changed after send %d", value.tag, iteration+1)
				}
			}
			require.Equal(t, 3, calls)
			// Distinct storage is required even if two checksums or timestamps
			// happen to have identical text. Editing a first-send getter cannot
			// edit a current value or its separately encoded wire representation.
			for _, old := range retained[:5] {
				fields := &fixture.message.Header.FieldMap
				if old.tag == tagCheckSum {
					fields = &fixture.message.Trailer.FieldMap
				}
				current := retainOutboundScalar(t, fields, old.tag)
				old.value[0] = '?'
				require.Equal(t, current.wantValue, string(current.value), "old getter aliases current tag %d", old.tag)
				require.Equal(t, current.wantWire, string(current.wire), "old getter aliases current wire tag %d", old.tag)
			}
		})
	}
}

type outboundRetainedReplay struct {
	message                *Message
	raw, getter            []byte
	groups                 *RepeatingGroup
	wantRaw                []byte
	wantGetter, callbackID string
}

func TestOutboundLegacyReplaySurvivesJournalReuseAndAnotherResend(t *testing.T) {
	fixture := newScopedOutboundBenchmark(t, true, "")
	defer fixture.close()
	store, err := NewBoundedStore(2, 16384)
	require.NoError(t, err)
	fixture.store, fixture.session.store, fixture.session.outbound.journal = store, store, store
	var retained []outboundRetainedReplay
	fixture.session.application = &scopedOutboundLifecycleApplication{toApp: func(message *Message) error {
		if message == fixture.message {
			return nil
		}
		getter, reject := message.Body.GetBytes(11)
		require.Nil(t, reject)
		groups := outboundOwnershipParties()
		require.Nil(t, message.Body.GetGroup(groups))
		callbackID := "callback-only-" + strconv.Itoa(len(retained)+1)
		retained = append(retained, outboundRetainedReplay{
			message: message, raw: message.Bytes(), getter: getter, groups: groups,
			wantRaw: bytes.Clone(message.Bytes()), wantGetter: string(getter), callbackID: callbackID,
		})
		// Resend keeps the original serialized body despite convenience-map
		// mutations in ToApp, while the retained callback message keeps edits.
		message.Body.SetString(11, callbackID)
		changed := outboundOwnershipParties()
		party := changed.Add()
		party.SetString(448, "callback-party")
		party.SetString(447, "D")
		party.SetInt(452, 28)
		message.Body.SetGroup(changed)
		return nil
	}}
	firstWire := completeOutboundOwnershipSend(t, fixture)
	firstStored, present := store.message(1)
	require.True(t, present)
	require.Equal(t, firstWire, firstStored)
	firstBody := bytes.Clone(outboundOwnershipParse(t, fixture, firstWire).bodyBytes)
	firstReplay := completeOutboundOwnershipResend(t, fixture, 1)
	require.Len(t, retained, 1)
	require.Equal(t, firstBody, outboundOwnershipParse(t, fixture, firstReplay).bodyBytes)
	require.Equal(t, 2, store.NextSenderMsgSeqNum(), "resend must not assign another sender sequence")
	assertOutboundRetainedReplay(t, retained[0], "4501")

	require.NoError(t, store.RetireBefore(2))
	fixture.message.Body.SetString(11, "journal-second")
	completeOutboundOwnershipSend(t, fixture)
	require.NoError(t, store.RetireBefore(3))
	fixture.message.Body.SetString(11, "journal-third-overwrites-first")
	changed := outboundOwnershipParties()
	party := changed.Add()
	party.SetString(448, "reused-party")
	party.SetString(447, "D")
	party.SetInt(452, 28)
	fixture.message.Body.SetGroup(changed)
	thirdWire := completeOutboundOwnershipSend(t, fixture)
	thirdStored, present := store.message(3)
	require.True(t, present)
	require.Same(t, &firstStored[0], &thirdStored[0], "fixture must overwrite the actual first journal slot")
	require.NotEqual(t, firstWire, thirdStored)
	thirdBody := bytes.Clone(outboundOwnershipParse(t, fixture, thirdWire).bodyBytes)
	thirdReplay := completeOutboundOwnershipResend(t, fixture, 3)
	require.Len(t, retained, 2)
	require.Equal(t, thirdBody, outboundOwnershipParse(t, fixture, thirdReplay).bodyBytes)
	require.Equal(t, 4, store.NextSenderMsgSeqNum())
	require.NotSame(t, retained[0].message, retained[1].message, "separate resend requests must not recycle delivered pointers")
	assertOutboundRetainedReplay(t, retained[0], "4501")
	assertOutboundRetainedReplay(t, retained[1], "reused-party")
	require.Len(t, fixture.session.outbound.vacant, len(fixture.session.outbound.slots))
}

func outboundOwnershipParties() *RepeatingGroup {
	return NewRepeatingGroup(453, GroupTemplate{GroupElement(448), GroupElement(447), GroupElement(452)})
}

func assertOutboundRetainedReplay(t *testing.T, retained outboundRetainedReplay, partyID string) {
	t.Helper()
	require.Equal(t, retained.wantRaw, retained.raw)
	require.Equal(t, retained.wantRaw, retained.message.Bytes())
	require.Equal(t, retained.wantGetter, string(retained.getter))
	id, reject := retained.message.Body.GetString(11)
	require.Nil(t, reject)
	require.Equal(t, retained.callbackID, id)
	require.Equal(t, 1, retained.groups.Len())
	party, reject := retained.groups.Get(0).GetString(448)
	require.Nil(t, reject)
	require.Equal(t, partyID, party)
	changed := outboundOwnershipParties()
	require.Nil(t, retained.message.Body.GetGroup(changed))
	party, reject = changed.Get(0).GetString(448)
	require.Nil(t, reject)
	require.Equal(t, "callback-party", party)
}

func completeOutboundOwnershipSend(t *testing.T, fixture *scopedOutboundBenchmark) []byte {
	t.Helper()
	require.NoError(t, SendToTarget(fixture.message, fixture.session.sessionID))
	<-fixture.session.messageEvent
	fixture.session.sendMutex.Lock()
	fixture.session.sendQueued(true)
	fixture.session.sendMutex.Unlock()
	return completeOutboundOwnershipWrite(t, fixture)
}

func completeOutboundOwnershipResend(t *testing.T, fixture *scopedOutboundBenchmark, sequence int) []byte {
	t.Helper()
	require.NoError(t, (inSession{}).resendMessages(fixture.session, sequence, sequence, *NewMessage()))
	return completeOutboundOwnershipWrite(t, fixture)
}

func completeOutboundOwnershipWrite(t *testing.T, fixture *scopedOutboundBenchmark) []byte {
	t.Helper()
	select {
	case <-fixture.session.messageEvent:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not complete the frame")
	}
	require.Len(t, fixture.session.outbound.vacant, len(fixture.session.outbound.slots))
	wire := bytes.Clone(fixture.connection.wire[:fixture.connection.used])
	fixture.connection.used = 0
	require.NoError(t, validateFIXWireBodyLengthAndCheckSum(wire))
	return wire
}

func outboundOwnershipParse(t *testing.T, fixture *scopedOutboundBenchmark, wire []byte) *Message {
	t.Helper()
	fixture.peer.Reset()
	require.NoError(t, fixture.peer.Parse(wire))
	return fixture.peer.Message()
}
