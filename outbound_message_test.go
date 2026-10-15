package quickfix

import (
	"bytes"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/datadictionary"
)

func outboundTestMessage(t testing.TB, fields, size int) *OutboundMessage {
	t.Helper()
	message, err := NewOutboundMessage(fields, size)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func outboundTestWire(t testing.TB, message *Message) []byte {
	t.Helper()
	wire, err := message.AppendTo(make([]byte, 0, 8192))
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestOutboundMessageScalarWireFixture(t *testing.T) {
	message := outboundTestMessage(t, 16, 256)
	message.Header().SetString(tagBeginString, BeginStringFIX44)
	message.Header().SetString(tagMsgType, "A")
	message.Header().SetTime(tagSendingTime, FIXUTCTimestamp{
		Time: time.Date(2014, time.June, 15, 19, 49, 56, 999999999, time.UTC), Precision: Seconds,
	})
	message.Body().SetString(554, "secret")
	message.Body().SetString(553, "my_user")
	if err := message.Err(); err != nil {
		t.Fatal(err)
	}
	const expected = "8=FIX.4.4\x019=49\x0135=A\x0152=20140615-19:49:56\x01553=my_user\x01554=secret\x0110=072\x01"
	if wire := outboundTestWire(t, message.Message()); string(wire) != expected {
		t.Fatalf("wire = %q, want %q", wire, expected)
	}
	if message.Message().Header.Has(tagBodyLength) || message.Message().Trailer.Has(tagCheckSum) || message.Message().fields != nil {
		t.Fatal("read/append view unexpectedly contains generated or parsed fields")
	}
	if err := message.Header().SetInt(tagBodyLength, 9999); err != nil {
		t.Fatal(err)
	}
	if err := message.Trailer().SetString(tagCheckSum, "999"); err != nil {
		t.Fatal(err)
	}
	if wire := outboundTestWire(t, message.Message()); string(wire) != expected {
		t.Fatal("stored length or checksum changed generated wire")
	}
}

func TestOutboundMessageIntegerAndTimestampFormatting(t *testing.T) {
	maximum := int(^uint(0) >> 1)
	for _, value := range []int{0, -1, 1, maximum, -maximum - 1} {
		message := outboundTestMessage(t, 1, 128)
		if err := message.Body().SetInt(38, value); err != nil {
			t.Fatal(err)
		}
		actual, reject := message.Message().Body.GetBytes(38)
		if reject != nil || string(actual) != strconv.Itoa(value) {
			t.Fatalf("integer %d = %q, reject = %v", value, actual, reject)
		}
	}
	for _, precision := range []TimestampPrecision{Seconds, Millis, Micros, Nanos, -1} {
		for _, year := range []int{2026, -1, 10000} {
			value := FIXUTCTimestamp{Time: time.Date(year, time.October, 15, 9, 10, 11, 987654321, time.FixedZone("offset", 90*60)), Precision: precision}
			message := outboundTestMessage(t, 1, 128)
			if err := message.Header().SetTime(52, value); err != nil {
				t.Fatal(err)
			}
			actual, reject := message.Message().Header.GetBytes(52)
			if reject != nil || string(actual) != value.UTC().Format(value.layout()) {
				t.Fatalf("timestamp = %q, reject = %v", actual, reject)
			}
		}
	}
	message := outboundTestMessage(t, 2, 10)
	message.Header().SetBool(43, true)
	message.Body().SetBool(97, false)
	if message.Err() != nil {
		t.Fatal(message.Err())
	}
	if yes, _ := message.Message().Header.GetBytes(43); string(yes) != "Y" {
		t.Fatal("true was not encoded as Y")
	}
	if no, _ := message.Message().Body.GetBytes(97); string(no) != "N" {
		t.Fatal("false was not encoded as N")
	}
}

func TestOutboundMessageNestedGroupWire(t *testing.T) {
	dictionary := boundedValidationNestedDictionary()
	wire := boundedValidationNestedWire(borrowedNestedWire)
	legacy := NewMessage()
	if err := ParseMessageWithDataDictionary(legacy, bytes.NewBuffer(wire), dictionary, dictionary); err != nil {
		t.Fatal(err)
	}
	expected := outboundTestWire(t, legacy)
	parsed := borrowedTestValues(t, borrowedNestedWire)
	typed := make([]OutboundField, len(parsed))
	for index, value := range parsed {
		typed[index] = OutboundField{Tag: value.tag, Value: value.value}
	}
	for _, raw := range []bool{false, true} {
		message := outboundTestMessage(t, 64, 1024)
		message.Header().SetString(8, BeginStringFIX44)
		message.Header().SetString(35, "D")
		var err error
		if raw {
			err = message.Body().SetRawGroup(1000, []byte(borrowedNestedWire))
		} else {
			err = message.Body().SetGroup(1000, typed)
		}
		if err != nil {
			t.Fatal(err)
		}
		if actual := outboundTestWire(t, message.Message()); !bytes.Equal(actual, expected) {
			t.Fatalf("nested group wire = %q, want %q", actual, expected)
		}
		values, ok := message.Message().Body.lookup(1000)
		if !ok || len(values) != len(parsed) || &values[0] != &message.fields[2] {
			t.Fatal("group metadata is not one contiguous owned span")
		}
	}
}

func TestOutboundMessageTypedGroupBinaryValue(t *testing.T) {
	binary := []byte{'a', '\x01', 'b', '=', 0}
	group := []OutboundField{{453, []byte("1")}, {448, []byte("party")}, {354, []byte("5")}, {355, binary}}
	message := outboundTestMessage(t, 6, 128)
	message.Header().SetString(8, BeginStringFIX44)
	message.Header().SetString(35, "D")
	if err := message.Body().SetGroup(453, group); err != nil {
		t.Fatal(err)
	}
	expected := boundedValidationNestedWire("453=1\x01448=party\x01354=5\x01355=a\x01b=\x00\x01")
	if actual := outboundTestWire(t, message.Message()); !bytes.Equal(actual, expected) {
		t.Fatalf("binary group = %q, want %q", actual, expected)
	}
	copy := outboundTestMessage(t, 6, 128)
	if err := copy.CopyFrom(message.Message()); err != nil {
		t.Fatal(err)
	}
	clear(binary)
	clear(group[1].Value)
	message.Reset()
	if actual := outboundTestWire(t, copy.Message()); !bytes.Equal(actual, expected) {
		t.Fatal("typed group or CopyFrom retained caller/source bytes")
	}
}

func TestOutboundMessageCopiedInputAndResendHeaders(t *testing.T) {
	const original = "8=FIX.4.2\x019=104\x0135=D\x0134=2\x0149=TW\x0152=20140515-19:49:56.659\x0156=ISLD\x0111=100\x0121=1\x0140=1\x0154=1\x0155=TSLA\x0160=00010101-00:00:00.000\x0110=039\x01"
	wire := []byte(original)
	source, err := NewMessageSlot(nil, nil, DefaultParseLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Parse(wire); err != nil {
		t.Fatal(err)
	}
	received := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	source.Message().ReceiveTime = received
	message := outboundTestMessage(t, 64, 1024)
	if err := message.CopyFrom(source.Message()); err != nil {
		t.Fatal(err)
	}
	if string(wire) != original || !message.Message().ReceiveTime.Equal(received) {
		t.Fatal("CopyFrom changed source bytes or failed to copy receive metadata")
	}
	message.Header().SetString(tagOrigSendingTime, "20140515-19:49:56.659")
	message.Header().SetTime(tagSendingTime, FIXUTCTimestamp{Time: time.Date(2014, time.June, 15, 19, 49, 56, 0, time.UTC), Precision: Seconds})
	message.Header().SetBool(tagPossDupFlag, true)
	if err := message.Err(); err != nil {
		t.Fatal(err)
	}
	if actual, _ := source.Message().Header.GetBytes(tagSendingTime); string(actual) != "20140515-19:49:56.659" {
		t.Fatal("outbound mutation changed borrowed source fields")
	}
	source.Reset()
	clear(wire)
	const expected = "8=FIX.4.2\x019=131\x0135=D\x0134=2\x0143=Y\x0149=TW\x0152=20140615-19:49:56\x0156=ISLD\x01122=20140515-19:49:56.659\x0111=100\x0121=1\x0140=1\x0154=1\x0155=TSLA\x0160=00010101-00:00:00.000\x0110=122\x01"
	if actual := outboundTestWire(t, message.Message()); string(actual) != expected {
		t.Fatalf("resend wire = %q, want %q", actual, expected)
	}
	if message.Message().inbound != nil || message.Message().rawMessage != nil || message.Message().fields != nil || message.Message().bodyBytes != nil {
		t.Fatal("builder retained parsed source storage or inbound ownership")
	}
}

type outboundTestSnapshot struct {
	wire       string
	arena      []byte
	usedBytes  int
	usedFields int
	received   time.Time
}

func outboundTestSnapshotOf(t testing.TB, message *OutboundMessage) outboundTestSnapshot {
	t.Helper()
	return outboundTestSnapshot{wire: string(outboundTestWire(t, message.Message())), arena: bytes.Clone(message.arena[:cap(message.arena)]),
		usedBytes: len(message.arena), usedFields: message.usedFields, received: message.Message().ReceiveTime}
}

func TestOutboundMessageMutationCapacityAtomicity(t *testing.T) {
	for _, fixture := range []struct {
		name          string
		fields, bytes int
		mutate        func(*OutboundMessage) error
		expected      error
	}{
		{"scalar metadata overwrite", 1, 128, func(m *OutboundMessage) error { return m.Body().SetString(11, "replacement") }, ErrFieldCapacity},
		{"scalar byte overwrite", 4, 5, func(m *OutboundMessage) error { return m.Body().SetString(11, "replacement") }, ErrOutboundBuilderCapacity},
		{"group metadata", 3, 128, func(m *OutboundMessage) error {
			return m.Body().SetGroup(453, []OutboundField{{453, []byte("1")}, {448, []byte("p")}, {447, []byte("D")}})
		}, ErrFieldCapacity},
		{"group bytes", 8, 12, func(m *OutboundMessage) error {
			return m.Body().SetGroup(453, []OutboundField{{453, []byte("1")}, {448, []byte("party")}})
		}, ErrOutboundBuilderCapacity},
		{"raw metadata", 2, 128, func(m *OutboundMessage) error { return m.Body().SetRawGroup(453, []byte("453=1\x01448=p\x01")) }, ErrFieldCapacity},
		{"raw bytes", 8, 12, func(m *OutboundMessage) error { return m.Body().SetRawGroup(453, []byte("453=1\x01448=party\x01")) }, ErrOutboundBuilderCapacity},
		{"invalid scalar tag", 8, 128, func(m *OutboundMessage) error { return m.Body().SetString(0, "bad") }, ErrInvalidOutboundField},
		{"invalid typed member", 8, 128, func(m *OutboundMessage) error {
			return m.Body().SetGroup(453, []OutboundField{{453, []byte("1")}, {-1, []byte("bad")}})
		}, ErrInvalidOutboundField},
		{"typed root mismatch", 8, 128, func(m *OutboundMessage) error { return m.Body().SetGroup(453, []OutboundField{{448, []byte("1")}}) }, ErrInvalidOutboundField},
		{"nil typed group", 8, 128, func(m *OutboundMessage) error { return m.Body().SetGroup(453, nil) }, ErrInvalidOutboundField},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			message := outboundTestMessage(t, fixture.fields, fixture.bytes)
			if err := message.Body().SetString(11, "A"); err != nil {
				t.Fatal(err)
			}
			before := outboundTestSnapshotOf(t, message)
			if err := fixture.mutate(message); err != fixture.expected || message.Err() != fixture.expected {
				t.Fatalf("error = %v, sticky = %v, want %v", err, message.Err(), fixture.expected)
			}
			if after := outboundTestSnapshotOf(t, message); !reflect.DeepEqual(after, before) {
				t.Fatal("failed mutation changed fields, metadata budget, or arena storage")
			}
			if err := message.Body().Remove(11); err != fixture.expected {
				t.Fatal("a subsequent mutation did not return the first sticky error")
			}
			message.Reset()
			if message.Err() != nil || message.usedFields != 0 || len(message.arena) != 0 || message.Message().Body.Has(11) {
				t.Fatal("reset did not restore budgets and clear sticky state")
			}
			if err := message.Body().SetString(11, "B"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutboundMessageRawGroupSyntaxAtomicity(t *testing.T) {
	for _, wire := range []string{"", "453=1", "448=1\x01", "453=1\x01448", "453=1\x01=bad\x01", "453=1\x01x=bad\x01", "453=1\x010=bad\x01", "453=1\x019999999999999999999999999999999=bad\x01"} {
		message := outboundTestMessage(t, 8, 128)
		message.Body().SetString(11, "A")
		before := outboundTestSnapshotOf(t, message)
		if err := message.Body().SetRawGroup(453, []byte(wire)); err != ErrInvalidOutboundField {
			t.Fatalf("raw %q: error = %v", wire, err)
		}
		if after := outboundTestSnapshotOf(t, message); !reflect.DeepEqual(after, before) {
			t.Fatalf("raw %q changed message storage before failing", wire)
		}
	}
}

func TestOutboundMessageRemoveKeepsMutationBudgetAndCopiesInput(t *testing.T) {
	message := outboundTestMessage(t, 4, 64)
	value := []byte("original")
	if err := message.Body().SetBytes(11, value); err != nil {
		t.Fatal(err)
	}
	clear(value)
	if actual, _ := message.Message().Body.GetBytes(11); string(actual) != "original" {
		t.Fatal("scalar setter retained input bytes")
	}
	usedFields, usedBytes := message.usedFields, len(message.arena)
	if err := message.Body().Remove(11); err != nil {
		t.Fatal(err)
	}
	if message.Message().Body.Has(11) || message.usedFields != usedFields || len(message.arena) != usedBytes {
		t.Fatal("remove retained the key or reclaimed append-only mutation budget")
	}
	raw := []byte("453=1\x01448=party\x01")
	if err := message.Body().SetRawGroup(453, raw); err != nil {
		t.Fatal(err)
	}
	clear(raw)
	if err := message.Body().SetString(11, "again"); err != nil {
		t.Fatal(err)
	}
	values, _ := message.Message().Body.lookup(453)
	if len(values) != 2 || string(values[1].value) != "party" || message.Message().Body.fieldCount() != 2 {
		t.Fatal("raw group retained caller bytes or remove/re-add duplicated a map key")
	}
}

func TestOutboundMessageCopyPreflightAtomicity(t *testing.T) {
	large := NewMessage()
	large.Body.SetString(11, "a long copied value")
	large.Body.SetString(55, "symbol")
	for _, limits := range []struct{ fields, bytes int }{{1, 128}, {8, 5}} {
		message := outboundTestMessage(t, limits.fields, limits.bytes)
		message.Body().SetString(11, "A")
		before := outboundTestSnapshotOf(t, message)
		if err := message.CopyFrom(large); err != ErrFieldCapacity && err != ErrOutboundBuilderCapacity {
			t.Fatalf("copy error = %v, want configured capacity error", err)
		}
		if after := outboundTestSnapshotOf(t, message); !reflect.DeepEqual(after, before) {
			t.Fatal("failed whole-message copy changed destination")
		}
	}
	message := outboundTestMessage(t, 8, 128)
	message.Body().SetString(11, "A")
	before := outboundTestSnapshotOf(t, message)
	if err := message.CopyFrom(message.Message()); err != nil || !reflect.DeepEqual(outboundTestSnapshotOf(t, message), before) {
		t.Fatal("copying exact view was not a no-op")
	}
	alias := *message.Message()
	if err := message.CopyFrom(&alias); err != ErrInvalidOutboundSource || !reflect.DeepEqual(outboundTestSnapshotOf(t, message), before) {
		t.Fatal("shallow alias was not rejected before changing its source")
	}
	message.Reset()
	if err := message.CopyFrom(nil); err != ErrInvalidOutboundSource {
		t.Fatal("nil source was not rejected")
	}
}

func TestOutboundMessageCopyFromBorrowedArenaUsesRemainingBudget(t *testing.T) {
	message := outboundTestMessage(t, 8, 256)
	if err := message.Body().SetBytes(58, []byte(boundedHeartbeatFrame)); err != nil {
		t.Fatal(err)
	}
	borrowed, reject := message.Message().Body.GetBytes(58)
	if reject != nil {
		t.Fatal(reject)
	}
	source := NewMessage()
	if err := ParseMessage(source, bytes.NewBuffer(borrowed)); err != nil {
		t.Fatal(err)
	}
	usedFields, usedBytes := message.usedFields, len(message.arena)
	if err := message.CopyFrom(source); err != nil {
		t.Fatal(err)
	}
	if string(borrowed) != boundedHeartbeatFrame || string(source.Bytes()) != boundedHeartbeatFrame {
		t.Fatal("copy into owned storage changed its independently parsed source")
	}
	if message.usedFields <= usedFields || len(message.arena) <= usedBytes || message.Message().Body.Has(58) {
		t.Fatal("whole-message copy reclaimed mutation storage or retained old map keys")
	}
	if actual := outboundTestWire(t, message.Message()); string(actual) != boundedHeartbeatFrame {
		t.Fatalf("copy from borrowed arena = %q", actual)
	}
}

func TestOutboundMessageLimitsAndInactiveLifecycle(t *testing.T) {
	maximum := int(^uint(0) >> 1)
	for _, limits := range []struct{ fields, bytes int }{{0, 1}, {1, 0}, {-1, 1}, {1, -1}, {maximum, 1}, {1, maximum}} {
		if message, err := NewOutboundMessage(limits.fields, limits.bytes); message != nil || err != ErrInvalidOutboundMessageLimits {
			t.Fatalf("limits %+v: message = %v, error = %v", limits, message, err)
		}
	}
	message := outboundTestMessage(t, 4, 64)
	message.active = false
	if err := message.Body().SetString(11, "A"); err != ErrOutboundMessageInactive {
		t.Fatal("inactive builder accepted a mutation")
	}
	message.Reset()
	if message.active || message.Err() != nil {
		t.Fatal("reset changed lifecycle ownership or retained sticky error")
	}
	message.active = true
	if err := message.Body().SetString(11, "A"); err != nil {
		t.Fatal(err)
	}
}

func populateBoundedOrder(message *OutboundMessage, timestamp FIXUTCTimestamp) error {
	message.Reset()
	message.Header().SetString(35, "D")
	message.Header().SetString(8, BeginStringFIX40)
	message.Header().SetString(9, "0")
	message.Header().SetString(49, "0")
	message.Header().SetString(56, "0")
	message.Header().SetString(34, "0")
	message.Header().SetTime(52, timestamp)
	message.Body().SetString(11, "A")
	message.Body().SetString(21, "1")
	message.Body().SetString(55, "A")
	message.Body().SetString(54, "1")
	message.Body().SetString(40, "1")
	message.Body().SetInt(38, 5)
	message.Body().SetString(100, "0")
	message.Trailer().SetString(10, "000")
	return message.Err()
}

func repopulateLegacyOrder(message *Message, timestamp FIXUTCTimestamp) {
	message.Header.SetString(35, "D")
	message.Header.SetString(8, BeginStringFIX40)
	message.Header.SetString(9, "0")
	message.Header.SetString(49, "0")
	message.Header.SetString(56, "0")
	message.Header.SetString(34, "0")
	message.Header.SetField(52, timestamp)
	message.Body.SetString(11, "A")
	message.Body.SetString(21, "1")
	message.Body.SetString(55, "A")
	message.Body.SetString(54, "1")
	message.Body.SetString(40, "1")
	message.Body.SetInt(38, 5)
	message.Body.SetString(100, "0")
	message.Trailer.SetString(10, "000")
}

func TestOutboundMessageWarmedNoAllocations(t *testing.T) {
	legacy, expected, _ := allocationBaselineFIX40Order(t)
	var timestamp FIXUTCTimestamp
	if reject := legacy.Header.GetField(52, &timestamp); reject != nil {
		t.Fatal(reject)
	}
	message := outboundTestMessage(t, 64, 2048)
	destination := make([]byte, 0, 1024)
	repopulated := NewMessage()
	repopulateLegacyOrder(repopulated, timestamp)
	wire, err := repopulated.AppendTo(destination)
	if err != nil || !bytes.Equal(wire, expected) {
		t.Fatalf("legacy repopulation wire = %q, error = %v", wire, err)
	}
	if allocations := testing.AllocsPerRun(1000, func() {
		if err := populateBoundedOrder(message, timestamp); err != nil {
			t.Fatal(err)
		}
		wire, err := message.Message().AppendTo(destination)
		if err != nil || !bytes.Equal(wire, expected) {
			t.Fatalf("wire = %q, error = %v", wire, err)
		}
	}); allocations != 0 {
		t.Fatalf("construct and encode allocated %g times", allocations)
	}
	dictionary, err := datadictionary.Parse("spec/FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	grouped := NewMessage()
	if err := ParseMessageWithDataDictionary(grouped, bytes.NewBufferString(allocationBaselineRepeatingGroupMessage), dictionary, dictionary); err != nil {
		t.Fatal(err)
	}
	if allocations := testing.AllocsPerRun(1000, func() {
		message.Reset()
		if err := message.CopyFrom(grouped); err != nil {
			t.Fatal(err)
		}
		if _, err := message.Message().AppendTo(destination); err != nil {
			t.Fatal(err)
		}
	}); allocations != 0 {
		t.Fatalf("group copy and encode allocated %g times", allocations)
	}
	typed := []OutboundField{{453, []byte("1")}, {448, []byte("party")}, {447, []byte("D")}}
	raw := []byte("386=1\x01336=NOPL\x01")
	if allocations := testing.AllocsPerRun(1000, func() {
		message.Reset()
		message.Header().SetString(8, BeginStringFIX44)
		message.Header().SetString(35, "D")
		message.Header().SetBool(43, true)
		message.Body().SetGroup(453, typed)
		message.Body().SetRawGroup(386, raw)
		message.Body().Remove(386)
		if message.Err() != nil {
			t.Fatal(message.Err())
		}
		if _, err := message.Message().AppendTo(destination); err != nil {
			t.Fatal(err)
		}
	}); allocations != 0 {
		t.Fatalf("group construction and encode allocated %g times", allocations)
	}
}

func BenchmarkBoundedBuild(b *testing.B) {
	// Preserve the exact pre-existing benchmark as the allocating reference.
	b.Run("legacy_existing_message_baseline", benchmarkAllocationBaselineBuild)
	// Both construction paths use the caller-owned encoder. This reference
	// isolates allocations that remain in legacy field mutation after phase 2.
	b.Run("legacy_repopulate_and_append", func(b *testing.B) {
		fixture, expected, _ := allocationBaselineFIX40Order(b)
		var timestamp FIXUTCTimestamp
		if reject := fixture.Header.GetField(52, &timestamp); reject != nil {
			b.Fatal(reject)
		}
		message := NewMessage()
		destination := make([]byte, 0, DefaultParseLimits().MaxFrameBytes)
		repopulateLegacyOrder(message, timestamp)
		wire, err := message.AppendTo(destination)
		if err != nil || !bytes.Equal(wire, expected) {
			b.Fatalf("warm-up wire = %q, error = %v", wire, err)
		}
		b.ReportAllocs()
		b.SetBytes(int64(len(expected)))
		b.ResetTimer()
		for iteration := 0; iteration < b.N; iteration++ {
			repopulateLegacyOrder(message, timestamp)
			allocationBaselineBytes, err = message.AppendTo(destination)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	for _, construct := range []bool{false, true} {
		name := "bounded_existing_message_encode"
		if construct {
			name = "bounded_construct_and_encode"
		}
		b.Run(name, func(b *testing.B) {
			legacy, expected, _ := allocationBaselineFIX40Order(b)
			var timestamp FIXUTCTimestamp
			if reject := legacy.Header.GetField(52, &timestamp); reject != nil {
				b.Fatal(reject)
			}
			message := outboundTestMessage(b, DefaultParseLimits().MaxFields, 2*DefaultParseLimits().MaxFrameBytes)
			destination := make([]byte, 0, DefaultParseLimits().MaxFrameBytes)
			if err := populateBoundedOrder(message, timestamp); err != nil {
				b.Fatal(err)
			}
			wire, err := message.Message().AppendTo(destination)
			if err != nil || !bytes.Equal(wire, expected) {
				b.Fatalf("warm-up wire = %q, error = %v", wire, err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(expected)))
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if construct {
					if err := populateBoundedOrder(message, timestamp); err != nil {
						b.Fatal(err)
					}
				}
				allocationBaselineBytes, err = message.Message().AppendTo(destination)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
