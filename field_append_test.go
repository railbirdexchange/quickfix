package quickfix

import (
	"bytes"
	"strconv"
	"testing"
	"time"
)

func TestFIXIntAppendTo(t *testing.T) {
	maximum := int(^uint(0) >> 1)
	minimum := -maximum - 1
	for _, value := range []int{0, 1, -1, 9, 10, 99, 100, -10, minimum, maximum} {
		expected := strconv.Itoa(value)
		t.Run(expected, func(t *testing.T) {
			field := FIXInt(value)
			assertBoundedFieldAppend(t, field.AppendTo, expected)
			if got := string(field.Write()); got != expected {
				t.Fatalf("legacy Write() = %q, want %q", got, expected)
			}
		})
	}
}

func TestFIXUTCTimestampAppendTo(t *testing.T) {
	localTime := time.Date(2016, time.February, 8, 17, 7, 16, 954123987, time.FixedZone("UTC-5", -5*60*60))
	cases := []struct {
		name      string
		value     time.Time
		precision TimestampPrecision
		expected  string
	}{
		{"seconds", localTime, Seconds, "20160208-22:07:16"},
		{"millis truncate", localTime, Millis, "20160208-22:07:16.954"},
		{"micros truncate", localTime, Micros, "20160208-22:07:16.954123"},
		{"nanos", localTime, Nanos, "20160208-22:07:16.954123987"},
		{"unknown precision defaults to millis", localTime, TimestampPrecision(99), "20160208-22:07:16.954"},
		{"UTC date rollover", time.Date(2016, time.February, 8, 1, 2, 3, 0, time.FixedZone("UTC+3", 3*60*60)), Seconds, "20160207-22:02:03"},
		{"zero time", time.Time{}, Millis, "00010101-00:00:00.000"},
		{"year zero", time.Date(0, time.January, 2, 3, 4, 5, 0, time.UTC), Seconds, "00000102-03:04:05"},
		{"negative year", time.Date(-1, time.January, 2, 3, 4, 5, 0, time.UTC), Millis, "-00010102-03:04:05.000"},
		{"five digit year", time.Date(10000, time.January, 2, 3, 4, 5, 123456789, time.UTC), Nanos, "100000102-03:04:05.123456789"},
		{"negative five digit year", time.Date(-10000, time.January, 2, 3, 4, 5, 123456789, time.UTC), Micros, "-100000102-03:04:05.123456"},
	}
	for _, fixture := range cases {
		t.Run(fixture.name, func(t *testing.T) {
			field := FIXUTCTimestamp{Time: fixture.value, Precision: fixture.precision}
			assertBoundedFieldAppend(t, field.AppendTo, fixture.expected)
			if got := string(field.Write()); got != fixture.expected {
				t.Fatalf("legacy Write() = %q, want %q", got, fixture.expected)
			}
		})
	}
}

func assertBoundedFieldAppend(t *testing.T, appendField func([]byte) ([]byte, error), expected string) {
	t.Helper()
	const prefix = "prefix:"
	storage := make([]byte, len(prefix)+len(expected))
	copy(storage, prefix)
	result, err := appendField(storage[:len(prefix)])
	if err != nil {
		t.Fatalf("AppendTo with exact capacity: %v", err)
	}
	if string(result) != prefix+expected {
		t.Fatalf("AppendTo = %q, want %q", result, prefix+expected)
	}
	if &result[0] != &storage[0] || cap(result) != cap(storage) {
		t.Fatal("AppendTo did not retain caller-owned storage")
	}
	for _, capacity := range []int{0, len(expected) - 1} {
		storage := bytes.Repeat([]byte{0xa5}, len(prefix)+capacity)
		copy(storage, prefix)
		before := bytes.Clone(storage)
		result, err := appendField(storage[:len(prefix)])
		if err != ErrBufferTooSmall {
			t.Fatalf("AppendTo with %d bytes available: error = %v, want ErrBufferTooSmall", capacity, err)
		}
		if len(result) != len(prefix) || cap(result) != len(storage) || &result[0] != &storage[0] {
			t.Fatal("failed AppendTo did not return the original destination")
		}
		if !bytes.Equal(storage, before) {
			t.Fatal("failed AppendTo changed destination backing storage")
		}
	}
	result, err = appendField(nil)
	if err != ErrBufferTooSmall || result != nil {
		t.Fatalf("AppendTo(nil) = %v, %v; want nil, ErrBufferTooSmall", result, err)
	}
}
