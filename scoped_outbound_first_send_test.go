package quickfix

import (
	"bytes"
	"testing"
)

// BenchmarkScopedOutboundFirstSend includes ordinary convenience construction or
// reparse on every operation, followed by the original callback, persistence,
// completed writer copy, output-slot return, peer parse and acknowledgement.
// Session/dictionary/connection setup and ring warmup are outside measurement.
func BenchmarkScopedOutboundFirstSend(benchmark *testing.B) {
	for _, fresh := range []bool{true, false} {
		name := "reparsed_message"
		if fresh {
			name = "fresh_message"
		}
		benchmark.Run(name, func(benchmark *testing.B) {
			wire, dictionary := boundedOwnershipFixture(benchmark, false)
			raw := bytes.NewBuffer(wire)
			fixture := newScopedOutboundBenchmark(benchmark, false, "")
			defer fixture.close()
			fixture.session.setState(fixture.session, inSession{})
			process := func() {
				if fresh {
					fixture.message = NewMessage()
				}
				if err := ParseMessageWithDataDictionary(fixture.message, raw, dictionary, dictionary); err != nil {
					benchmark.Fatal(err)
				}
				fixture.application.message = fixture.message
				if err := fixture.process(); err != nil {
					benchmark.Fatal(err)
				}
			}
			for iteration := 0; iteration < 66; iteration++ {
				process()
			}
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for iteration := 0; iteration < benchmark.N; iteration++ {
				process()
			}
			benchmark.StopTimer()
			if fixture.application.calls != benchmark.N+66 {
				benchmark.Fatalf("callback count: got %d, want %d", fixture.application.calls, benchmark.N+66)
			}
		})
	}
}
