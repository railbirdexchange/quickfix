package quickfix

import (
	"bytes"
	"strings"
	"testing"
)

func partyGroupValues() []GroupField {
	return []GroupField{
		{Tag: 448, Text: "EXEC-FIRM"}, {Tag: 447, Text: "D"}, {Tag: 452, Text: "1"},
		{Tag: 448, Text: "CLIENT-ID"}, {Tag: 447, Text: "D"}, {Tag: 452, Text: "3"},
	}
}

func legacyPartyGroup() *RepeatingGroup {
	group := NewRepeatingGroup(453, GroupTemplate{GroupElement(448), GroupElement(447), GroupElement(452)})
	for _, party := range []struct{ id, role string }{{"EXEC-FIRM", "1"}, {"CLIENT-ID", "3"}} {
		entry := group.Add()
		entry.SetString(448, party.id)
		entry.SetString(447, "D")
		entry.SetString(452, party.role)
	}
	return group
}

func TestGroupValuesWireParityAndNestedBinary(t *testing.T) {
	message := newArenaTestMessage(t, 4096, 64)
	legacy := NewMessage()
	legacy.Header.SetString(tagBeginString, "FIX.4.4")
	legacy.Header.SetString(tagMsgType, "D")
	if err := message.Body.SetGroupValues(453, 2, partyGroupValues()); err != nil {
		t.Fatal(err)
	}
	legacy.Body.SetGroup(legacyPartyGroup())
	if got, want := arenaTestWire(t, message), arenaTestWire(t, legacy); !bytes.Equal(got, want) {
		t.Fatalf("party wire differs: got %q want %q", got, want)
	}
	group := NewRepeatingGroup(453, GroupTemplate{
		GroupElement(448), GroupElement(447), GroupElement(452),
		NewRepeatingGroup(802, GroupTemplate{GroupElement(523), GroupElement(803)}),
	})
	entry := group.Add()
	entry.SetString(448, "P=\x01ARTY")
	entry.SetString(447, "")
	entry.SetString(452, "1")
	nested := NewRepeatingGroup(802, GroupTemplate{GroupElement(523), GroupElement(803)})
	child := nested.Add()
	child.SetBytes(523, []byte{'a', 0, '=', '\x01', 'z'})
	child.SetString(803, "4")
	entry.SetGroup(nested)
	legacy.Body.SetGroup(group)
	values := []GroupField{
		{Tag: 448, Text: "P=\x01ARTY"}, {Tag: 447, Value: []byte{}, Text: "ignored"},
		{Tag: 452, Text: "1"}, {Tag: 802, Text: "1"},
		{Tag: 523, Value: []byte{'a', 0, '=', '\x01', 'z'}, Text: "ignored"}, {Tag: 803, Text: "4"},
	}
	if err := message.Body.SetGroupValues(453, 1, values); err != nil {
		t.Fatal(err)
	}
	if got, want := arenaTestWire(t, message), arenaTestWire(t, legacy); !bytes.Equal(got, want) {
		t.Fatalf("nested/binary wire differs: got %q want %q", got, want)
	}
	if err := message.Body.SetGroupValues(453, 0, nil); err != nil {
		t.Fatal(err)
	}
	legacy.Body.SetGroup(NewRepeatingGroup(453, nil))
	if got, want := arenaTestWire(t, message), arenaTestWire(t, legacy); !bytes.Equal(got, want) {
		t.Fatalf("empty group wire differs: got %q want %q", got, want)
	}
}

func TestGroupValuesOwnInputAndAliasSnapshot(t *testing.T) {
	for _, arenaEnabled := range []bool{false, true} {
		message := NewMessage()
		if arenaEnabled {
			message = newArenaTestMessage(t, 1024, 32)
		}
		external := []byte("ORIGINAL")
		if err := message.Body.SetGroupValues(453, 1, []GroupField{{Tag: 448, Value: external}}); err != nil {
			t.Fatal(err)
		}
		external[0] = 'X'
		group, _ := message.Body.lookup(453)
		if string(group[1].value) != "ORIGINAL" || string(group[1].bytes) != "448=ORIGINAL\x01" {
			t.Fatalf("arena %v retained caller input", arenaEnabled)
		}
	}
	for _, plane := range []string{"values", "wire"} {
		for _, replacement := range []string{"short", strings.Repeat("long", 12)} {
			message := newArenaTestMessage(t, 2048, 32)
			message.Body.SetString(11, "scalar-source")
			if err := message.Body.SetGroupValues(453, 2, partyGroupValues()); err != nil {
				t.Fatal(err)
			}
			if err := message.Body.SetGroupValues(555, 1, []GroupField{{Tag: 600, Text: "trailing"}}); err != nil {
				t.Fatal(err)
			}
			group, _ := message.Body.lookup(453)
			source := group[1].value[1:5]
			if plane == "wire" {
				source = group[1].bytes[5:9]
			}
			scalar, _ := message.Body.GetBytes(11)
			if err := message.Body.SetGroupValues(453, 1, []GroupField{
				{Tag: 448, Text: replacement}, {Tag: 447, Value: source}, {Tag: 452, Value: scalar},
			}); err != nil {
				t.Fatal(err)
			}
			group, _ = message.Body.lookup(453)
			if string(group[2].value) != "XEC-" || string(group[3].value) != "scalar-source" {
				t.Fatalf("%s alias corrupted: %q %q", plane, group[2].value, group[3].value)
			}
			message.Body.SetString(11, "a much longer scalar moves both group planes")
			group, _ = message.Body.lookup(453)
			if string(group[2].value) != "XEC-" || string(group[3].value) != "scalar-source" {
				t.Fatal("group data lost after scalar movement")
			}
			trailing, _ := message.Body.lookup(555)
			if string(trailing[1].value) != "trailing" {
				t.Fatal("group metadata lost after compaction")
			}
			group[2].value[0] = '!'
			if string(group[2].bytes) != "447=XEC-\x01" {
				t.Fatal("mutable getter changed encoded snapshot")
			}
		}
	}
}

func TestGroupValuesReplacementRemoveClear(t *testing.T) {
	message := newArenaTestMessage(t, 1024, 32)
	for cycle := 0; cycle < 10; cycle++ {
		if err := message.Body.SetGroupValues(453, 2, partyGroupValues()); err != nil {
			t.Fatal(err)
		}
		if err := message.Trailer.SetGroupValues(555, 1, []GroupField{{Tag: 600, Text: "trailer"}}); err != nil {
			t.Fatal(err)
		}
		message.Body.SetString(453, "scalar")
		assertArenaValue(t, &message.Body.FieldMap, 453, "scalar")
		if len(message.arena.groupValues) != 2 {
			t.Fatal("scalar replacement did not release group metadata")
		}
		if err := message.Body.SetGroupValues(453, 2, partyGroupValues()); err != nil {
			t.Fatal(err)
		}
		message.Body.SetGroup(legacyPartyGroup())
		if len(message.arena.groupValues) != 2 {
			t.Fatal("legacy group replacement did not release group metadata")
		}
		if err := message.Body.SetGroupValues(453, 2, partyGroupValues()); err != nil {
			t.Fatal(err)
		}
		message.Trailer.Remove(555)
		group, _ := message.Body.lookup(453)
		if string(group[4].value) != "CLIENT-ID" {
			t.Fatal("remove corrupted another group's metadata")
		}
		message.Body.Clear()
		if len(message.arena.groupValues) != 0 || message.arena.fieldCount != 2 {
			t.Fatal("clear did not reclaim group metadata and field budget")
		}
		for _, value := range message.arena.groupValues[:cap(message.arena.groupValues)] {
			if value.value != nil || value.bytes != nil {
				t.Fatal("vacant metadata retains stale slices")
			}
		}
	}
}

func TestGroupValuesCapacityAndInvalidInputAreAtomic(t *testing.T) {
	for _, test := range []struct {
		name   string
		fields int
		values []GroupField
		want   error
	}{
		{"field_limit", 8, partyGroupValues(), ErrFieldCapacity},
		{"frame_limit", 32, []GroupField{{Tag: 448, Text: strings.Repeat("x", 256)}}, ErrFrameCapacity},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := newArenaTestMessage(t, 256, test.fields)
			if err := message.Body.SetGroupValues(453, 1, []GroupField{{Tag: 448, Text: "kept"}}); err != nil {
				t.Fatal(err)
			}
			wire := bytes.Clone(message.arena.bytes[:message.arena.used])
			metadata := bytes.Clone(message.arena.values[:message.arena.used])
			used, count := message.arena.used, message.arena.fieldCount
			if err := message.Body.SetGroupValues(453, 1, test.values); err != test.want {
				t.Fatalf("got %v want %v", err, test.want)
			}
			if message.arena.used != used || message.arena.fieldCount != count ||
				!bytes.Equal(wire, message.arena.bytes[:used]) || !bytes.Equal(metadata, message.arena.values[:used]) {
				t.Fatal("capacity failure changed existing storage")
			}
			group, _ := message.Body.lookup(453)
			if string(group[1].value) != "kept" || len(message.arena.groupValues) != 2 {
				t.Fatal("capacity failure changed group metadata")
			}
			if _, err := message.AppendTo(make([]byte, 0, 256)); err != test.want {
				t.Fatalf("construction error did not reach AppendTo: %v", err)
			}
			message.Body.Clear()
			if err := message.Body.SetGroupValues(453, 1, []GroupField{{Tag: 448, Text: "recovered"}}); err != nil {
				t.Fatal(err)
			}
			arenaTestWire(t, message)
		})
	}
	message := newArenaTestMessage(t, 1024, 32)
	message.Body.SetString(11, "unchanged")
	wire := arenaTestWire(t, message)
	for _, invalid := range []struct {
		tag    Tag
		count  int
		values []GroupField
	}{
		{0, 1, partyGroupValues()}, {453, -1, nil}, {453, 1, nil},
		{453, 0, partyGroupValues()}, {453, 1, []GroupField{{Tag: -1}}},
	} {
		if err := message.Body.SetGroupValues(invalid.tag, invalid.count, invalid.values); err != ErrInvalidGroupValues {
			t.Fatalf("invalid input returned %v", err)
		}
		if !bytes.Equal(wire, arenaTestWire(t, message)) {
			t.Fatal("invalid input mutated message")
		}
	}
}

func TestGroupValuesShallowCopyDetaches(t *testing.T) {
	message := newArenaTestMessage(t, 1024, 32)
	if err := message.Body.SetGroupValues(453, 2, partyGroupValues()); err != nil {
		t.Fatal(err)
	}
	copy := *message
	if err := copy.Body.SetGroupValues(453, 1, []GroupField{{Tag: 448, Text: "COPY"}}); err != nil {
		t.Fatal(err)
	}
	message.Body.Clear()
	group, _ := copy.Body.lookup(453)
	if string(group[1].value) != "COPY" || copy.Body.arena != nil {
		t.Fatal("shallow copy retained mutable original storage")
	}
	if message.Body.Has(453) {
		t.Fatal("original clear did not remove group")
	}
}

func TestGroupValuesGetGroupRetainsOwnedSnapshot(t *testing.T) {
	message := newArenaTestMessage(t, 1024, 32)
	if err := message.Body.SetGroupValues(453, 2, partyGroupValues()); err != nil {
		t.Fatal(err)
	}
	retained := NewRepeatingGroup(453, GroupTemplate{GroupElement(448), GroupElement(447), GroupElement(452)})
	if err := message.Body.GetGroup(retained); err != nil {
		t.Fatal(err)
	}
	message.Body.Clear()
	if err := message.Body.SetGroupValues(453, 1, []GroupField{{Tag: 448, Text: "replacement"}}); err != nil {
		t.Fatal(err)
	}
	first, firstErr := retained.Get(0).GetString(448)
	second, secondErr := retained.Get(1).GetString(448)
	if retained.Len() != 2 || firstErr != nil || secondErr != nil || first != "EXEC-FIRM" || second != "CLIENT-ID" {
		t.Fatal("legacy GetGroup snapshot changed after outgoing group reuse")
	}
}

func TestGroupValuesWarmedLifecycleZeroAllocations(t *testing.T) {
	message := newArenaTestMessage(t, 1024, 32)
	values := partyGroupValues()
	buffer := make([]byte, 0, 1024)
	allocations := testing.AllocsPerRun(1000, func() {
		message.Body.Clear()
		message.Body.SetString(11, "order")
		if err := message.Body.SetGroupValues(453, 2, values); err != nil {
			panic(err)
		}
		if err := message.Body.SetGroupValues(453, 1, values[:3]); err != nil {
			panic(err)
		}
		message.Body.SetString(11, "longer-order")
		if _, err := message.AppendTo(buffer[:0]); err != nil {
			panic(err)
		}
		message.Body.Remove(453)
	})
	if allocations != 0 {
		t.Fatalf("warmed group lifecycle allocated %g objects", allocations)
	}
}

func BenchmarkOrderedGroupConstruction(b *testing.B) {
	for _, name := range []string{"repeating_group", "group_values"} {
		b.Run(name, func(b *testing.B) {
			message := newArenaTestMessage(b, 1024, 32)
			buffer := make([]byte, 0, 1024)
			values := partyGroupValues()
			populate := func() {
				if name == "repeating_group" {
					message.Body.SetGroup(legacyPartyGroup())
				} else if err := message.Body.SetGroupValues(453, 2, values); err != nil {
					b.Fatal(err)
				}
			}
			populate()
			if _, err := message.AppendTo(buffer[:0]); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				populate()
				if _, err := message.AppendTo(buffer[:0]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
