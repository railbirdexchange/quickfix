package quickfix

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScopedOutboundFirstSendEstablishesScopeOnlyOnReuse(t *testing.T) {
	fixture := newScopedOutboundBenchmark(t, false, "")
	defer fixture.close()
	message := fixture.message
	require.NoError(t, fixture.process())
	require.Nil(t, message.arena)
	require.Nil(t, message.outboundFields)
	require.Same(t, message, message.outboundOwner)
	require.False(t, message.Header.reusable)
	require.False(t, message.Trailer.reusable)
	message.CopyInto(message)
	require.Same(t, message, message.outboundOwner, "self-copy must preserve the representation")
	require.NoError(t, fixture.process())
	require.NotNil(t, message.arena)
	require.Same(t, message, message.arena.owner)
	require.NotNil(t, message.outboundFields)
	require.Same(t, message, message.outboundFields.owner)
	if allocations := testing.AllocsPerRun(100, func() {
		if err := fixture.process(); err != nil {
			t.Fatal(err)
		}
	}); allocations != 0 {
		t.Fatalf("reused completed send allocated: %v", allocations)
	}
}

func TestScopedOutboundFirstSendOwnsEngineValuesAndWire(t *testing.T) {
	fixture := newScopedOutboundBenchmark(t, false, "")
	defer fixture.close()
	message := fixture.message
	fixture.session.application = &scopedOutboundLifecycleApplication{toApp: func(message *Message) error {
		for _, tag := range []Tag{tagMsgSeqNum, tagSendingTime} {
			value, reject := message.Header.GetBytes(tag)
			require.Nil(t, reject)
			field, _ := message.Header.lookup(tag)
			wire := bytes.Clone(field[0].bytes)
			value[0] = 'X'
			require.Equal(t, wire, field[0].bytes, "mutating a cold getter must not rewrite wire")
		}
		return nil
	}}
	// process verifies the peer's encoded sequence independently of the getter
	// mutation above, and returns only after the slot has returned from Write.
	require.NoError(t, fixture.process())
	bodyLength, reject := message.Header.GetBytes(tagBodyLength)
	require.Nil(t, reject)
	checksum, reject := message.Trailer.GetBytes(tagCheckSum)
	require.Nil(t, reject)
	wantLength, wantChecksum := bytes.Clone(bodyLength), bytes.Clone(checksum)
	for range fixture.session.outbound.slots {
		slot := <-fixture.session.outbound.vacant
		clear(slot.wire[:cap(slot.wire)])
		slot.release()
	}
	require.Equal(t, wantLength, bodyLength, "BodyLength getter borrowed a recycled output slot")
	require.Equal(t, wantChecksum, checksum, "CheckSum getter borrowed a recycled output slot")
	for _, fields := range []*FieldMap{&message.Header.FieldMap, &message.Trailer.FieldMap} {
		for _, tag := range []Tag{tagBodyLength, tagCheckSum} {
			if field, present := fields.lookup(tag); present {
				wire := bytes.Clone(field[0].bytes)
				field[0].value[0] = 'X'
				require.Equal(t, wire, field[0].bytes)
			}
		}
	}
}

func TestScopedOutboundFirstSendInternalBytesDetachInput(t *testing.T) {
	message := NewMessage()
	source := []byte("20261016-12:34:56.789")
	message.setOutboundBytes(&message.Header.FieldMap, outboundOriginalTimestamp, tagOrigSendingTime, source)
	value, reject := message.Header.GetBytes(tagOrigSendingTime)
	require.Nil(t, reject)
	field, _ := message.Header.lookup(tagOrigSendingTime)
	wire := bytes.Clone(field[0].bytes)
	source[0] = 'X'
	require.Equal(t, byte('2'), value[0])
	value[0] = 'Y'
	require.Equal(t, wire, field[0].bytes)
	require.Nil(t, message.outboundFields)
}

func TestScopedOutboundFirstSendReparseResetsReuse(t *testing.T) {
	for _, previouslyWarmed := range []bool{false, true} {
		name := "cold"
		if previouslyWarmed {
			name = "previously_warmed"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newScopedOutboundBenchmark(t, false, "")
			defer fixture.close()
			message := fixture.message
			if previouslyWarmed {
				require.NoError(t, fixture.process())
				require.NoError(t, fixture.process())
				require.NotNil(t, message.arena)
			}
			wire, dictionary := boundedOwnershipFixture(t, false)
			raw := bytes.NewBuffer(wire)
			for range 4 {
				require.NoError(t, ParseMessageWithDataDictionary(message, raw, dictionary, dictionary))
				require.Nil(t, message.outboundOwner)
				require.Nil(t, message.arena)
				require.NoError(t, fixture.process())
				require.Nil(t, message.arena, "convenience reparse must not recreate the construction arena")
				require.Same(t, message, message.outboundOwner)
			}
		})
	}
}

func TestScopedOutboundFirstSendCopyBoundaries(t *testing.T) {
	fixture := newScopedOutboundBenchmark(t, false, "")
	defer fixture.close()
	source := fixture.message
	require.NoError(t, fixture.process())
	require.NoError(t, fixture.process())
	sourceSequence, reject := source.Header.GetInt(tagMsgSeqNum)
	require.Nil(t, reject)
	shallow := *source
	require.NotSame(t, &shallow, shallow.outboundOwner)
	fixture.message, fixture.application.message = &shallow, &shallow
	require.NoError(t, fixture.process())
	require.NotSame(t, source.arena, shallow.arena)
	require.NotSame(t, source.outboundFields, shallow.outboundFields)
	afterSequence, reject := source.Header.GetInt(tagMsgSeqNum)
	require.Nil(t, reject)
	require.Equal(t, sourceSequence, afterSequence)

	copied := NewMessage()
	source.CopyInto(copied)
	require.Nil(t, copied.outboundOwner)
	require.Nil(t, copied.outboundFields)
	require.Nil(t, copied.arena)
	fixture.message, fixture.application.message = copied, copied
	require.NoError(t, fixture.process())
	require.Nil(t, copied.arena)
	require.Nil(t, copied.outboundFields)
	require.Same(t, copied, copied.outboundOwner)

	// CopyInto a previously warmed destination must also reset its reuse marker.
	source.CopyInto(&shallow)
	require.Nil(t, shallow.outboundOwner)
	require.Nil(t, shallow.arena)
	fixture.message, fixture.application.message = &shallow, &shallow
	require.NoError(t, fixture.process())
	require.Nil(t, shallow.arena)
	copySequence, reject := copied.Header.GetInt(tagMsgSeqNum)
	require.Nil(t, reject)
	require.Equal(t, sourceSequence+2, copySequence)
}
