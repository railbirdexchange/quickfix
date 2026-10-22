package quickfix

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Keep the oracle independent of FIXUTCTimestamp.Read and the byte validator.
// These are exactly the four layouts selected by the legacy length gate.
func timestampValidationOracle(value []byte) bool {
	var layout string
	switch len(value) {
	case 17:
		layout = "20060102-15:04:05"
	case 21:
		layout = "20060102-15:04:05.000"
	case 24:
		layout = "20060102-15:04:05.000000"
	case 27:
		layout = "20060102-15:04:05.000000000"
	default:
		return false
	}
	_, err := time.Parse(layout, string(value))
	return err == nil
}

func assertTimestampValidationParity(t testing.TB, value []byte) {
	t.Helper()
	want := timestampValidationOracle(value)
	for _, kind := range []string{"UTCTIMESTAMP", "TIME"} {
		if got := validFieldValue(kind, value); got != want {
			t.Fatalf("%s %q: accepted=%t, time.Parse=%t", kind, value, got, want)
		}
	}
}

func TestTimestampValidationSignedFractions(t *testing.T) {
	const base = "20261015-01:02:03"
	for _, width := range []int{3, 6, 9} {
		for _, separator := range []string{".", ","} {
			for _, fraction := range []struct {
				value string
				valid bool
			}{
				{strings.Repeat("0", width), true},
				{strings.Repeat("9", width), true},
				{"+" + strings.Repeat("0", width-1), true},
				{"+" + strings.Repeat("9", width-1), true},
				{"-" + strings.Repeat("0", width-1), true},
				{"-" + strings.Repeat("0", width-2) + "1", false},
				{"-" + strings.Repeat("9", width-1), false},
				{"++" + strings.Repeat("0", width-2), false},
				{"-+" + strings.Repeat("0", width-2), false},
				{strings.Repeat("0", width-1) + "+", false},
				{" " + strings.Repeat("0", width-1), false},
			} {
				value := []byte(base + separator + fraction.value)
				if got := timestampValidationOracle(value); got != fraction.valid {
					t.Fatalf("oracle acceptance for %q = %t, want %t", value, got, fraction.valid)
				}
				original := bytes.Clone(value)
				assertTimestampValidationParity(t, value)
				if !bytes.Equal(value, original) {
					t.Fatalf("validation mutated caller-owned bytes: %q -> %q", original, value)
				}
			}
		}
	}
}

func TestTimestampValidationCalendarAndLengths(t *testing.T) {
	for _, year := range []int{0, 1, 4, 100, 400, 1900, 1999, 2000, 2004, 2100, 2400, 9999} {
		for month := 0; month <= 13; month++ {
			for day := 0; day <= 32; day++ {
				date := fmt.Sprintf("%04d%02d%02d-23:59:59", year, month, day)
				for _, suffix := range []string{"", ".123", ",+00000", ".-00000000"} {
					assertTimestampValidationParity(t, []byte(date+suffix))
				}
			}
		}
	}
	for _, value := range []string{
		"", "bad timestamp", "20261015-24:00:00", "20261015-23:60:00",
		"20261015-23:59:60", "20261015- 1:02:03", "20261015-+1:02:03",
		"20261015-01:02:03.12", "20261015-01:02:03.1234",
		"20261015-01:02:03.12345", "20261015-01:02:03.1234567",
		"20261015-01:02:03.12345678", "20261015-01:02:03.1234567890",
		// time.Parse accepts one-digit hours, but each compensation below
		// leaves excess text at the library's four permitted total lengths.
		"20261015-1:02:03.", "20261015-1:02:03.1234",
		"20261015-1:02:03.1234567", "20261015-1:02:03.1234567890",
	} {
		if timestampValidationOracle([]byte(value)) {
			t.Fatalf("negative fixture unexpectedly accepted by time.Parse: %q", value)
		}
		assertTimestampValidationParity(t, []byte(value))
	}
	for length := 0; length <= 64; length++ {
		assertTimestampValidationParity(t, bytes.Repeat([]byte{'0'}, length))
	}
}

func TestTimestampValidationEveryByteMutation(t *testing.T) {
	for _, original := range []string{
		"00000229-23:59:59", "20261015-01:02:03.123", "20261015-01:02:03,+12",
		"20261015-01:02:03.-00", "20261015-01:02:03.123456",
		"20261015-01:02:03,-00000", "99991231-23:59:59.123456789",
		"20261015-01:02:03,+12345678", "20261015-01:02:03.-00000000",
	} {
		value := []byte(original)
		for index := range value {
			for replacement := 0; replacement <= 255; replacement++ {
				value[index] = byte(replacement)
				assertTimestampValidationParity(t, value)
			}
			value[index] = original[index]
		}
	}
}

func FuzzTimestampValidationLegacyParity(f *testing.F) {
	for _, value := range []string{
		"20261015-01:02:03", "20261015-01:02:03.123", "20261015-01:02:03,+12",
		"20261015-01:02:03.-00", "20261015-01:02:03.123456",
		"20261015-01:02:03,-00000", "20261015-01:02:03.123456789",
		"20261015-01:02:03,+12345678", "20261015-01:02:03.-00000000",
		"00000229-00:00:00", "19000229-00:00:00", "20261015-1:02:03.1234", "",
	} {
		f.Add([]byte(value))
	}
	f.Fuzz(func(t *testing.T, value []byte) {
		assertTimestampValidationParity(t, value)
	})
}

func TestTimestampValidationSessionReject(t *testing.T) {
	fixture := newTimestampValidationFixture(t, []byte("19000229-01:02:03.123"), false)
	defer fixture.close()
	retained := fixture.session.Validate(fixture.message)
	for range 66 {
		if err := completeTimestampValidationReject(fixture); err != nil {
			t.Fatal(err)
		}
	}
	parseTimestampValidationFixture(t, fixture, []byte("20000229-01:02:03,-00"))
	if reject := fixture.session.Validate(fixture.message); reject != nil {
		t.Fatalf("valid timestamp after reject: %v", reject)
	}
	if retained.Error() != "Incorrect data format for value" || retained.RejectReason() != 6 {
		t.Fatalf("retained timestamp reject changed: %v", retained)
	}
	if reference, present := retained.RefTagID(); !present || reference != 60 {
		t.Fatalf("retained reference = %d, %t", reference, present)
	}
}
