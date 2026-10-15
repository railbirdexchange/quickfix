package quickfix

import (
	"bytes"
	"errors"
	"strconv"
	"testing"

	"github.com/quickfixgo/quickfix/datadictionary"
)

func TestMessageSlotDictionaryParity(t *testing.T) {
	_, order, dict40 := allocationBaselineFIX40Order(t)
	dict44, err := datadictionary.Parse("spec/FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		name string
		wire []byte
		dict *datadictionary.DataDictionary
	}{
		{"order", order, dict40},
		{"groups", []byte(allocationBaselineRepeatingGroupMessage), dict44},
		{"nested_siblings", buildFIXT11Wire("35=D\x0134=2\x0149=S\x0156=T\x0152=20231231-20:19:41\x01", "78=2\x0179=A\x01539=1\x01524=N\x01804=1\x01545=X\x0180=10\x0179=B\x01539=0\x0180=20\x01453=1\x01448=P\x01447=D\x01452=1\x01528=I\x01"), dict44},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			legacy := NewMessage()
			if err := ParseMessageWithDataDictionary(legacy, bytes.NewBuffer(fixture.wire), fixture.dict, fixture.dict); err != nil {
				t.Fatal(err)
			}
			slot, err := NewMessageSlot(fixture.dict, fixture.dict, DefaultParseLimits())
			if err != nil {
				t.Fatal(err)
			}
			if slot.Message() != nil {
				t.Fatal("unparsed slot has a message")
			}
			if err := slot.Parse(fixture.wire); err != nil {
				t.Fatal(err)
			}
			parsed := slot.Message()
			if !bytes.Equal(parsed.Bytes(), fixture.wire) {
				t.Fatal("cached frame differs")
			}
			if !bytes.Equal(parsed.bodyBytes, legacy.bodyBytes) {
				t.Fatalf("body differs: %q / %q", parsed.bodyBytes, legacy.bodyBytes)
			}
			want, err := legacy.AppendTo(make([]byte, 0, len(fixture.wire)+128))
			if err != nil {
				t.Fatal(err)
			}
			got, err := parsed.AppendTo(make([]byte, 0, len(fixture.wire)+128))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("encoded wire differs: %q / %q", got, want)
			}
			if err := validateFIXWireBodyLengthAndCheckSum(got); err != nil {
				t.Fatal(err)
			}
			if fixture.name == "order" {
				if err := NewValidator(defaultValidatorSettings, dict40, nil).Validate(parsed); err != nil {
					t.Fatal(err)
				}
			}
			slot.Reset()
			if slot.Message() != nil || slot.message.rawMessage != nil || len(slot.message.fields) != 0 {
				t.Fatal("reset retained parsed message")
			}
			for _, field := range slot.message.fields[:cap(slot.message.fields)] {
				if field.bytes != nil || field.value != nil {
					t.Fatal("reset retained frame reference")
				}
			}
			if err := slot.Parse(fixture.wire); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMessageSlotLimitsAndRecovery(t *testing.T) {
	dict, err := datadictionary.Parse("spec/FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	wire := buildFIXT11Wire("35=D\x01", "78=1\x0179=A\x01539=1\x01524=N\x01804=1\x01545=X\x01")
	for _, test := range []struct {
		name   string
		limits ParseLimits
		wire   []byte
		want   error
	}{
		{"exact", ParseLimits{len(wire), 10, 3, 3}, wire, nil},
		{"frame", ParseLimits{len(wire) - 1, 10, 3, 3}, wire, ErrFrameCapacity},
		{"fields", ParseLimits{len(wire), 9, 3, 3}, wire, ErrFieldCapacity},
		{"entries", ParseLimits{len(wire), 10, 2, 3}, wire, ErrGroupCapacity},
		{"depth", ParseLimits{len(wire), 10, 3, 2}, wire, ErrGroupDepth},
		{"declared", ParseLimits{4096, 64, 3, 3}, buildFIXT11Wire("35=D\x01", "453=4\x01"), ErrGroupCapacity},
		{"observed", ParseLimits{4096, 64, 1, 3}, buildFIXT11Wire("35=D\x01", "453=1\x01448=A\x01448=B\x01"), ErrGroupCapacity},
	} {
		t.Run(test.name, func(t *testing.T) {
			slot, err := NewMessageSlot(dict, dict, test.limits)
			if err != nil {
				t.Fatal(err)
			}
			if err := slot.Parse(test.wire); !errors.Is(err, test.want) {
				t.Fatalf("got %v want %v", err, test.want)
			}
			if test.want != nil && slot.Message() != nil {
				t.Fatal("failed parse exposed message")
			}
			if err := slot.Parse(buildFIXT11Wire("35=D\x01", "")); err != nil {
				t.Fatalf("reuse after error: %v", err)
			}
		})
	}
}

func TestMessageSlotRejectsInvalidLimitsAndTruncation(t *testing.T) {
	maximumInt := int(^uint(0) >> 1)
	for _, limits := range []ParseLimits{{}, {12, 4, -1, 0}, {12, 5, 0, 0}, {100, 8, 9, 1}, {100, 8, 1, 9}, {maximumInt, maximumInt / 3, 0, 0}} {
		if _, err := NewMessageSlot(nil, nil, limits); err != ErrInvalidParseLimits {
			t.Fatalf("limits %+v: %v", limits, err)
		}
	}
	slot, err := NewMessageSlot(nil, nil, DefaultParseLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range []string{"", "8=FIX.4.4\x01", "8=FIX.4.4\x019=5\x01", "8=FIX.4.4\x019=5\x0135=D\x01", "8=FIX.4.4\x019=5\x0135=D\x0134=1\x01", "8=FIX.4.4\x019=5\x0135=D\x01212=999\x01213=x\x0110=000\x01"} {
		if err := slot.Parse([]byte(frame)); err == nil {
			t.Fatalf("accepted %q", frame)
		}
	}
}

func TestMessageSlotChangingTagSetsDoesNotAllocate(t *testing.T) {
	slot, err := NewMessageSlot(nil, nil, ParseLimits{4096, 128, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	frames := make([][]byte, 64)
	for i := range frames {
		var body []byte
		for j := 0; j < 80; j++ {
			body = strconv.AppendInt(body, int64(10000+i*80+j), 10)
			body = append(body, '=', 'X', '\x01')
		}
		frames[i] = buildFIXT11Wire("35=D\x01", string(body))
	}
	i := 0
	if allocs := testing.AllocsPerRun(1000, func() {
		if err := slot.Parse(frames[i%len(frames)]); err != nil {
			panic(err)
		}
		i++
	}); allocs != 0 {
		t.Fatalf("changing tags allocated: %v", allocs)
	}
}

func TestMessageSlotSectionGroupsAndXMLData(t *testing.T) {
	dict, err := datadictionary.Parse("spec/FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	header := buildFIXT11Wire("35=D\x01627=2\x01628=A\x01628=B\x01", "11=order\x01")
	slot, err := NewMessageSlot(dict, dict, ParseLimits{4096, 64, 2, 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := slot.Parse(header); err != nil {
		t.Fatal(err)
	}
	hops, ok := slot.Message().Header.lookup(627)
	if !ok || len(hops) != 3 {
		t.Fatalf("header group: %v", hops)
	}
	if !bytes.Equal(slot.Message().bodyBytes, []byte("11=order\x01")) {
		t.Fatalf("body: %q", slot.Message().bodyBytes)
	}
	slot.limits.MaxGroupEntries = 1
	if err := slot.Parse(header); err != ErrGroupCapacity {
		t.Fatalf("header entries: %v", err)
	}
	slot.limits.MaxGroupEntries = 2
	slot.limits.MaxGroupDepth = 0
	slot.message.groupStack = slot.message.groupStack[:0:0]
	if err := slot.Parse(header); err != ErrGroupDepth {
		t.Fatalf("header depth: %v", err)
	}

	trailerGroup := datadictionary.NewGroupFieldDef(datadictionary.NewFieldType("TrailerGroup", 9000, "NUMINGROUP"), false,
		[]datadictionary.MessagePart{datadictionary.NewFieldDef(datadictionary.NewFieldType("TrailerID", 9001, "STRING"), true)})
	custom := *dict
	custom.Trailer = datadictionary.NewMessageDef("Trailer", "", []datadictionary.MessagePart{trailerGroup})
	slot, err = NewMessageSlot(&custom, dict, ParseLimits{4096, 64, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	trailer := buildFIXT11Wire("35=D\x01", "11=order\x019000=1\x019001=T\x01")
	if err := slot.Parse(trailer); err != nil {
		t.Fatal(err)
	}
	values, ok := slot.Message().Trailer.lookup(9000)
	if !ok || len(values) != 2 {
		t.Fatalf("trailer group: %v", values)
	}
	if !bytes.Equal(slot.Message().bodyBytes, []byte("11=order\x01")) {
		t.Fatalf("trailer in body: %q", slot.Message().bodyBytes)
	}
	if err := slot.Parse(buildFIXT11Wire("35=D\x01", "11=order\x019000=2\x019001=T\x019001=U\x01")); err != ErrGroupCapacity {
		t.Fatalf("trailer entries: %v", err)
	}

	xml := buildFIXT11Wire("35=D\x01212=5\x01213=a\x01b\x01c\x01", "")
	slot, err = NewMessageSlot(dict, dict, ParseLimits{4096, 6, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := slot.Parse(xml); err != nil {
			t.Fatal(err)
		}
		value, err := slot.Message().Header.GetBytes(213)
		if err != nil || !bytes.Equal(value, []byte("a\x01b\x01c")) || len(slot.Message().fields) != 6 {
			t.Fatalf("XML fields: %q, %d, %v", value, len(slot.Message().fields), err)
		}
	}
}

func TestMessageSlotMalformedGroupCounts(t *testing.T) {
	dict, err := datadictionary.Parse("spec/FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	slot, err := NewMessageSlot(dict, dict, ParseLimits{4096, 64, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "-1", "X", "18446744073709551617", "18446744073709551616", "999999999999999999999999999999999999999999"} {
		if err := slot.Parse(buildFIXT11Wire("35=D\x01", "453="+value+"\x01")); err == nil {
			t.Fatalf("accepted count %q", value)
		}
	}
	for _, malformed := range []string{"8=FIX.4.4\x019=20\x0135=D\x01453=1\x01448=no-terminator", "8=FIX.4.4\x019=20\x0135=D\x01453=1\x01448=A\x01"} {
		if err := slot.Parse([]byte(malformed)); err == nil {
			t.Fatalf("accepted truncation %q", malformed)
		}
	}
	if err := slot.Parse(buildFIXT11Wire("35=D\x01", "453=1\x01448=A\x01")); err != nil {
		t.Fatalf("reuse after malformed: %v", err)
	}
	group, err := slot.Group(453)
	if err != nil || group.Len() != 1 {
		t.Fatalf("group: %v, %d", err, group.Len())
	}
	iterator := group.Iter()
	entry, ok := iterator.Next()
	if !ok {
		t.Fatal("missing entry")
	}
	if value, ok := entry.GetBytes(448); !ok || string(value) != "A" {
		t.Fatalf("party: %q", value)
	}
	slot.Message().Header.Remove(35)
	if _, err := slot.Group(453); err != ErrBorrowedGroupNotFound {
		t.Fatalf("removed MsgType: %v", err)
	}
}

func BenchmarkBoundedParsing(b *testing.B) {
	dict, err := datadictionary.Parse("spec/FIX44.xml")
	if err != nil {
		b.Fatal(err)
	}
	frame := []byte(allocationBaselineRepeatingGroupMessage)
	slot, err := NewMessageSlot(dict, dict, DefaultParseLimits())
	if err != nil {
		b.Fatal(err)
	}
	if err := slot.Parse(frame); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(frame)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := slot.Parse(frame); err != nil {
			b.Fatal(err)
		}
	}
}

func FuzzMessageSlotParse(f *testing.F) {
	dictionary, err := datadictionary.Parse("spec/FIX44.xml")
	if err != nil {
		f.Fatal(err)
	}
	slot, err := NewMessageSlot(dictionary, dictionary, ParseLimits{4096, 128, 64, 8})
	if err != nil {
		f.Fatal(err)
	}
	f.Add([]byte(allocationBaselineRepeatingGroupMessage))
	f.Add(buildFIXT11Wire("35=D\x01", "78=1\x0179=A\x01539=1\x01524=N\x01804=1\x01545=X\x01"))
	f.Add(buildFIXT11Wire("35=D\x01212=5\x01213=a\x01b\x01c\x01", ""))
	f.Add([]byte("8=FIX.4.4\x019=5\x0135=D\x01453=\x0110=000\x01"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, frame []byte) {
		if err := slot.Parse(frame); err != nil {
			if slot.Message() != nil {
				t.Fatal("failed parse exposed message")
			}
			return
		}
		message := slot.Message()
		if message == nil || len(message.fields) > 128 || len(message.groupStack) > 8 {
			t.Fatal("invalid bounded parse state")
		}
		for _, tag := range message.Body.fieldTags() {
			if _, err := slot.Group(tag); err != nil && err != ErrInvalidBorrowedGroup && err != ErrBorrowedGroupNotFound {
				t.Fatal(err)
			}
		}
		slot.Reset()
	})
}
