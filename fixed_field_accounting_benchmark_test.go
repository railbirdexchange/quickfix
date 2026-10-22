package quickfix

import (
	"reflect"
	"strconv"
	"testing"
)

var fixedFieldAccountingResult int
var fixedFieldSetupResult FieldMap

func TestFixedFieldAccountingLayout(test *testing.T) {
	entry := reflect.TypeOf(fixedFieldEntry{})
	test.Logf("entry_bytes=%d backing_bytes=%d int_bytes=%d", entry.Size(), reflect.TypeOf(fixedFieldMap{}).Size(), reflect.TypeOf(int(0)).Size())
	for index := 0; index < entry.NumField(); index++ {
		field := entry.Field(index)
		test.Logf("field=%s offset=%d size=%d", field.Name, field.Offset, field.Type.Size())
	}
}

func newFixedFieldAccountingFixture(test testing.TB, capacity, populated, removed int, grouped bool) FieldMap {
	test.Helper()
	var fields FieldMap
	fields.initFixed(normalFieldOrder, capacity)
	for index := 0; index < populated; index++ {
		tag := Tag(1000 + index)
		values := make([]TagValue, 1)
		values[0].init(tag, []byte("12345.6789"))
		if grouped {
			values = make([]TagValue, 4)
			values[0].init(tag, []byte("1"))
			values[1].init(448, []byte("PARTY"))
			values[2].init(447, []byte("D"))
			values[3].init(452, []byte("1"))
		}
		if !fields.put(tag, values) {
			test.Fatal("fixture exceeded configured field capacity")
		}
	}
	for index := 0; index < removed; index++ {
		fields.Remove(Tag(1000 + index))
	}
	return fields
}

// Setup is measured separately from warmed accounting. Results escape so the
// benchmark includes the fixed lookup, tag index and lifecycle bookkeeping.
func BenchmarkFixedFieldSetup(benchmark *testing.B) {
	for _, capacity := range []int{4, 64, 256} {
		benchmark.Run(strconv.Itoa(capacity), func(benchmark *testing.B) {
			var warm FieldMap
			warm.initFixed(normalFieldOrder, capacity)
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for range benchmark.N {
				fixedFieldSetupResult.initFixed(normalFieldOrder, capacity)
			}
		})
	}
}

func BenchmarkFixedFieldAccounting(benchmark *testing.B) {
	for _, scenario := range []struct {
		name                string
		capacity, populated int
		removed             int
		grouped             bool
	}{
		{name: "sparse", capacity: 256, populated: 8},
		{name: "dense", capacity: 256, populated: 256},
		{name: "groups", capacity: 256, populated: 8, grouped: true},
		{name: "tombstones", capacity: 256, populated: 256, removed: 248},
	} {
		benchmark.Run(scenario.name, func(benchmark *testing.B) {
			fields := newFixedFieldAccountingFixture(benchmark, scenario.capacity, scenario.populated, scenario.removed, scenario.grouped)
			expected := fields.length() + fields.total()
			if expected == 0 {
				benchmark.Fatal("empty accounting fixture")
			}
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for range benchmark.N {
				fixedFieldAccountingResult = fields.length() + fields.total()
			}
			benchmark.StopTimer()
			if fixedFieldAccountingResult != expected {
				benchmark.Fatalf("accounting changed: got %d, want %d", fixedFieldAccountingResult, expected)
			}
		})
	}
}

func BenchmarkFixedFieldLookup(benchmark *testing.B) {
	for _, populated := range []int{8, 256} {
		benchmark.Run(strconv.Itoa(populated), func(benchmark *testing.B) {
			fields := newFixedFieldAccountingFixture(benchmark, 256, populated, 0, false)
			tag := Tag(1000 + populated - 1)
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for range benchmark.N {
				values, found := fields.lookup(tag)
				if !found {
					benchmark.Fatal("populated tag disappeared")
				}
				fixedFieldAccountingResult = len(values)
			}
		})
	}
}

// Exercise removal/reinsertion and reset with stable field storage. The tags
// change without allocating, covering table tombstones and lifecycle cleanup.
func BenchmarkFixedFieldChurn(benchmark *testing.B) {
	var fields FieldMap
	fields.initFixed(normalFieldOrder, 64)
	var values [8]TagValue
	for index := range values {
		values[index].init(Tag(1000+index), []byte("value"))
	}
	run := func(generation int) {
		for index := range values {
			tag := Tag(1000 + (generation&1023)*8 + index)
			values[index].tag = tag
			if !fields.put(tag, values[index:index+1]) {
				benchmark.Fatal("churn exhausted field storage")
			}
		}
		for index := range values {
			fields.Remove(values[index].tag)
		}
		if generation&15 == 15 {
			fields.Clear()
		}
	}
	for generation := range 1024 {
		run(generation)
	}
	benchmark.ReportAllocs()
	benchmark.ResetTimer()
	for generation := range benchmark.N {
		run(generation)
	}
}

// Include the same owned String call seen in outbound service callbacks, then
// wait for the existing writer's completed write, slot return and peer parsing.
// This is a bounded component fixture, not a TCP or application latency result.
func BenchmarkFixedFieldCompletedString(benchmark *testing.B) {
	fixture := newScopedOutboundBenchmarkWithDelivery(benchmark, false, "", true)
	defer fixture.close()
	fixture.message.rawMessage = nil
	application := &scopedOutboundHookApplication{scopedOutboundBenchmarkApplication: fixture.application}
	application.toApp = func(message *Message) error {
		messageStringBenchmarkResult = message.String()
		return nil
	}
	fixture.session.application = BorrowApplication(application)
	for range 66 {
		if err := fixture.process(); err != nil {
			benchmark.Fatal(err)
		}
	}
	benchmark.ReportAllocs()
	benchmark.ResetTimer()
	for range benchmark.N {
		if err := fixture.process(); err != nil {
			benchmark.Fatal(err)
		}
	}
	benchmark.StopTimer()
	if fixture.application.calls != benchmark.N+66 {
		benchmark.Fatalf("callbacks=%d, want %d", fixture.application.calls, benchmark.N+66)
	}
}
