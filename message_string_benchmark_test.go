package quickfix

import (
	"bytes"
	"testing"
)

var messageStringBenchmarkResult string

// String is an owned compatibility boundary. These synchronous measurements
// include cooking and the returned string, with construction and warm-up outside
// the timer. Arena rows separate the full-wire cost from legacy field setters.
func BenchmarkMessageString(b *testing.B) {
	for _, kind := range []string{"new_order_single", "session_reject"} {
		for _, storage := range []string{"ordinary", "arena", "raw"} {
			b.Run(storage+"/"+kind, func(b *testing.B) {
				message := newMessageStringFixture(b, kind)
				if storage == "arena" {
					if err := message.attachMessageArena(4096, 64); err != nil {
						b.Fatal(err)
					}
				}
				if storage == "raw" {
					wire := message.build()
					message = NewMessage()
					if err := ParseMessage(message, bytes.NewBuffer(wire)); err != nil {
						b.Fatal(err)
					}
				}
				messageStringBenchmarkResult = message.String()
				b.ReportAllocs()
				b.SetBytes(int64(len(messageStringBenchmarkResult)))
				b.ResetTimer()
				for iteration := 0; iteration < b.N; iteration++ {
					messageStringBenchmarkResult = message.String()
				}
			})
		}
	}
}
