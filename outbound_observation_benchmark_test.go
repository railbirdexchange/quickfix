package quickfix

import (
	"sync/atomic"
	"testing"
)

// BenchmarkOutboundObservation retains the existing completed-write component:
// SendToTarget, bounded persistence, encoding, independent writer copy, slot
// return, peer parse and journal retirement. The FIX44 grouped source fixture
// supplies an AJ-bearing dictionary; changing its MsgType exercises observation,
// not application-specific validation of a complete QuoteResponse.
//
// Observer counters are atomic because writer-stage callbacks may run on the
// writer goroutine. Every callback consumes owned value metadata without keeping
// mutable frame references or allocating. The warm-up determines events/op so
// the same benchmark can measure added lifecycle stages without changing its
// source. All observed callbacks must finish within each completed operation.
func BenchmarkOutboundObservation(benchmark *testing.B) {
	for _, messageType := range []string{"8", "AJ", "CUSTOM.APPLICATION.MESSAGE.TYPE"} {
		for _, enabled := range []bool{false, true} {
			mode := "disabled"
			if enabled {
				mode = "enabled"
			}
			benchmark.Run(messageType+"/"+mode, func(benchmark *testing.B) {
				SetSendStageObserver(nil)
				defer SetSendStageObserver(nil)
				fixture := newScopedOutboundBenchmarkWithDelivery(benchmark, true, messageType, true)
				defer fixture.close()
				var events, checksum atomic.Uint64
				var invalid atomic.Bool
				if enabled {
					SetSendStageObserver(func(event SendStageEvent) {
						if event.MsgType != messageType || event.SessionID != fixture.session.sessionID ||
							event.Stage == "" || event.Duration < 0 || !event.Success {
							invalid.Store(true)
						}
						value := uint64(event.Duration)
						for index := range event.MsgType {
							value += uint64(event.MsgType[index])
						}
						for index := range event.Stage {
							value += uint64(event.Stage[index])
						}
						checksum.Add(value)
						events.Add(1)
					})
				}
				const warmups = 66 // Visit all 33 output slots twice.
				for iteration := 0; iteration < warmups; iteration++ {
					if err := fixture.process(); err != nil {
						benchmark.Fatal(err)
					}
				}
				warmEvents := events.Load()
				eventsPerOperation := warmEvents / warmups
				if invalid.Load() || warmEvents%warmups != 0 ||
					enabled && eventsPerOperation < 4 ||
					!enabled && (warmEvents != 0 || checksum.Load() != 0) {
					benchmark.Fatalf("invalid warm-up observation: events=%d checksum=%d invalid=%v",
						warmEvents, checksum.Load(), invalid.Load())
				}
				benchmark.ReportAllocs()
				benchmark.ResetTimer()
				for iteration := 0; iteration < benchmark.N; iteration++ {
					if err := fixture.process(); err != nil {
						benchmark.Fatal(err)
					}
				}
				benchmark.StopTimer()
				expectedEvents := uint64(benchmark.N+warmups) * eventsPerOperation
				if invalid.Load() || events.Load() != expectedEvents ||
					enabled && checksum.Load() == 0 ||
					!enabled && checksum.Load() != 0 {
					benchmark.Fatalf("invalid completed observation: events=%d want=%d checksum=%d invalid=%v",
						events.Load(), expectedEvents, checksum.Load(), invalid.Load())
				}
				if fixture.application.calls != benchmark.N+warmups {
					benchmark.Fatalf("callbacks=%d want=%d", fixture.application.calls, benchmark.N+warmups)
				}
				benchmark.ReportMetric(float64(eventsPerOperation), "events/op")
			})
		}
	}
}
