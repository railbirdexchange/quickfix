package quickfix

import "testing"

// Global sinks force the public getter's independently retainable string to
// escape. Setup, dictionary construction, parse and warm-up stay untimed.
var sessionHeaderStringResult string
var sessionHeaderCallbackResults [3]string

func BenchmarkSessionHeaderStringRead(benchmark *testing.B) {
	for _, scenario := range []struct {
		name       string
		tag        Tag
		inbound    bool
		mismatched bool
		standalone bool
	}{
		{name: "inbound_sender", tag: tagSenderCompID, inbound: true},
		{name: "inbound_target", tag: tagTargetCompID, inbound: true},
		{name: "outbound_sender", tag: tagSenderCompID},
		{name: "outbound_target", tag: tagTargetCompID},
		{name: "inbound_sender_mismatch", tag: tagSenderCompID, inbound: true, mismatched: true},
		{name: "inbound_per_message_subid", tag: tagSenderSubID, inbound: true, mismatched: true},
		{name: "standalone_sender", tag: tagSenderCompID, standalone: true},
	} {
		benchmark.Run(scenario.name, func(benchmark *testing.B) {
			id := sessionHeaderStringID("benchmark")
			current := newSessionHeaderStringSession(benchmark, id)
			slot := current.outbound.replay
			if scenario.inbound {
				slot = current.boundedIn.parsed
			}
			wire := sessionHeaderStringWire(benchmark, id, scenario.inbound)
			if scenario.standalone {
				var err error
				slot, err = NewMessageSlot(current.transportDataDictionary, current.appDataDictionary, DefaultParseLimits())
				if err != nil {
					benchmark.Fatal(err)
				}
			}
			if err := slot.Parse(wire); err != nil {
				benchmark.Fatal(err)
			}
			fields := &slot.Message().Header.FieldMap
			if scenario.mismatched {
				fields.SetString(scenario.tag, "per-message-value")
			}
			want, reject := fields.GetString(scenario.tag)
			if reject != nil || want == "" {
				benchmark.Fatalf("invalid fixture: %q, %v", want, reject)
			}
			for range 66 {
				sessionHeaderStringResult, reject = fields.GetString(scenario.tag)
				if reject != nil || sessionHeaderStringResult != want {
					benchmark.Fatalf("warm getter: %q, %v", sessionHeaderStringResult, reject)
				}
			}
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for range benchmark.N {
				sessionHeaderStringResult, reject = fields.GetString(scenario.tag)
				if reject != nil || sessionHeaderStringResult != want {
					benchmark.Fatalf("getter: %q, %v", sessionHeaderStringResult, reject)
				}
			}
			benchmark.StopTimer()
		})
	}
}

// This is a completed-write component with actual public identity reads inside
// every callback. It includes control construction, bounded persistence, encode,
// completed writer copy, returned slot, peer checks and journal retirement. It
// is not a TCP, inbound round-trip or whole FIX-server latency measurement.
func BenchmarkSessionHeaderStringHeartbeat(benchmark *testing.B) {
	fixture := newPreparedRejectHeartbeatFixture(benchmark)
	defer fixture.close()
	want := [3]string{fixture.session.sessionID.BeginString, fixture.session.sessionID.SenderCompID, fixture.session.sessionID.TargetCompID}
	fixture.controlApplication.hook = func(message *Message) {
		for index, tag := range [...]Tag{tagBeginString, tagSenderCompID, tagTargetCompID} {
			var reject MessageRejectError
			sessionHeaderCallbackResults[index], reject = message.Header.GetString(tag)
			if reject != nil || sessionHeaderCallbackResults[index] != want[index] {
				benchmark.Fatalf("callback GetString(%d) = %q, %v", tag, sessionHeaderCallbackResults[index], reject)
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
	if fixture.controlApplication.calls != benchmark.N+66 {
		benchmark.Fatalf("callbacks=%d, want %d", fixture.controlApplication.calls, benchmark.N+66)
	}
}
