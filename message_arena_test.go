package quickfix

import (
	"bytes"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func newArenaTestMessage(t testing.TB, frameBytes, fieldCount int) *Message {
	t.Helper()
	message := NewMessage()
	message.Header.SetString(tagBeginString, "FIX.4.4")
	message.Header.SetString(tagMsgType, "D")
	if err := message.attachMessageArena(frameBytes, fieldCount); err != nil {
		t.Fatal(err)
	}
	return message
}

func arenaTestWire(t testing.TB, message *Message) []byte {
	t.Helper()
	wire, err := message.AppendTo(make([]byte, 0, 4096))
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func assertArenaValue(t testing.TB, fields *FieldMap, tag Tag, expected string) {
	t.Helper()
	value, reject := fields.GetBytes(tag)
	if reject != nil || string(value) != expected {
		t.Fatalf("tag %d: value %q reject %v; want %q", tag, value, reject, expected)
	}
}

func TestMessageArenaScalarWireParity(t *testing.T) {
	message := newArenaTestMessage(t, 1024, 32)
	legacy := NewMessage()
	legacy.Header.SetString(tagBeginString, "FIX.4.4")
	legacy.Header.SetString(tagMsgType, "D")
	for _, value := range []int{0, 1, -1, 123456, int(^uint(0) >> 1), -int(^uint(0)>>1) - 1} {
		for _, fields := range []*FieldMap{&message.Body.FieldMap, &legacy.Body.FieldMap} {
			fields.SetInt(38, value)
			fields.SetString(11, "order-"+strconv.Itoa(value))
			fields.SetBool(43, value < 0)
			fields.SetBytes(55, []byte("SYMBOL"))
		}
		if got, want := arenaTestWire(t, message), arenaTestWire(t, legacy); !bytes.Equal(got, want) {
			t.Fatalf("wire differs: got %q want %q", got, want)
		}
	}
	message.Body.SetString(11, "")
	assertArenaValue(t, &message.Body.FieldMap, 11, "")
	message.Body.SetBytes(55, nil)
	value, _ := message.Body.GetBytes(55)
	if value != nil {
		t.Fatal("nil external SetBytes value lost its alias semantics")
	}
}

func TestMessageArenaSetBytesAliasing(t *testing.T) {
	for _, test := range []struct {
		name, first, second string
		target              Tag
		source              Tag
		start, end          int
	}{
		{"growth_moves_source", "a", "abcdefgh", 11, 55, 1, 7},
		{"shrink_moves_source", "abcdefghijklmnop", "abcdefgh", 11, 55, 2, 5},
		{"same_field_subslice", "abcdefgh", "tail", 11, 11, 1, 6},
		{"same_field_full", "abcdefgh", "tail", 11, 11, 0, 8},
		{"earlier_source", "abcdefgh", "x", 55, 11, 2, 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := newArenaTestMessage(t, 512, 16)
			message.Body.SetString(11, test.first)
			message.Body.SetString(55, test.second)
			source, _ := message.Body.GetBytes(test.source)
			borrowed := source[test.start:test.end:test.end]
			expected := string(borrowed)
			message.Body.SetBytes(test.target, borrowed)
			assertArenaValue(t, &message.Body.FieldMap, test.target, expected)
			values, _ := message.Body.lookup(test.target)
			if string(values[0].bytes) != strconv.Itoa(int(test.target))+"="+expected+"\x01" {
				t.Fatalf("wire did not preserve call-entry input: %q", values[0].bytes)
			}
			if test.source != test.target {
				message.Body.SetString(test.source, "a much longer replacement value")
				assertArenaValue(t, &message.Body.FieldMap, test.target, expected)
			}
		})
	}

	t.Run("external_alias_keeps_separate_wire", func(t *testing.T) {
		message := newArenaTestMessage(t, 512, 16)
		external := []byte("ABC")
		message.Body.SetBytes(55, external)
		message.Body.SetString(11, "grow another field")
		value, _ := message.Body.GetBytes(55)
		if &value[0] != &external[0] {
			t.Fatal("external alias detached")
		}
		external[0] = 'Z'
		assertArenaValue(t, &message.Body.FieldMap, 55, "ZBC")
		values, _ := message.Body.lookup(55)
		if string(values[0].bytes) != "55=ABC\x01" {
			t.Fatal("external mutation changed encoded snapshot")
		}
	})

	t.Run("mutable_owned_value_keeps_separate_wire", func(t *testing.T) {
		message := newArenaTestMessage(t, 512, 16)
		message.Body.SetString(11, "before")
		message.Body.SetString(55, "ABC")
		value, _ := message.Body.GetBytes(55)
		value[0] = 'Z'
		message.Body.SetString(11, "an unrelated growing field shifts both planes")
		assertArenaValue(t, &message.Body.FieldMap, 55, "ZBC")
		values, _ := message.Body.lookup(55)
		if string(values[0].bytes) != "55=ABC\x01" {
			t.Fatal("owned value mutation changed encoded snapshot")
		}
		value, _ = message.Body.GetBytes(55)
		message.Body.SetBytes(11, value[1:])
		assertArenaValue(t, &message.Body.FieldMap, 11, "BC")
		values, _ = message.Body.lookup(11)
		if string(values[0].bytes) != "11=BC\x01" {
			t.Fatal("value-plane source was not captured before movement")
		}
	})

	t.Run("overlap_from_wire_plane", func(t *testing.T) {
		message := newArenaTestMessage(t, 512, 16)
		message.Body.SetString(11, "long target field")
		message.Body.SetString(55, "SYMBOL")
		values, _ := message.Body.lookup(55)
		source := values[0].bytes[3:9:9]
		message.Body.SetBytes(11, source)
		assertArenaValue(t, &message.Body.FieldMap, 11, "SYMBOL")
	})
}

func TestMessageArenaPublicReparseDetaches(t *testing.T) {
	message := newArenaTestMessage(t, 512, 16)
	other := NewMessage()
	other.Header.SetString(tagBeginString, "FIX.4.4")
	other.Header.SetString(tagMsgType, "D")
	other.Body.SetString(11, "parsed")
	if err := ParseMessage(message, bytes.NewBuffer(arenaTestWire(t, other))); err != nil {
		t.Fatal(err)
	}
	if message.arena != nil || message.Body.arena != nil {
		t.Fatal("convenience parser retained outbound arena accounting")
	}
	assertArenaValue(t, &message.Body.FieldMap, 11, "parsed")
}

func TestMessageArenaAttachRefusesOversizedInputWithoutMutation(t *testing.T) {
	message := NewMessage()
	message.Header.SetString(tagBeginString, "FIX.4.4")
	message.Header.SetString(tagMsgType, "D")
	message.Body.SetString(11, "body")
	wire := arenaTestWire(t, message)
	if err := message.attachMessageArena(512, 4); err != ErrFieldCapacity {
		t.Fatalf("field error = %v", err)
	}
	if err := message.attachMessageArena(24, 8); err != ErrFrameCapacity {
		t.Fatalf("frame error = %v", err)
	}
	if message.arena != nil || !bytes.Equal(wire, arenaTestWire(t, message)) {
		t.Fatal("failed attachment mutated source")
	}
}

func TestMessageArenaCannotChangeLimitsAfterAttachment(t *testing.T) {
	message := newArenaTestMessage(t, 512, 16)
	message.Body.SetString(11, "original")
	originalArena := message.arena
	originalWire := arenaTestWire(t, message)
	for _, limits := range [][2]int{{256, 16}, {1024, 16}, {512, 8}, {512, 32}} {
		if err := message.attachMessageArena(limits[0], limits[1]); err != errMessageArenaLimits {
			t.Fatalf("limits %v: got %v, want capacity mismatch", limits, err)
		}
		if message.arena != originalArena || !bytes.Equal(arenaTestWire(t, message), originalWire) {
			t.Fatal("capacity mismatch changed the message")
		}
	}
	if err := message.attachMessageArena(512, 16); err != nil {
		t.Fatal(err)
	}
}

func TestMessageArenaCapacityIsAtomicAndLatched(t *testing.T) {
	message := newArenaTestMessage(t, 64, 8)
	message.Body.SetString(11, "original")
	before := bytes.Clone(message.arena.bytes)
	beforeUsed := message.arena.used
	message.Body.SetString(11, strings.Repeat("x", 64))
	if message.constructionError() != ErrFrameCapacity {
		t.Fatal("missing frame capacity error")
	}
	assertArenaValue(t, &message.Body.FieldMap, 11, "original")
	if message.arena.used != beforeUsed || !bytes.Equal(before, message.arena.bytes) {
		t.Fatal("failed setter changed arena")
	}
	message.Body.SetString(11, "smaller")
	assertArenaValue(t, &message.Body.FieldMap, 11, "original")
	destination := []byte("prefix")
	if got, err := message.AppendTo(destination); err != ErrFrameCapacity || !bytes.Equal(got, destination) {
		t.Fatalf("append did not preserve destination: %q %v", got, err)
	}
	for _, encode := range []func(){func() { message.Bytes() }, func() { message.String() }, func() { message.build() }, func() { message.buildWithBodyBytes(nil) }} {
		func() {
			defer func() {
				if got := recover(); got != ErrFrameCapacity {
					t.Errorf("panic = %v, want static capacity error", got)
				}
			}()
			encode()
		}()
	}
	message.Header.Clear()
	if message.constructionError() != ErrFrameCapacity {
		t.Fatal("other section clear reset body error")
	}
	message.Body.Clear()
	message.Header.SetString(tagBeginString, "FIX.4.4")
	message.Header.SetString(tagMsgType, "D")
	message.Body.SetString(11, "recovered")
	if message.constructionError() != nil {
		t.Fatal("clear did not recover")
	}
	arenaTestWire(t, message)
}

func TestMessageArenaWholeFrameAndFieldBudget(t *testing.T) {
	message := newArenaTestMessage(t, 64, 6)
	message.Body.SetString(11, "one")
	message.Trailer.SetString(89, "two")
	message.Header.SetInt(34, 1)
	if message.constructionError() != ErrFieldCapacity || message.Header.Has(34) {
		t.Fatal("whole-message field limit not atomic")
	}
	message.Header.Clear()
	message.Header.SetString(tagBeginString, "FIX.4.4")
	message.Header.SetString(tagMsgType, "D")
	if message.constructionError() != nil {
		t.Fatal("field limit did not recover after clear")
	}
	// The two framing fields count even before an encoding method stores them.
	minimal := newArenaTestMessage(t, 64, 4)
	minimal.Body.SetInt(38, 1)
	if minimal.constructionError() != ErrFieldCapacity || minimal.Body.Has(38) {
		t.Fatal("implicit BodyLength and CheckSum did not count against MaxFields")
	}

	message = newArenaTestMessage(t, 40, 8)
	// 8=FIX.4.4<SOH> + 35=D<SOH> + 11=<15 bytes><SOH> + computed 9/10 = 46 bytes.
	message.Body.SetString(11, strings.Repeat("x", 15))
	if message.constructionError() != ErrFrameCapacity || message.Body.Has(11) {
		t.Fatal("framing bytes excluded from whole-frame budget")
	}
}

func TestMessageArenaRemoveClearAndChurn(t *testing.T) {
	message := newArenaTestMessage(t, 256, 8)
	for index := 0; index < 100; index++ {
		message.Body.SetString(11, "a long clordid")
		message.Body.SetInt(55, index)
		message.Body.Remove(11)
		assertArenaValue(t, &message.Body.FieldMap, 55, strconv.Itoa(index))
		message.Body.SetString(Tag(1000+index), "new")
		message.Body.Clear()
		if message.arena.fieldCount != 2 || message.arena.used != 15 {
			t.Fatalf("capacity leaked: count %d bytes %d", message.arena.fieldCount, message.arena.used)
		}
	}
}

func TestMessageArenaCopyAndValueCopyDetach(t *testing.T) {
	message := newArenaTestMessage(t, 512, 16)
	message.Body.SetString(11, "original")
	wire := arenaTestWire(t, message)
	shallow := *message
	shallow.Body.SetString(11, "copy")
	assertArenaValue(t, &message.Body.FieldMap, 11, "original")
	if shallow.Body.arena != nil {
		t.Fatal("shallow field mutation kept shared arena")
	}
	if !bytes.Equal(wire, arenaTestWire(t, message)) {
		t.Fatal("shallow mutation changed original wire")
	}

	copied := *message
	message.CopyInto(&copied)
	sectionCopy := message.Body.FieldMap
	message.Body.CopyInto(&sectionCopy)
	message.Body.SetString(11, "replacement is longer")
	assertArenaValue(t, &copied.Body.FieldMap, 11, "original")
	assertArenaValue(t, &sectionCopy, 11, "original")
	if copied.arena != nil || copied.Body.arena != nil || sectionCopy.arena != nil {
		t.Fatal("explicit copy retained arena")
	}
	message.CopyInto(message)
	message.Body.CopyInto(&message.Body.FieldMap)
	assertArenaValue(t, &message.Body.FieldMap, 11, "replacement is longer")

	ordinary := NewMessage()
	ordinary.Body.SetString(11, "cold copied destination")
	ordinary.CopyInto(message)
	if message.arena != nil || message.Body.arena != nil {
		t.Fatal("CopyInto destination kept construction arena")
	}
	assertArenaValue(t, &message.Body.FieldMap, 11, "cold copied destination")
}

func TestMessageArenaExistingGroupsSurviveMovesAndCopies(t *testing.T) {
	message := NewMessage()
	message.Header.SetString(tagBeginString, "FIX.4.4")
	message.Header.SetString(tagMsgType, "D")
	message.Body.SetString(11, "old")
	group := NewRepeatingGroup(453, GroupTemplate{GroupElement(448), GroupElement(447)})
	entry := group.Add()
	entry.SetString(448, "PARTY")
	entry.SetString(447, "D")
	message.Body.SetGroup(group)
	if err := message.attachMessageArena(512, 16); err != nil {
		t.Fatal(err)
	}
	message.Body.SetString(11, "longer value moves group bytes")
	values, _ := message.Body.lookup(453)
	if len(values) != 3 || string(values[1].bytes) != "448=PARTY\x01" {
		t.Fatalf("group was flattened or corrupted: %v", values)
	}
	owned := NewMessage()
	message.CopyInto(owned)
	read := NewRepeatingGroup(453, GroupTemplate{GroupElement(448), GroupElement(447)})
	if err := message.Body.GetGroup(read); err != nil {
		t.Fatal(err)
	}
	message.Body.Clear()
	message.Body.SetString(11, strings.Repeat("x", 100))
	values, _ = owned.Body.lookup(453)
	if len(values) != 3 || string(values[1].bytes) != "448=PARTY\x01" {
		t.Fatal("copy retained group arena")
	}
	assertArenaValue(t, &read.Get(0).FieldMap, 448, "PARTY")
}

func TestMessageArenaScalarMutationsAllocateZero(t *testing.T) {
	message := newArenaTestMessage(t, 512, 16)
	external := []byte("symbol")
	mutate := func() {
		message.Body.SetInt(38, 123456)
		message.Body.SetString(11, "order-one")
		message.Body.SetBool(43, true)
		message.Body.SetBytes(55, external)
		message.Body.SetString(11, "x")
		value, _ := message.Body.GetBytes(11)
		message.Body.SetBytes(55, value)
		message.Body.Remove(43)
		message.Body.SetBool(43, false)
	}
	mutate()
	if allocations := testing.AllocsPerRun(1000, mutate); allocations != 0 {
		t.Fatalf("allocations = %g", allocations)
	}
	if err := message.constructionError(); err != nil {
		t.Fatal(err)
	}
	runtime.KeepAlive(message)
}

// This component benchmark measures cold setup cost only. It is not the
// ordinary-session performance qualification or a substitute for completed I/O.
func BenchmarkMessageArenaColdConstruction(b *testing.B) {
	for _, attach := range []bool{false, true} {
		name := "ordinary"
		if attach {
			name = "with_default_arena"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for index := 0; index < b.N; index++ {
				message := NewMessage()
				message.Header.SetString(tagBeginString, "FIX.4.4")
				message.Header.SetString(tagMsgType, "D")
				message.Body.SetString(11, "order-id")
				message.Body.SetInt(38, 100)
				message.Body.SetBool(43, false)
				if attach {
					limits := DefaultParseLimits()
					if err := message.attachMessageArena(limits.MaxFrameBytes, limits.MaxFields); err != nil {
						b.Fatal(err)
					}
				}
				runtime.KeepAlive(message)
			}
		})
	}
}
