package quickfix

import (
	"bytes"
	"errors"
	"testing"

	"github.com/quickfixgo/quickfix/datadictionary"
)

func TestScopedMessageCopiesAndGroupReadersOwnFrameBytes(t *testing.T) {
	dictionary, err := datadictionary.Parse("spec/FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	slot, err := NewMessageSlot(dictionary, dictionary, DefaultParseLimits())
	if err != nil {
		t.Fatal(err)
	}
	slot.enableScopedCallbacks()
	wire := buildFIXT11Wire("35=D\x01627=2\x01628=H1\x01628=H2\x01", "11=original\x0178=2\x0179=A\x01539=1\x01524=N\x01804=1\x01545=X\x0180=10\x0179=B\x01539=0\x0180=20\x01453=1\x01448=P\x01447=D\x01452=1\x01")
	if err := slot.Parse(wire); err != nil {
		t.Fatal(err)
	}
	source := slot.Message()
	frameOwner := &inboundSlot{}
	source.inbound = frameOwner
	ownedString, err := source.Body.GetString(11)
	if err != nil {
		t.Fatal(err)
	}
	wantWire, err := source.AppendTo(make([]byte, 0, 4096))
	if err != nil {
		t.Fatal(err)
	}
	wantBody := bytes.Clone(source.bodyBytes)
	wantFields := cloneOwnedField(source.fields)
	owned := NewMessage()
	// A previously parsed destination must not keep returning its stale frame.
	if err := ParseMessage(owned, bytes.NewBuffer(buildFIXT11Wire("35=0\x01", ""))); err != nil {
		t.Fatal(err)
	}
	source.CopyInto(owned)
	valueCopy := *source
	source.CopyInto(&valueCopy)
	bodyValueCopy := source.Body.FieldMap
	source.Body.CopyInto(&bodyValueCopy)
	source.CopyInto(source)
	if source.inbound != frameOwner || source.parseSlot != slot {
		t.Fatal("CopyInto changed the source scratch ownership")
	}
	for _, copied := range []*Message{owned, &valueCopy} {
		if copied.inbound != nil || copied.parseSlot != nil {
			t.Fatal("owned copy retained the source scratch ownership")
		}
	}
	var body FieldMap
	body.init()
	source.Body.CopyInto(&body)
	var fixedBody FieldMap
	fixedBody.initFixed(normalFieldOrder, DefaultParseLimits().MaxFields)
	source.Body.CopyInto(&fixedBody)
	group := scopedAllocationGroup()
	if err := source.Body.GetGroup(group); err != nil {
		t.Fatal(err)
	}
	var copiedEntry FieldMap
	copiedEntry.init()
	group.Get(0).CopyInto(&copiedEntry)
	hops := NewRepeatingGroup(627, GroupTemplate{GroupElement(628)})
	if err := source.Header.GetGroup(hops); err != nil {
		t.Fatal(err)
	}

	for index := range wire {
		wire[index] = 'x'
	}
	slot.Reset()
	if err := slot.Parse(buildFIXT11Wire("35=0\x01", "")); err != nil {
		t.Fatal(err)
	}
	if ownedString != "original" {
		t.Fatalf("string changed after frame reuse: %q", ownedString)
	}
	for _, fields := range []*FieldMap{&owned.Body.FieldMap, &valueCopy.Body.FieldMap, &bodyValueCopy, &body, &fixedBody} {
		if value, err := fields.GetString(11); err != nil || value != "original" {
			t.Fatalf("copied scalar changed: %q, %v", value, err)
		}
		allocations := scopedAllocationGroup()
		if err := fields.GetGroup(allocations); err != nil {
			t.Fatal(err)
		}
		assertScopedAllocationGroup(t, allocations)
		parties := NewRepeatingGroup(453, GroupTemplate{GroupElement(448), GroupElement(447), GroupElement(452)})
		if err := fields.GetGroup(parties); err != nil || parties.Len() != 1 {
			t.Fatalf("sibling group was not copied: %v", err)
		}
		if value, err := parties.Get(0).GetString(448); err != nil || value != "P" {
			t.Fatalf("sibling group changed: %q, %v", value, err)
		}
	}
	assertScopedAllocationGroup(t, group)
	if value, err := copiedEntry.GetString(79); err != nil || value != "A" {
		t.Fatalf("copy of an extracted group changed: %q, %v", value, err)
	}
	if hops.Len() != 2 {
		t.Fatalf("header group length changed: %d", hops.Len())
	}
	if value, err := hops.Get(1).GetString(628); err != nil || value != "H2" {
		t.Fatalf("header group changed: %q, %v", value, err)
	}
	if !bytes.Equal(owned.bodyBytes, wantBody) || len(owned.fields) != len(wantFields) {
		t.Fatal("copied raw body or field index changed")
	}
	for index, field := range owned.fields {
		if !bytes.Equal(field.value, wantFields[index].value) || !bytes.Equal(field.bytes, wantFields[index].bytes) {
			t.Fatalf("copied field %d changed after frame reuse", index)
		}
	}
	gotWire, err := owned.AppendTo(make([]byte, 0, 4096))
	if err != nil || !bytes.Equal(gotWire, wantWire) {
		t.Fatalf("copied message wire differs: %q, %v", gotWire, err)
	}
	if owned.String() != string(wantWire) {
		t.Fatal("copied message retained stale or borrowed raw bytes")
	}
}

func scopedAllocationGroup() *RepeatingGroup {
	return NewRepeatingGroup(78, GroupTemplate{
		GroupElement(79),
		NewRepeatingGroup(539, GroupTemplate{
			GroupElement(524),
			NewRepeatingGroup(804, GroupTemplate{GroupElement(545)}),
		}),
		GroupElement(80),
	})
}

func assertScopedAllocationGroup(t *testing.T, group *RepeatingGroup) {
	t.Helper()
	if group.Len() != 2 {
		t.Fatalf("allocation group length = %d", group.Len())
	}
	if value, err := group.Get(1).GetString(79); err != nil || value != "B" {
		t.Fatalf("second allocation changed: %q, %v", value, err)
	}
	nested := NewRepeatingGroup(539, GroupTemplate{GroupElement(524), NewRepeatingGroup(804, GroupTemplate{GroupElement(545)})})
	if err := group.Get(0).GetGroup(nested); err != nil || nested.Len() != 1 {
		t.Fatalf("nested group was not retained: %v", err)
	}
	leaf := NewRepeatingGroup(804, GroupTemplate{GroupElement(545)})
	if err := nested.Get(0).GetGroup(leaf); err != nil || leaf.Len() != 1 {
		t.Fatalf("leaf group was not retained: %v", err)
	}
	if value, err := leaf.Get(0).GetString(545); err != nil || value != "X" {
		t.Fatalf("leaf value changed: %q, %v", value, err)
	}
}

func TestScopedSettersDetachOnlyOnCapacityAndRestoreSlot(t *testing.T) {
	for _, setter := range []struct {
		name string
		set  func(*FieldMap)
	}{
		{"bytes", func(fields *FieldMap) { fields.SetBytes(5000, []byte("value")) }},
		{"string", func(fields *FieldMap) { fields.SetString(5000, "value") }},
		{"integer", func(fields *FieldMap) { fields.SetInt(5000, 123) }},
		{"boolean", func(fields *FieldMap) { fields.SetBool(5000, true) }},
		{"field", func(fields *FieldMap) { fields.SetField(5000, FIXString("value")) }},
		{"group", func(fields *FieldMap) {
			group := NewRepeatingGroup(5000, GroupTemplate{GroupElement(5001)})
			group.Add().SetString(5001, "value")
			fields.SetGroup(group)
		}},
	} {
		t.Run(setter.name, func(t *testing.T) {
			for section := 0; section < 3; section++ {
				slot, err := NewMessageSlot(nil, nil, ParseLimits{1024, 4, 0, 0})
				if err != nil {
					t.Fatal(err)
				}
				slot.enableScopedCallbacks()
				wire := buildFIXT11Wire("35=0\x01", "")
				if err := slot.Parse(wire); err != nil {
					t.Fatal(err)
				}
				sections := [3]*FieldMap{&slot.message.Header.FieldMap, &slot.message.Body.FieldMap, &slot.message.Trailer.FieldMap}
				fields := sections[section]
				backing := fields.fixed
				for tag := Tag(1000); fields.fieldCount() < 4; tag++ {
					fields.SetString(tag, "original")
				}
				fields.SetString(1000, "replacement")
				if fields.fixed != backing {
					t.Fatal("setter detached before capacity exhaustion")
				}
				setter.set(fields)
				if fields.fixed != nil || !fields.Has(5000) || fields.fieldCount() != 5 {
					t.Fatal("exhausted setter did not detach to an ordinary map")
				}
				if value, err := fields.GetString(1000); err != nil || value != "replacement" {
					t.Fatalf("detachment lost existing values: %q, %v", value, err)
				}
				fields.SetString(5002, "unbounded callback convenience")
				slot.message.inbound = &inboundSlot{}
				slot.Reset()
				if fields.fixed != backing || fields.tagLookup != nil || fields.rwLock != nil || !fields.scoped || fields.fieldCount() != 0 || slot.message.inbound != nil || slot.message.parseSlot != slot || len(backing.touched) != 0 {
					t.Fatal("reset did not restore fixed storage and release callback metadata")
				}
				for _, entry := range backing.entries {
					if entry.state != fixedFieldVacant || entry.values != nil || entry.scalar[0].value != nil || entry.scalar[0].bytes != nil {
						t.Fatal("reset retained detached callback field references")
					}
				}
				if err := slot.Parse(wire); err != nil || fields.Has(5000) {
					t.Fatalf("slot could not be reused after setter detachment: %v", err)
				}
			}
		})
	}
}

func TestScopedParserNeverDetachesOnExhaustion(t *testing.T) {
	slot, err := NewMessageSlot(nil, nil, ParseLimits{1024, 4, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	slot.enableScopedCallbacks()
	backing := slot.message.Body.fixed
	if err := slot.Parse(buildFIXT11Wire("35=D\x01", "11=too-many-fields\x01")); !errors.Is(err, ErrFieldCapacity) {
		t.Fatalf("parse exhaustion = %v", err)
	}
	if slot.Message() != nil || slot.message.Body.fixed != backing || slot.message.Body.tagLookup != nil {
		t.Fatal("parser exposed a message or detached on exhaustion")
	}
	wire := buildFIXT11Wire("35=0\x01", "")
	if err := slot.Parse(wire); err != nil {
		t.Fatal(err)
	}
	for tag := Tag(1000); tag < 1005; tag++ {
		slot.message.Body.SetString(tag, "callback value")
	}
	if slot.message.Body.fixed != nil {
		t.Fatal("fixture did not exhaust setter capacity")
	}
	// Parse also restores original storage if the caller did not Reset first.
	if err := slot.Parse(buildFIXT11Wire("35=D\x01", "11=too-many-fields\x01")); !errors.Is(err, ErrFieldCapacity) {
		t.Fatalf("parse after detachment = %v", err)
	}
	if slot.message.Body.fixed != backing || slot.message.Body.Has(1000) {
		t.Fatal("parser reused the detached allocating map")
	}
	strict, err := NewMessageSlot(nil, nil, ParseLimits{1024, 4, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	for tag := Tag(1000); tag < 1004; tag++ {
		strict.message.Body.SetString(tag, "strict")
	}
	assertFixedFieldCapacityPanic(t, func() { strict.message.Body.SetString(1004, "overflow") })
}

func TestScopedCopyDestinationRestoresOriginalParseStorage(t *testing.T) {
	slot, err := NewMessageSlot(nil, nil, ParseLimits{1024, 4, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	slot.enableScopedCallbacks()
	if err := slot.Parse(buildFIXT11Wire("35=0\x01", "")); err != nil {
		t.Fatal(err)
	}
	backing := slot.message.Body.fixed
	fields := slot.message.fields[:cap(slot.message.fields)]
	frameOwner := &inboundSlot{}
	slot.message.inbound = frameOwner
	source := NewMessage()
	wire := buildFIXT11Wire("35=D\x01", "1000=A\x011001=B\x011002=C\x011003=D\x011004=E\x01")
	if err := ParseMessage(source, bytes.NewBuffer(wire)); err != nil {
		t.Fatal(err)
	}
	source.CopyInto(slot.Message())
	if slot.message.Body.fixed != nil || slot.message.Body.fieldCount() != 5 || cap(slot.message.fields) <= 4 {
		t.Fatal("CopyInto did not preserve unlimited callback convenience behavior")
	}
	if slot.message.inbound != frameOwner || slot.message.parseSlot != slot {
		t.Fatal("CopyInto released the actual destination scratch ownership")
	}
	valueCopy := slot.message
	source.CopyInto(&valueCopy)
	if valueCopy.inbound != nil || valueCopy.parseSlot != nil || valueCopy.rawMessage != nil {
		t.Fatal("ordinary source copy retained a value copy's scratch ownership")
	}
	slot.Reset()
	if slot.message.Body.fixed != backing || cap(slot.message.fields) != 4 || &slot.message.fields[:cap(slot.message.fields)][0] != &fields[0] {
		t.Fatal("CopyInto replaced the original parse storage permanently")
	}
	if err := slot.Parse(wire); !errors.Is(err, ErrFieldCapacity) {
		t.Fatalf("parser used allocating CopyInto storage after reset: %v", err)
	}
}

func TestScopedMessageCopyOwnsReplacedSection(t *testing.T) {
	slot, err := NewMessageSlot(nil, nil, DefaultParseLimits())
	if err != nil {
		t.Fatal(err)
	}
	slot.enableScopedCallbacks()
	wire := buildFIXT11Wire("35=D\x01", "11=original\x01")
	if err := slot.Parse(wire); err != nil {
		t.Fatal(err)
	}
	source := slot.Message()
	value, err := source.Body.GetBytes(11)
	if err != nil {
		t.Fatal(err)
	}
	var replacement FieldMap
	replacement.init()
	replacement.SetBytes(11, value)
	source.Body.FieldMap = replacement
	owned := NewMessage()
	source.CopyInto(owned)
	for index := range wire {
		wire[index] = 'x'
	}
	slot.Reset()
	if got, err := owned.Body.GetString(11); err != nil || got != "original" {
		t.Fatalf("copied replacement section retained scoped bytes: %q, %v", got, err)
	}
}

func TestScopedFieldMapCopyUsesSharedCurrentTags(t *testing.T) {
	var source, owned FieldMap
	source.initFixed(normalFieldOrder, 4)
	source.scoped = true
	owned.init()
	source.SetString(11, "original")
	view := source
	source.SetString(55, "symbol")
	source.Remove(11)
	source.SetString(54, "1")
	source.SetString(44, "10")
	view.CopyInto(&owned)
	source.Clear()
	if owned.fieldCount() != 3 || owned.Has(11) {
		t.Fatal("copy did not use the shared backing's current fields")
	}
	var wire bytes.Buffer
	owned.write(&wire)
	if got := wire.String(); got != "44=10\x0154=1\x0155=symbol\x01" {
		t.Fatalf("copy used a stale value-copy tag index: %q", got)
	}
}

func TestScopedValueCopyDestinationDoesNotMutateOriginal(t *testing.T) {
	slot, err := NewMessageSlot(nil, nil, DefaultParseLimits())
	if err != nil {
		t.Fatal(err)
	}
	slot.enableScopedCallbacks()
	if err := slot.Parse(buildFIXT11Wire("35=D\x01", "11=original\x01")); err != nil {
		t.Fatal(err)
	}
	original := slot.Message()
	frameOwner := &inboundSlot{}
	original.inbound = frameOwner
	ordinary := NewMessage()
	if err := ParseMessage(ordinary, bytes.NewBuffer(buildFIXT11Wire("35=F\x01", "11=replacement\x01"))); err != nil {
		t.Fatal(err)
	}
	messageCopy := *original
	ordinary.CopyInto(&messageCopy)
	bodyCopy := original.Body.FieldMap
	ordinary.Body.CopyInto(&bodyCopy)
	if got, err := original.Body.GetString(11); err != nil || got != "original" {
		t.Fatalf("copying into a value copy changed the original body: %q, %v", got, err)
	}
	if got, err := original.Header.GetString(35); err != nil || got != "D" {
		t.Fatalf("copying into a value copy changed the original header: %q, %v", got, err)
	}
	if original.inbound != frameOwner || original.parseSlot != slot || messageCopy.inbound != nil || messageCopy.parseSlot != nil {
		t.Fatal("copying into a value copy transferred scratch ownership")
	}
	slot.Reset()
	for _, fields := range []*FieldMap{&messageCopy.Body.FieldMap, &bodyCopy} {
		if got, err := fields.GetString(11); err != nil || got != "replacement" {
			t.Fatalf("value copy changed after original reset: %q, %v", got, err)
		}
	}
}

func TestScopedFixedResetClearsTouchedEntriesAfterChurn(t *testing.T) {
	var fields FieldMap
	fields.initFixed(normalFieldOrder, 4)
	backing := fields.fixed
	touchedStorage := &backing.touched[:cap(backing.touched)][0]
	fields.SetString(11, "first")
	for range 20 {
		fields.Remove(11)
		fields.SetString(11, "replacement")
	}
	if len(backing.touched) != 1 {
		t.Fatalf("reusing a tombstone recorded duplicate touches: %d", len(backing.touched))
	}
	for tag := Tag(1000); tag < 2000; tag++ {
		fields.SetString(tag, "transient")
		fields.Remove(tag)
	}
	if &backing.touched[:cap(backing.touched)][0] != touchedStorage || len(backing.touched) > len(backing.entries) {
		t.Fatal("tag churn grew touched-entry storage")
	}
	seen := make(map[Tag]bool)
	for _, index := range backing.touched {
		if seen[index] {
			t.Fatal("touched-entry index contains duplicates")
		}
		seen[index] = true
	}
	fields.Clear()
	if len(backing.touched) != 0 || fields.fieldCount() != 0 {
		t.Fatal("reset retained active tag indexes")
	}
	for _, entry := range backing.entries {
		if entry.state != fixedFieldVacant || entry.tag != 0 || entry.values != nil || entry.scalar[0].value != nil || entry.scalar[0].bytes != nil {
			t.Fatal("reset retained a touched field or scalar reference")
		}
	}
	fields.SetString(55, "reused")
	if value, err := fields.GetString(55); err != nil || value != "reused" || len(backing.touched) != 1 {
		t.Fatalf("lookup failed after touched reset: %q, %v", value, err)
	}
}

func TestMessageSlotResetClearsHighWaterAfterShrinkAndFailure(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, failure := range []struct {
			name  string
			frame []byte
		}{
			{"malformed", buildFIXT11Wire("35=D\x01", "11=A\x0155=B\x01invalid\x01")},
			{"field_capacity", buildFIXT11Wire("35=D\x01", "11=A\x0155=B\x0154=1\x0144=10\x0138=5\x0140=2\x0159=0\x0160=T\x01")},
			{"frame_capacity", bytes.Repeat([]byte{'x'}, 1025)},
		} {
			t.Run(failure.name+map[bool]string{false: "/strict", true: "/scoped"}[scoped], func(t *testing.T) {
				slot, err := NewMessageSlot(nil, nil, ParseLimits{1024, 8, 0, 0})
				if err != nil {
					t.Fatal(err)
				}
				if scoped {
					slot.enableScopedCallbacks()
				}
				if err := slot.Parse(buildFIXT11Wire("35=D\x01", "11=A\x0155=B\x0154=1\x01")); err != nil {
					t.Fatal(err)
				}
				firstUsed := slot.fieldsUsed
				if err := slot.Parse(buildFIXT11Wire("35=0\x01", "")); err != nil {
					t.Fatal(err)
				}
				if slot.fieldsUsed != firstUsed || firstUsed <= len(slot.message.fields) {
					t.Fatal("shorter parse lost the prior high-water mark")
				}
				if err := slot.Parse(failure.frame); err == nil {
					t.Fatal("invalid frame unexpectedly parsed")
				}
				slot.Reset()
				if slot.fieldsUsed != 0 || len(slot.message.fields) != 0 {
					t.Fatal("reset retained the field high-water mark")
				}
				for _, value := range slot.message.fields[:cap(slot.message.fields)] {
					if value.tag != 0 || value.value != nil || value.bytes != nil {
						t.Fatal("reset retained bytes from a prior parse or failed extraction")
					}
				}
			})
		}
	}
}

func TestMessageSlotLegacyReparseReleasesWholeOriginalFieldSlab(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		slot, err := NewMessageSlot(nil, nil, ParseLimits{1024, 16, 0, 0})
		if err != nil {
			t.Fatal(err)
		}
		if scoped {
			slot.enableScopedCallbacks()
		}
		if err := slot.Parse(buildFIXT11Wire("35=0\x01", "")); err != nil {
			t.Fatal(err)
		}
		source := slot.Message()
		originalFields := source.fields[:cap(source.fields)]
		longer := buildFIXT11Wire("35=D\x01", "11=A\x0155=B\x0154=1\x0144=10\x0138=5\x01")
		if err := ParseMessage(source, bytes.NewBuffer(longer)); err != nil {
			t.Fatal(err)
		}
		if err := ParseMessage(source, bytes.NewBuffer(buildFIXT11Wire("35=0\x01", ""))); err != nil {
			t.Fatal(err)
		}
		if originalFields[7].value == nil {
			t.Fatal("fixture did not leave a reference beyond the bounded parser's high-water mark")
		}
		slot.Reset()
		for _, value := range originalFields {
			if value.tag != 0 || value.value != nil || value.bytes != nil {
				t.Fatal("reset retained bytes from callback convenience parsing")
			}
		}
		if slot.fieldsUsed != 0 || cap(source.fields) != len(originalFields) {
			t.Fatal("reset did not restore the bounded parser storage")
		}
	}
}

func TestMessageSlotCopyIntoRestoresStrictParseStorage(t *testing.T) {
	slot, err := NewMessageSlot(nil, nil, ParseLimits{1024, 8, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	originalFields := slot.message.fields[:cap(slot.message.fields)]
	wire := buildFIXT11Wire("35=D\x01", "11=A\x0155=B\x0154=1\x01")
	for range 2 {
		if err := slot.Parse(wire); err != nil {
			t.Fatal(err)
		}
		NewMessage().CopyInto(slot.Message())
		if len(slot.message.fields) != 0 || cap(slot.message.fields) != 0 {
			t.Fatal("fixture did not replace the parse slice with a shorter copy")
		}
		slot.Reset()
		if cap(slot.message.fields) != len(originalFields) || &slot.message.fields[:cap(slot.message.fields)][0] != &originalFields[0] {
			t.Fatal("reset did not restore the strict slot's original parse storage")
		}
		for _, value := range originalFields {
			if value.tag != 0 || value.value != nil || value.bytes != nil {
				t.Fatal("reset retained the strict slot's prior frame bytes")
			}
		}
	}
	tooMany := buildFIXT11Wire("35=D\x01", "11=A\x0155=B\x0154=1\x0144=10\x0138=5\x01")
	if err := slot.Parse(tooMany); !errors.Is(err, ErrFieldCapacity) {
		t.Fatalf("copy changed the strict parser capacity: %v", err)
	}
}

func TestMessageSlotResetReleasesGroupLookaheadOnError(t *testing.T) {
	group := datadictionary.NewGroupFieldDef(datadictionary.NewFieldType("Group", 9000, "NUMINGROUP"), false,
		[]datadictionary.MessagePart{
			datadictionary.NewFieldDef(datadictionary.NewFieldType("Entry", 9001, "STRING"), true),
			datadictionary.NewFieldDef(datadictionary.NewFieldType("XMLDataLen", 212, "LENGTH"), false),
		})
	dictionary := &datadictionary.DataDictionary{Messages: map[string]*datadictionary.MessageDef{
		"D": datadictionary.NewMessageDef("WithGroup", "D", []datadictionary.MessagePart{group}),
	}}
	slot, err := NewMessageSlot(nil, dictionary, ParseLimits{1024, 16, 1, 1})
	if err != nil {
		t.Fatal(err)
	}
	wire := buildFIXT11Wire("35=D\x01", "9000=1\x019001=entry\x01212=invalid\x0155=lookahead\x01")
	if err := slot.Parse(wire); !errors.Is(err, ErrInvalidUnsignedValue) {
		t.Fatalf("invalid length after group lookahead: %v", err)
	}
	fields := slot.message.fields[:cap(slot.message.fields)]
	if string(fields[6].value) != "lookahead" {
		t.Fatal("fixture did not extract and rewind the group boundary")
	}
	slot.Reset()
	for _, value := range fields {
		if value.tag != 0 || value.value != nil || value.bytes != nil {
			t.Fatal("reset retained the rewound group boundary field")
		}
	}
}

func TestMessageSlotLegacyReparseCannotEnlargeGroupDepth(t *testing.T) {
	dictionary, err := datadictionary.Parse("spec/FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	nested := buildFIXT11Wire("35=D\x01", "78=1\x0179=A\x01539=1\x01524=N\x01")
	for _, reset := range []bool{false, true} {
		slot, err := NewMessageSlot(dictionary, dictionary, ParseLimits{1024, 32, 4, 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := slot.Parse(buildFIXT11Wire("35=0\x01", "")); err != nil {
			t.Fatal(err)
		}
		originalStack := slot.message.groupStack[:cap(slot.message.groupStack)]
		if err := ParseMessageWithDataDictionary(slot.Message(), bytes.NewBuffer(nested), dictionary, dictionary); err != nil {
			t.Fatal(err)
		}
		if cap(slot.message.groupStack) <= slot.limits.MaxGroupDepth {
			t.Fatal("fixture did not enlarge the legacy parser's group stack")
		}
		if reset {
			slot.Reset()
			if originalStack[0] != nil {
				t.Fatal("reset retained the original group stack reference")
			}
		}
		if err := slot.Parse(nested); !errors.Is(err, ErrGroupDepth) {
			t.Fatalf("legacy reparse bypassed configured group depth (reset=%v): %v", reset, err)
		}
		if cap(slot.message.groupStack) != len(originalStack) || &slot.message.groupStack[:cap(slot.message.groupStack)][0] != &originalStack[0] {
			t.Fatal("bounded parser did not restore its original fixed group stack")
		}
	}
}

func TestOrdinaryFieldMapCopyIntoKeepsExistingByteSemantics(t *testing.T) {
	var source, destination FieldMap
	source.init()
	destination.init()
	value := []byte("original")
	source.SetBytes(11, value)
	source.CopyInto(&destination)
	copy(value, "modified")
	if got, err := destination.GetString(11); err != nil || got != "modified" {
		t.Fatalf("ordinary CopyInto byte semantics changed: %q, %v", got, err)
	}
}
