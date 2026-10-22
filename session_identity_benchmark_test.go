package quickfix

import (
	"strings"
	"testing"
)

var sessionIdentityText string

// Registration and warm-up are outside the timed loop. The same source runs
// against the baseline implementation and the registered-session optimization.
func BenchmarkSessionIdentityString(benchmark *testing.B) {
	for _, fixture := range []struct {
		name string
		id   SessionID
	}{
		{"short", SessionID{BeginString: BeginStringFIXT11, SenderCompID: "RAILBIRD", TargetCompID: "PREPRODFCM20"}},
		{"all_fields", SessionID{BeginString: BeginStringFIXT11, SenderCompID: "RAILBIRD", SenderSubID: "desk", SenderLocationID: "sender-location", TargetCompID: "PREPRODFCM20", TargetSubID: "customer", TargetLocationID: "target-location", Qualifier: "trading"}},
		{"long", SessionID{BeginString: BeginStringFIXT11, SenderCompID: strings.Repeat("S", 128), TargetCompID: strings.Repeat("T", 128)}},
	} {
		for _, registered := range []bool{false, true} {
			state := "unregistered"
			if registered {
				state = "registered"
			}
			benchmark.Run(fixture.name+"/"+state, func(benchmark *testing.B) {
				if registered {
					if err := registerSession(&session{sessionID: fixture.id}); err != nil {
						benchmark.Fatal(err)
					}
					defer UnregisterSession(fixture.id)
				}
				sessionIdentityText = fixture.id.String()
				benchmark.ReportAllocs()
				benchmark.ResetTimer()
				for range benchmark.N {
					sessionIdentityText = fixture.id.String()
				}
				benchmark.StopTimer()
			})
		}
	}
}

// Includes control construction, callback, persistence, encoding, completed
// writer copy, returned output slot and peer validation. Formatting counts are
// explicit component scenarios, not a claim about the production callback mix.
func BenchmarkHeartbeatSessionIdentity(benchmark *testing.B) {
	for _, scenario := range []struct {
		name  string
		reads int
	}{{"without_formatting", 0}, {"format_once", 1}, {"format_four_times", 4}} {
		benchmark.Run(scenario.name, func(benchmark *testing.B) {
			fixture := newPreparedRejectHeartbeatFixture(benchmark)
			defer fixture.close()
			fixture.controlApplication.hook = func(*Message) {
				for range scenario.reads {
					sessionIdentityText = fixture.session.sessionID.String()
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
