package quickfix

import (
	"bytes"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/datadictionary"
)

// These additive API measurements supplement the unchanged controlled baseline.
func BenchmarkBoundedEncoding(b *testing.B) {
	b.Run("new_order_single", func(b *testing.B) {
		message, wire, _ := allocationBaselineFIX40Order(b)
		benchmarkBoundedMessageEncoding(b, message, nil, len(wire), false)
	})
	b.Run("repeating_group", func(b *testing.B) {
		dictionary, err := datadictionary.Parse("spec/FIX44.xml")
		if err != nil {
			b.Fatal(err)
		}
		message := NewMessage()
		if err := ParseMessageWithDataDictionary(message, bytes.NewBufferString(allocationBaselineRepeatingGroupMessage), dictionary, dictionary); err != nil {
			b.Fatal(err)
		}
		benchmarkBoundedMessageEncoding(b, message, nil, len(message.build()), false)
	})
	b.Run("resend_preserved_body", func(b *testing.B) {
		message := NewMessage()
		if err := ParseMessage(message, bytes.NewBufferString(allocationBaselineRepeatingGroupMessage)); err != nil {
			b.Fatal(err)
		}
		message.Header.SetBool(tagPossDupFlag, true)
		message.Header.SetString(tagOrigSendingTime, "20231231-20:19:41")
		message.Header.SetString(tagSendingTime, "20231231-20:20:41")
		benchmarkBoundedMessageEncoding(b, message, message.bodyBytes, len(message.buildWithBodyBytes(message.bodyBytes)), true)
	})
	b.Run("integer", func(b *testing.B) {
		field := FIXInt(123456789)
		dst := make([]byte, 0, 20)
		if _, err := field.AppendTo(dst); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for iteration := 0; iteration < b.N; iteration++ {
			var err error
			allocationBaselineBytes, err = field.AppendTo(dst[:0])
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("timestamp_nanos", func(b *testing.B) {
		field := FIXUTCTimestamp{Time: time.Date(2026, 10, 15, 10, 11, 12, 123456789, time.UTC), Precision: Nanos}
		dst := make([]byte, 0, 27)
		if _, err := field.AppendTo(dst); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for iteration := 0; iteration < b.N; iteration++ {
			var err error
			allocationBaselineBytes, err = field.AppendTo(dst[:0])
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func benchmarkBoundedMessageEncoding(b *testing.B, message *Message, body []byte, capacity int, resend bool) {
	b.Helper()
	dst := make([]byte, 0, capacity)
	encode := message.AppendTo
	if resend {
		encode = func(dst []byte) ([]byte, error) { return message.AppendWithBodyBytesTo(dst, body) }
	}
	if _, err := encode(dst); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(capacity))
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		var err error
		allocationBaselineBytes, err = encode(dst[:0])
		if err != nil {
			b.Fatal(err)
		}
	}
}
