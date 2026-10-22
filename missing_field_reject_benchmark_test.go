package quickfix

import "testing"

var missingFieldRejectResult MessageRejectError
var missingFieldRejectText string

// Retain the returned error so the benchmark includes its public owned lifetime.
// Report ignored-text lookups separately from consumers that render the error.
func BenchmarkMissingFieldReject(benchmark *testing.B) {
	for _, fixed := range []bool{false, true} {
		storage := "legacy"
		if fixed {
			storage = "fixed"
		}
		benchmark.Run(storage+"/GetString", func(benchmark *testing.B) {
			var fields FieldMap
			if fixed {
				fields.initFixed(normalFieldOrder, 8)
			} else {
				fields.init()
			}
			_, reject := fields.GetString(41)
			if reject == nil || reject.Error() != "Conditionally Required Field Missing (41)" {
				benchmark.Fatalf("unexpected reject: %v", reject)
			}
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for range benchmark.N {
				_, missingFieldRejectResult = fields.GetString(41)
			}
		})
	}
	for _, tag := range []struct {
		name  string
		value Tag
	}{{"tag41", 41}, {"tag526", 526}} {
		for _, reads := range []struct {
			name  string
			count int
		}{{"unrendered", 0}, {"render_once", 1}, {"render_twice", 2}} {
			benchmark.Run(tag.name+"/"+reads.name, func(benchmark *testing.B) {
				missingFieldRejectResult = ConditionallyRequiredFieldMissing(tag.value)
				missingFieldRejectText = missingFieldRejectResult.Error()
				benchmark.ReportAllocs()
				benchmark.ResetTimer()
				for range benchmark.N {
					missingFieldRejectResult = ConditionallyRequiredFieldMissing(tag.value)
					for range reads.count {
						missingFieldRejectText = missingFieldRejectResult.Error()
					}
				}
			})
		}
	}
}

// Include three absent optional-field reads in an otherwise unchanged completed
// heartbeat. The fixture waits for write completion, slot return and peer parse.
func BenchmarkHeartbeatMissingFieldReads(benchmark *testing.B) {
	fixture := newHeartbeatStoreFixture(benchmark, "borrowed_external")
	defer fixture.close()
	fixture.controlApplication.hook = func(message *Message) {
		for _, tag := range [...]Tag{11, 41, 526} {
			_, missingFieldRejectResult = message.Body.GetString(tag)
			if missingFieldRejectResult == nil {
				benchmark.Fatalf("unexpected field %d in heartbeat", tag)
			}
		}
	}
	for range 66 {
		if err := fixture.process("idle_heartbeat"); err != nil {
			benchmark.Fatal(err)
		}
	}
	benchmark.ReportAllocs()
	benchmark.ResetTimer()
	for range benchmark.N {
		if err := fixture.process("idle_heartbeat"); err != nil {
			benchmark.Fatal(err)
		}
	}
	benchmark.StopTimer()
}
