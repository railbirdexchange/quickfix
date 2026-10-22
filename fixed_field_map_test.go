package quickfix

import (
	"bytes"
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestFixedFieldMapReadersAndValueCopies(t *testing.T) {
	var fields FieldMap
	fields.initFixed(normalFieldOrder, 5)
	if fields.tagLookup != nil || fields.rwLock != nil {
		t.Fatal("fixed backing created a legacy map or mutex")
	}
	copyOfFields := fields
	fields.SetString(1, "hello")
	fields.SetInt(2, 256)
	fields.SetBool(3, true)
	fields.SetString(52, "20160208-22:07:16.954")
	if value, err := copyOfFields.GetString(1); err != nil || value != "hello" {
		t.Fatalf("GetString from value copy = %q, %v", value, err)
	}
	if value, err := copyOfFields.GetInt(2); err != nil || value != 256 {
		t.Fatalf("GetInt = %d, %v", value, err)
	}
	if value, err := copyOfFields.GetBool(3); err != nil || !value {
		t.Fatalf("GetBool = %t, %v", value, err)
	}
	expectedTime := time.Date(2016, time.February, 8, 22, 7, 16, 954000000, time.UTC)
	if value, err := copyOfFields.GetTime(52); err != nil || !value.Equal(expectedTime) {
		t.Fatalf("GetTime = %v, %v", value, err)
	}
	if _, err := copyOfFields.GetBytes(999); err == nil {
		t.Fatal("missing tag did not retain reject behavior")
	}
	if _, err := copyOfFields.GetInt(1); err == nil {
		t.Fatal("malformed integer did not retain reject behavior")
	}
	copyOfFields.Remove(2)
	if fields.Has(2) || fields.fieldCount() != 3 || len(fields.fieldTags()) != 3 {
		t.Fatal("removal through value copy did not update canonical backing")
	}
	copyOfFields.SetString(4, "added through copy")
	if !fields.Has(4) || !slices.Contains(fields.fieldTags(), Tag(4)) {
		t.Fatal("addition through value copy was not visible")
	}
	copyOfFields.Clear()
	if fields.fieldCount() != 0 || len(fields.fieldTags()) != 0 || fields.Has(1) {
		t.Fatal("clear through value copy did not reset canonical backing")
	}
}

func TestFixedFieldMapBorrowedGroupsAndWireAccounting(t *testing.T) {
	var fields FieldMap
	fields.initFixed(normalFieldOrder, 4)
	values := make([]TagValue, 4)
	for index, wire := range []string{"453=1\x01", "448=party\x01", "447=D\x01", "452=1\x01"} {
		if err := values[index].parse([]byte(wire)); err != nil {
			t.Fatal(err)
		}
	}
	if !fields.put(453, values) {
		t.Fatal("group range unexpectedly exceeded capacity")
	}
	group := NewRepeatingGroup(453, GroupTemplate{GroupElement(448), GroupElement(447), GroupElement(452)})
	if err := fields.GetGroup(group); err != nil || group.Len() != 1 {
		t.Fatalf("GetGroup = %d members, %v", group.Len(), err)
	}
	if party, err := group.Get(0).GetString(448); err != nil || party != "party" {
		t.Fatalf("group party = %q, %v", party, err)
	}
	const wire = "453=1\x01448=party\x01447=D\x01452=1\x01"
	var buffer bytes.Buffer
	fields.write(&buffer)
	if buffer.String() != wire || fields.length() != len(wire) || fields.total() != bytesTotal([]byte(wire)) {
		t.Fatal("borrowed group range was not retained for encoding or accounting")
	}
	fields.SetString(453, "0")
	stored, found := fields.lookup(453)
	if !found || len(stored) != 1 || string(stored[0].value) != "0" {
		t.Fatal("scalar replacement retained the former group tail")
	}
}

func TestFixedFieldMapCapacityFailureIsAtomic(t *testing.T) {
	var fields FieldMap
	fields.initFixed(normalFieldOrder, 1)
	fields.SetString(11, "original")
	beforeEntries := append([]fixedFieldEntry(nil), fields.fixed.entries...)
	beforeTags := append([]Tag(nil), fields.fieldTags()...)
	newValue := []TagValue{{tag: 55, value: []byte("symbol")}}
	if fields.put(55, newValue) {
		t.Fatal("put succeeded after field slots were exhausted")
	}
	setters := []struct {
		name string
		set  func()
	}{
		{"bytes", func() { fields.SetBytes(55, []byte("symbol")) }},
		{"string", func() { fields.SetString(55, "symbol") }},
		{"integer", func() { fields.SetInt(55, 123) }},
		{"boolean", func() { fields.SetBool(55, true) }},
		{"field", func() { fields.SetField(55, FIXString("symbol")) }},
		{"group", func() { fields.SetGroup(NewRepeatingGroup(453, GroupTemplate{GroupElement(448)})) }},
	}
	for _, setter := range setters {
		t.Run(setter.name, func(t *testing.T) {
			assertFixedFieldCapacityPanic(t, setter.set)
			if !reflect.DeepEqual(fields.fixed.entries, beforeEntries) || !slices.Equal(fields.fieldTags(), beforeTags) {
				t.Fatal("capacity failure changed fixed backing")
			}
		})
	}
	fields.SetString(11, "replacement")
	if value, err := fields.GetString(11); err != nil || value != "replacement" {
		t.Fatal("existing field could not be replaced at capacity")
	}
}

func TestFixedFieldMapDisjointTagChurnIsBounded(t *testing.T) {
	var fields FieldMap
	fields.initFixed(normalFieldOrder, 3)
	entries := &fields.fixed.entries[0]
	tags := &fields.fixed.tags[:cap(fields.fixed.tags)][0]
	values := [3]TagValue{}
	for generation := 0; generation < 1000; generation++ {
		for index := range values {
			values[index].tag = Tag(generation*97 + index*17)
			if !fields.put(values[index].tag, values[index:index+1]) {
				t.Fatal("vacant field capacity was lost during disjoint tag churn")
			}
		}
		for index := range values {
			if !fields.Has(values[index].tag) {
				t.Fatal("inserted tag is missing")
			}
		}
		for index := range values {
			fields.Remove(values[index].tag)
		}
		if fields.fieldCount() != 0 || cap(fields.fixed.tags) != 3 || &fields.fixed.entries[0] != entries || &fields.fixed.tags[:cap(fields.fixed.tags)][0] != tags {
			t.Fatal("remove/reinsert changed backing storage or retained tag indexes")
		}
	}
}

func TestFixedFieldMapCopyIntoAndOrdering(t *testing.T) {
	var source, legacy, bounded FieldMap
	source.initFixed(headerFieldOrdering, 4)
	source.SetString(35, "D")
	source.SetString(9, "100")
	source.SetString(8, "FIX.4.4")
	source.SetString(1, "account")
	legacy.init()
	bounded.initFixed(normalFieldOrder, 4)
	for _, destination := range []*FieldMap{&legacy, &bounded} {
		source.CopyInto(destination)
		if !slices.Equal(destination.sortedTags(), []Tag{8, 9, 35, 1}) {
			t.Fatalf("CopyInto changed ordering: %v", destination.fieldTags())
		}
		if value, err := destination.GetString(1); err != nil || value != "account" {
			t.Fatalf("CopyInto value = %q, %v", value, err)
		}
	}
	var tooSmall FieldMap
	tooSmall.initFixed(normalFieldOrder, 1)
	tooSmall.SetString(55, "unchanged")
	assertFixedFieldCapacityPanic(t, func() { source.CopyInto(&tooSmall) })
	if value, err := tooSmall.GetString(55); err != nil || value != "unchanged" || tooSmall.fieldCount() != 1 {
		t.Fatal("failed CopyInto mutated the destination")
	}
	source.Clear()
	if value, err := bounded.GetString(1); err != nil || value != "account" {
		t.Fatal("clearing source destroyed copied fields")
	}
}

func TestFixedFieldMapInvalidCapacity(t *testing.T) {
	maximumInt := int(^uint(0) >> 1)
	for _, capacity := range []int{0, -1, maximumInt, maximumInt/2 + 1, maximumInt / 2} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("initFixed accepted invalid capacity %d", capacity)
				}
			}()
			var fields FieldMap
			fields.initFixed(normalFieldOrder, capacity)
		}()
	}
}

func assertFixedFieldCapacityPanic(t *testing.T, operation func()) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != ErrFieldCapacity {
			t.Fatalf("panic = %v, want ErrFieldCapacity", recovered)
		}
	}()
	operation()
}
