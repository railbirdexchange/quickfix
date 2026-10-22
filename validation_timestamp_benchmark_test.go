package quickfix

import (
	"bytes"
	"io"
	"testing"
)

var timestampValidationAccepted bool
var timestampValidationReject MessageRejectError
var timestampValidationErrorText string

var timestampValidationBenchmarks = [...]struct {
	name, value string
	valid       bool
}{
	{"seconds", "20261015-01:02:03", true},
	{"nanos", "20261015-01:02:03.123456789", true},
	{"comma", "20261015-01:02:03,123", true},
	{"signed_fraction", "20261015-01:02:03.+12", true},
	{"negative_zero", "20261015-01:02:03,-00", true},
	{"invalid_calendar", "19000229-01:02:03.123", false},
	{"invalid_fraction", "20261015-01:02:03.-01", false},
	{"invalid_length", "20261015-01:02:03.1234", false},
}

func BenchmarkTimestampValidationValue(benchmark *testing.B) {
	for _, scenario := range timestampValidationBenchmarks {
		benchmark.Run(scenario.name, func(benchmark *testing.B) {
			value := []byte(scenario.value)
			if timestampValidationOracle(value) != scenario.valid ||
				validFieldValue("UTCTIMESTAMP", value) != scenario.valid {
				benchmark.Fatal("invalid timestamp benchmark fixture")
			}
			for range 66 {
				timestampValidationAccepted = validFieldValue("UTCTIMESTAMP", value)
			}
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for range benchmark.N {
				timestampValidationAccepted = validFieldValue("UTCTIMESTAMP", value)
			}
			benchmark.StopTimer()
		})
	}
}

func newTimestampValidationFixture(test testing.TB, value []byte, valid bool) *scopedOutboundBenchmark {
	test.Helper()
	fixture := newRequiredValidationFixture(test, false)
	parseTimestampValidationFixture(test, fixture, value)
	reject := fixture.session.Validate(fixture.message)
	if valid {
		if reject != nil {
			fixture.close()
			test.Fatalf("valid timestamp rejected: %v", reject)
		}
		return fixture
	}
	if reject == nil || reject.RejectReason() != 6 || reject.Error() != "Incorrect data format for value" {
		fixture.close()
		test.Fatalf("expected timestamp format reject: %v", reject)
	}
	if reference, present := reject.RefTagID(); !present || reference != 60 {
		fixture.close()
		test.Fatalf("unexpected timestamp reference: %d, %t", reference, present)
	}
	return fixture
}

// Reparse wire bytes so both the field map and the ordered validation walk see
// the timestamp. Mutating a parsed FieldMap alone does not replace its wire walk.
func parseTimestampValidationFixture(test testing.TB, fixture *scopedOutboundBenchmark, value []byte) {
	test.Helper()
	wire, _, _ := configuredValidationInput(test, configuredValidationScenario{
		old: "60=20231231-20:19:41\x01", replacement: "60=" + string(value) + "\x01",
	})
	if err := fixture.session.boundedIn.parsed.Parse(wire); err != nil {
		fixture.close()
		test.Fatal(err)
	}
	fixture.message = fixture.session.boundedIn.parsed.Message()
}

func BenchmarkSessionTimestampValidation(benchmark *testing.B) {
	for _, scenario := range []struct {
		name, value string
		valid       bool
	}{
		{"valid", "20261015-01:02:03.123", true},
		{"comma_signed", "20261015-01:02:03,+12", true},
		{"invalid_calendar", "19000229-01:02:03.123", false},
	} {
		benchmark.Run(scenario.name, func(benchmark *testing.B) {
			fixture := newTimestampValidationFixture(benchmark, []byte(scenario.value), scenario.valid)
			defer fixture.close()
			for range 66 {
				timestampValidationReject = fixture.session.Validate(fixture.message)
			}
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for range benchmark.N {
				timestampValidationReject = fixture.session.Validate(fixture.message)
				if timestampValidationReject != nil {
					timestampValidationErrorText = timestampValidationReject.Error()
					timestampValidationErrorText = timestampValidationReject.Error()
				}
			}
			benchmark.StopTimer()
		})
	}
}

// Reuse the existing bounded writer fixture: this covers validation, control
// construction, borrowed callback, persistence, encoding, completed writer
// copy, returned output slot, independent peer parse and explicit retirement.
// It does not measure a TCP socket or whole-session round trip.
func completeTimestampValidationReject(fixture *scopedOutboundBenchmark) error {
	reject := fixture.session.Validate(fixture.message)
	if reject == nil {
		return io.ErrUnexpectedEOF
	}
	sequence := fixture.store.NextSenderMsgSeqNum()
	referenceSequence, fieldErr := fixture.message.Header.GetInt(tagMsgSeqNum)
	if fieldErr != nil {
		return fieldErr
	}
	if err := fixture.session.doReject(fixture.message, reject); err != nil {
		return err
	}
	if err := completeScopedControl(fixture); err != nil {
		return err
	}
	message := fixture.peer.Message()
	if value, err := message.Header.GetBytes(tagMsgType); err != nil || string(value) != "3" {
		return io.ErrUnexpectedEOF
	}
	actualSequence, fieldErr := message.Header.GetInt(tagMsgSeqNum)
	if fieldErr != nil || actualSequence != sequence || fixture.store.NextSenderMsgSeqNum() != sequence+1 {
		return ErrInvalidJournalSequence
	}
	for _, field := range [...]struct {
		tag  Tag
		want int
	}{
		{tagRefTagID, 60}, {tagSessionRejectReason, 6}, {tagRefSeqNum, referenceSequence},
	} {
		if value, err := message.Body.GetInt(field.tag); err != nil || value != field.want {
			return io.ErrUnexpectedEOF
		}
	}
	if value, err := message.Body.GetBytes(tagRefMsgType); err != nil || string(value) != "D" {
		return io.ErrUnexpectedEOF
	}
	if value, err := message.Body.GetBytes(tagText); err != nil || string(value) != "Incorrect data format for value" {
		return io.ErrUnexpectedEOF
	}
	index := fixture.store.nextEntry(sequence, sequence)
	if index < 0 || !bytes.Equal(fixture.store.entryBytes(index), fixture.connection.wire[:fixture.connection.used]) {
		return ErrInvalidJournalSequence
	}
	fixture.connection.used = 0
	fixture.floor++
	return fixture.store.RetireBefore(fixture.floor)
}

func BenchmarkSessionTimestampReject(benchmark *testing.B) {
	fixture := newTimestampValidationFixture(benchmark, []byte("19000229-01:02:03.123"), false)
	defer fixture.close()
	for range 66 {
		if err := completeTimestampValidationReject(fixture); err != nil {
			benchmark.Fatal(err)
		}
	}
	benchmark.ReportAllocs()
	benchmark.ResetTimer()
	for range benchmark.N {
		if err := completeTimestampValidationReject(fixture); err != nil {
			benchmark.Fatal(err)
		}
	}
	benchmark.StopTimer()
}
