package quickfix

import (
	"bytes"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestScopedOutboundColdScalarOwnsSeparateRegions(t *testing.T) {
	message := NewMessage()
	input := []byte("first")
	message.setOutboundBytes(&message.Header.FieldMap, outboundOriginalTimestamp, tagOrigSendingTime, input)
	first, _ := message.Header.lookup(tagOrigSendingTime)
	oldValue, oldWire := first[0].value, first[0].bytes
	require.Equal(t, len(oldValue), cap(oldValue))
	require.Equal(t, len(oldWire), cap(oldWire))
	input[0] = 'X'
	require.Equal(t, "first", string(oldValue))
	oldValue[0] = 'Y'
	require.Equal(t, "122=first\x01", string(oldWire))
	appended := append(oldWire, 'Z')
	require.Equal(t, "122=first\x01Z", string(appended))
	require.Equal(t, "Yirst", string(oldValue), "wire append crossed into the getter region")
	message.setOutboundBytes(&message.Header.FieldMap, outboundOriginalTimestamp, tagOrigSendingTime, []byte("longer replacement"))
	require.Equal(t, "Yirst", string(oldValue), "replacement rewrote the retained prior getter")
	require.Equal(t, "122=first\x01", string(oldWire), "replacement rewrote the retained prior wire")
	message.setOutboundBytes(&message.Header.FieldMap, outboundOriginalTimestamp, tagOrigSendingTime, nil)
	empty, _ := message.Header.lookup(tagOrigSendingTime)
	require.Empty(t, empty[0].value)
	require.Zero(t, cap(empty[0].value))
	require.Equal(t, "122=\x01", string(empty[0].bytes))
}

func TestScopedOutboundColdScalarFormatting(t *testing.T) {
	message := NewMessage()
	maximum := int(^uint(0) >> 1)
	for _, number := range []int{0, 1, -1, maximum, -maximum - 1} {
		message.setOutboundInt(&message.Header.FieldMap, outboundSequence, tagMsgSeqNum, number)
		field, _ := message.Header.lookup(tagMsgSeqNum)
		expected := strconv.Itoa(number)
		require.Equal(t, expected, string(field[0].value))
		require.Equal(t, "34="+expected+"\x01", string(field[0].bytes))
	}
	for _, boolean := range []bool{false, true} {
		message.setOutboundBool(&message.Header.FieldMap, outboundPossDup, tagPossDupFlag, boolean)
		field, _ := message.Header.lookup(tagPossDupFlag)
		expected := "N"
		if boolean {
			expected = "Y"
		}
		require.Equal(t, expected, string(field[0].value))
		require.Equal(t, "43="+expected+"\x01", string(field[0].bytes))
	}
	stamp := time.Date(2026, time.October, 16, 12, 34, 56, 987654321, time.UTC)
	for _, scenario := range []struct {
		stamp     time.Time
		precision TimestampPrecision
		expected  string
	}{
		{stamp, Seconds, "20261016-12:34:56"},
		{stamp, Millis, "20261016-12:34:56.987"},
		{stamp, Micros, "20261016-12:34:56.987654"},
		{stamp, Nanos, "20261016-12:34:56.987654321"},
		{stamp, TimestampPrecision(99), "20261016-12:34:56.987"},
		{stamp.AddDate(7974, 0, 0), Millis, "100001016-12:34:56.987"},
		{stamp.AddDate(-2027, 0, 0), Millis, "-00011016-12:34:56.987"},
	} {
		message.setOutboundTime(scenario.stamp, scenario.precision)
		field, _ := message.Header.lookup(tagSendingTime)
		require.Equal(t, scenario.expected, string(field[0].value))
		require.Equal(t, "52="+scenario.expected+"\x01", string(field[0].bytes))
	}
	require.Nil(t, message.outboundFields)
	require.Nil(t, message.arena)
}

func TestScopedOutboundColdScalarSingleAllocation(t *testing.T) {
	message := NewMessage()
	message.setOutboundInt(&message.Header.FieldMap, outboundSequence, tagMsgSeqNum, 1)
	message.setOutboundBool(&message.Header.FieldMap, outboundPossDup, tagPossDupFlag, false)
	message.setOutboundBytes(&message.Trailer.FieldMap, outboundChecksum, tagCheckSum, []byte("123"))
	message.setOutboundTime(time.Unix(1, 0), Nanos)
	checksum := []byte("234")
	stamp := time.Unix(2, 123456789)
	for name, set := range map[string]func(){
		"integer":   func() { message.setOutboundInt(&message.Header.FieldMap, outboundSequence, tagMsgSeqNum, 1234567) },
		"boolean":   func() { message.setOutboundBool(&message.Header.FieldMap, outboundPossDup, tagPossDupFlag, true) },
		"bytes":     func() { message.setOutboundBytes(&message.Trailer.FieldMap, outboundChecksum, tagCheckSum, checksum) },
		"timestamp": func() { message.setOutboundTime(stamp, Nanos) },
	} {
		t.Run(name, func(t *testing.T) {
			if allocations := testing.AllocsPerRun(100, set); allocations != 1 {
				t.Fatalf("cold replacement allocated %v objects, want one owned scalar", allocations)
			}
		})
	}
}

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
