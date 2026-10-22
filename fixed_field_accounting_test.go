package quickfix

import (
	"slices"
	"testing"
)

func TestFixedFieldIndexesKeepDisjointCapacities(test *testing.T) {
	var fields FieldMap
	fields.initFixed(normalFieldOrder, 4)
	if cap(fields.fixed.tags) != 4 || cap(fields.fixed.touched) != len(fields.fixed.entries) {
		test.Fatal("shared storage changed either configured index capacity")
	}
	for _, tag := range []Tag{400, 300, 200, 100} {
		fields.SetString(tag, "value")
	}
	touched := slices.Clone(fields.fixed.touched)
	if !slices.Equal(fields.sortedTags(), []Tag{100, 200, 300, 400}) || !slices.Equal(touched, fields.fixed.touched) {
		test.Fatal("sorting tags changed lifecycle indexes")
	}
	fields.Remove(200)
	if !slices.Equal(touched, fields.fixed.touched) {
		test.Fatal("removing a tag changed lifecycle indexes")
	}
	fields.Clear()
	for _, entry := range fields.fixed.entries {
		if entry.state != fixedFieldVacant || entry.values != nil || entry.scalar[0].value != nil || entry.scalar[0].bytes != nil {
			test.Fatal("reset retained a shared-index field reference")
		}
	}
}

// Calculate reference accounting from the complete physical table, independent
// of the lifecycle indexes used by the optimized accounting implementation.
func assertFixedFieldAccounting(test *testing.T, fields *FieldMap) {
	test.Helper()
	var length, total int
	for index := range fields.fixed.entries {
		entry := &fields.fixed.entries[index]
		if len(entry.values) == 0 {
			continue
		}
		for _, value := range entry.values {
			if value.tag != tagBeginString && value.tag != tagBodyLength && value.tag != tagCheckSum {
				length += value.length()
			}
			if value.tag != tagCheckSum {
				total += value.total()
			}
		}
	}
	if fields.length() != length || fields.total() != total {
		test.Fatalf("accounting length=%d total=%d, want length=%d total=%d", fields.length(), fields.total(), length, total)
	}
}

func TestFixedFieldAccountingAcrossTombstonesAndReset(test *testing.T) {
	var fields FieldMap
	fields.initFixed(normalFieldOrder, 8)
	copyOfFields := fields
	var values [8]TagValue
	for index := range values {
		values[index].init(Tag(1000+index), []byte("value"))
	}
	for generation := range 300 {
		for index := range values {
			tag := Tag(1000 + generation*8 + index)
			values[index].tag = tag
			if !fields.put(tag, values[index:index+1]) {
				test.Fatal("churn exhausted fixed field storage")
			}
		}
		assertFixedFieldAccounting(test, &copyOfFields)
		for index := range values {
			copyOfFields.Remove(values[index].tag)
			assertFixedFieldAccounting(test, &fields)
		}
		if generation%17 == 0 {
			copyOfFields.Clear()
			assertFixedFieldAccounting(test, &fields)
			for _, entry := range fields.fixed.entries {
				if entry.tag != 0 || entry.values != nil || entry.scalar[0].value != nil || entry.scalar[0].bytes != nil {
					test.Fatal("reset retained removed field storage")
				}
			}
		}
	}
}

func TestFixedFieldAccountingExclusionsAndGroupReplacement(test *testing.T) {
	var fields FieldMap
	fields.initFixed(normalFieldOrder, 8)
	for _, tag := range []Tag{tagBeginString, tagBodyLength, tagCheckSum, 11, 55} {
		fields.SetString(tag, "123")
	}
	group := []TagValue{{}, {}, {}, {}}
	group[0].init(453, []byte("1"))
	group[1].init(448, []byte("party"))
	group[2].init(447, []byte("D"))
	group[3].init(452, []byte("1"))
	if !fields.put(453, group) {
		test.Fatal("group exceeded fixed capacity")
	}
	assertFixedFieldAccounting(test, &fields)
	fields.Remove(11)
	fields.SetString(11, "longer replacement")
	assertFixedFieldAccounting(test, &fields)
	fields.SetString(453, "0")
	assertFixedFieldAccounting(test, &fields)
	fields.Clear()
	assertFixedFieldAccounting(test, &fields)
	if !fields.put(453, group) {
		test.Fatal("group could not be inserted after clear")
	}
	assertFixedFieldAccounting(test, &fields)
}
