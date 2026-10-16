package quickfix_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"
)

// This preserves the former reader as an independent oracle, including receiver
// state after errors and the noncanonical inputs accepted by time.Parse.
func legacyTimestampRead(value []byte, initial quickfix.FIXUTCTimestamp) (quickfix.FIXUTCTimestamp, error) {
	var layout string
	switch len(value) {
	case 17:
		layout, initial.Precision = "20060102-15:04:05", quickfix.Seconds
	case 21:
		layout, initial.Precision = "20060102-15:04:05.000", quickfix.Millis
	case 24:
		layout, initial.Precision = "20060102-15:04:05.000000", quickfix.Micros
	case 27:
		layout, initial.Precision = "20060102-15:04:05.000000000", quickfix.Nanos
	default:
		return initial, fmt.Errorf("Invalid Value for Timestamp: %s", value)
	}
	var err error
	initial.Time, err = time.Parse(layout, string(value))
	return initial, err
}

func assertTimestampReadParity(t testing.TB, value []byte) {
	t.Helper()
	initial := quickfix.FIXUTCTimestamp{
		Time:      time.Date(1985, time.October, 26, 1, 21, 0, 123, time.FixedZone("previous", 3600)),
		Precision: quickfix.TimestampPrecision(99),
	}
	want, wantErr := legacyTimestampRead(value, initial)
	got := initial
	gotErr := got.Read(value)
	if (gotErr == nil) != (wantErr == nil) || gotErr != nil && gotErr.Error() != wantErr.Error() {
		t.Fatalf("Read(%q) error = %v, want %v", value, gotErr, wantErr)
	}
	if got.Time != want.Time || got.Precision != want.Precision {
		t.Fatalf("Read(%q) = %#v, want %#v", value, got, want)
	}
}

func TestFIXUTCTimestampReadLegacyParity(t *testing.T) {
	for _, value := range []string{
		"", "20261015", "20261015-01:02:03.1", "20261015-01:02:03.1234",
		"00000101-00:00:00", "00000229-23:59:59.999",
		"19000229-00:00:00", "20000229-23:59:59.123456", "21000229-00:00:00",
		"99991231-23:59:59.999999999", "20261015-01:02:03,123",
		"20261015-01:02:03,123456", "20261015-01:02:03,123456789",
		"20261015-24:00:00", "20261015-23:60:00", "20261015-23:59:60",
		"20260015-01:02:03", "20261315-01:02:03", "20260431-01:02:03",
		"20261000-01:02:03", "20261032-01:02:03", "20261015- 1:02:03",
	} {
		assertTimestampReadParity(t, []byte(value))
	}
	// Exercise every byte position, including separators and non-ASCII input,
	// against the legacy acceptance/error behavior at all supported precisions.
	for _, original := range []string{
		"20261015-23:59:58", "20261015-23:59:58.123",
		"20261015-23:59:58.123456", "20261015-23:59:58.123456789",
	} {
		value := []byte(original)
		for index := range value {
			for replacement := 0; replacement <= 255; replacement++ {
				value[index] = byte(replacement)
				assertTimestampReadParity(t, value)
			}
			value[index] = original[index]
		}
	}
	for _, year := range []int{0, 1, 4, 100, 400, 1900, 1999, 2000, 2004, 2100, 2400, 9999} {
		for month := 1; month <= 12; month++ {
			for day := 28; day <= 32; day++ {
				assertTimestampReadParity(t, []byte(fmt.Sprintf("%04d%02d%02d-12:34:56.123456789", year, month, day)))
			}
		}
	}
}

func FuzzFIXUTCTimestampReadLegacyParity(f *testing.F) {
	for _, value := range []string{
		"20261015-01:02:03", "20261015-01:02:03.123", "20261015-01:02:03.123456",
		"20261015-01:02:03.123456789", "00000229-23:59:59.999", "20261015-01:02:03,123",
		"19000229-01:02:03", "20261015-23:59:60", "", "bad timestamp",
	} {
		f.Add([]byte(value))
	}
	f.Fuzz(func(t *testing.T, value []byte) {
		assertTimestampReadParity(t, value)
	})
}

func BenchmarkFIXUTCTimestampRead(benchmark *testing.B) {
	for _, fixture := range []struct{ name, wire string }{
		{"seconds", "20261015-01:02:03"},
		{"millis", "20261015-01:02:03.123"},
		{"micros", "20261015-01:02:03.123456"},
		{"nanos", "20261015-01:02:03.123456789"},
	} {
		benchmark.Run(fixture.name, func(benchmark *testing.B) {
			value := []byte(fixture.wire)
			var timestamp quickfix.FIXUTCTimestamp
			if err := timestamp.Read(value); err != nil {
				benchmark.Fatal(err)
			}
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for iteration := 0; iteration < benchmark.N; iteration++ {
				if err := timestamp.Read(value); err != nil {
					benchmark.Fatal(err)
				}
			}
		})
	}
}
