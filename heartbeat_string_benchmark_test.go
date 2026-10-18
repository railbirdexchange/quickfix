package quickfix

import "testing"

var heartbeatStringResult string

func BenchmarkHeartbeatStringRead(benchmark *testing.B) {
	for _, storage := range []string{"legacy", "fixed"} {
		for _, value := range []struct{ name, text string }{
			{name: "message_type", text: "0"},
			{name: "sender", text: "RAILBIRD"},
			{name: "target", text: "PREPRODFCM01"},
		} {
			benchmark.Run(storage+"/"+value.name, func(benchmark *testing.B) {
				var fields FieldMap
				if storage == "fixed" {
					fields.initFixed(normalFieldOrder, 8)
				} else {
					fields.init()
				}
				fields.SetString(tagMsgType, value.text)
				if got, reject := fields.GetString(tagMsgType); reject != nil || got != value.text {
					benchmark.Fatalf("string read = %q, %v", got, reject)
				}
				benchmark.ReportAllocs()
				benchmark.ResetTimer()
				for range benchmark.N {
					var reject MessageRejectError
					heartbeatStringResult, reject = fields.GetString(tagMsgType)
					if reject != nil {
						benchmark.Fatal(reject)
					}
				}
			})
		}
	}
}

// This completed-write component keeps observation enabled and performs the
// same public string read used repeatedly by FIX-server's admin callbacks.
// The observer consumes every event; it does not implement a StatsD transport.
// Store, logger and peer are the existing bounded component fixtures, not the
// deployed service. Each op includes output-slot return and peer validation.
func BenchmarkHeartbeatObservedStringReads(benchmark *testing.B) {
	for _, reads := range []struct {
		name  string
		count int
	}{{name: "observer_only"}, {name: "three_callback_reads", count: 3}} {
		benchmark.Run(reads.name, func(benchmark *testing.B) {
			fixture := newHeartbeatStoreFixture(benchmark, "borrowed_external")
			defer fixture.close()
			var events, invalidEvents int
			SetSendStageObserver(func(event SendStageEvent) {
				events++
				if event.Stage == "" || event.Duration < 0 || !event.Success || (event.MsgType != "" && event.MsgType != "0") {
					invalidEvents++
				}
			})
			defer SetSendStageObserver(nil)
			fixture.controlApplication.hook = func(message *Message) {
				for range reads.count {
					var reject MessageRejectError
					heartbeatStringResult, reject = message.Header.GetString(tagMsgType)
					if reject != nil || heartbeatStringResult != "0" {
						benchmark.Fatalf("callback type = %q, %v", heartbeatStringResult, reject)
					}
				}
			}
			for range 66 {
				if err := fixture.process("idle_heartbeat"); err != nil {
					benchmark.Fatal(err)
				}
			}
			warmupEvents := events
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for range benchmark.N {
				if err := fixture.process("idle_heartbeat"); err != nil {
					benchmark.Fatal(err)
				}
			}
			benchmark.StopTimer()
			if invalidEvents != 0 || warmupEvents == 0 || events*66 != warmupEvents*(benchmark.N+66) {
				benchmark.Fatalf("observer events=%d warmup=%d invalid=%d", events, warmupEvents, invalidEvents)
			}
		})
	}
}
