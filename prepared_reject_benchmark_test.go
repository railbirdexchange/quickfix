package quickfix

import "testing"

// This fixture uses FIX44 so every measured heartbeat field (11, 41, 526) is
// configured. The earlier FIX40 heartbeat fixture deliberately remains intact:
// tag 526 there exercises the allocating unknown-tag fallback.
func newPreparedRejectHeartbeatFixture(test testing.TB) *heartbeatStoreFixture {
	test.Helper()
	base := newScopedOutboundBenchmark(test, true, "")
	application := &scopedControlApplication{}
	base.session.application = BorrowApplication(application)
	return &heartbeatStoreFixture{
		scopedOutboundBenchmark: base,
		controlApplication:      application,
		timerDone:               make(chan struct{}),
	}
}

// The harness uses only the original getter/error APIs except RefTagID, which
// is deliberately not called here so identical source runs before/after the
// approved value-returning API change. Setup and dictionary parsing are untimed.
func BenchmarkPreparedMissingField(benchmark *testing.B) {
	for _, field := range []struct {
		name string
		tag  Tag
	}{{"configured", 526}, {"unknown", 100001}} {
		for _, reads := range []struct {
			name  string
			count int
		}{{"unrendered", 0}, {"render_twice", 2}} {
			benchmark.Run(field.name+"/"+reads.name, func(benchmark *testing.B) {
				wire, dictionary := boundedOwnershipFixture(benchmark, true)
				slot, err := NewMessageSlot(dictionary, dictionary, DefaultParseLimits())
				if err != nil {
					benchmark.Fatal(err)
				}
				if err := slot.Parse(wire); err != nil {
					benchmark.Fatal(err)
				}
				fields := &slot.Message().Body.FieldMap
				_, reject := fields.GetString(field.tag)
				if reject == nil || reject.Error() == "" {
					benchmark.Fatal("expected missing-field reject")
				}
				benchmark.ReportAllocs()
				benchmark.ResetTimer()
				for range benchmark.N {
					_, missingFieldRejectResult = fields.GetString(field.tag)
					for range reads.count {
						missingFieldRejectText = missingFieldRejectResult.Error()
					}
				}
			})
		}
	}
}

func BenchmarkPreparedRejectHeartbeat(benchmark *testing.B) {
	for _, reads := range []struct {
		name  string
		count int
	}{{"unrendered", 0}, {"render_twice", 2}} {
		benchmark.Run(reads.name, func(benchmark *testing.B) {
			fixture := newPreparedRejectHeartbeatFixture(benchmark)
			defer fixture.close()
			fixture.controlApplication.hook = func(message *Message) {
				for _, tag := range [...]Tag{11, 41, 526} {
					_, missingFieldRejectResult = message.Body.GetString(tag)
					if missingFieldRejectResult == nil {
						benchmark.Fatalf("unexpected field %d in heartbeat", tag)
					}
					for range reads.count {
						missingFieldRejectText = missingFieldRejectResult.Error()
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
		})
	}
}
