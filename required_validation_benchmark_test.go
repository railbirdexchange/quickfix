package quickfix

import (
	"bytes"
	"io"
	"testing"
)

var requiredValidationResult MessageRejectError
var requiredValidationText string

func newRequiredValidationFixture(test testing.TB, missingSide bool) *scopedOutboundBenchmark {
	test.Helper()
	fixture := newScopedOutboundBenchmarkWithDelivery(test, true, "", true)
	wire, _ := boundedOwnershipFixture(test, true)
	if err := fixture.session.boundedIn.parsed.Parse(wire); err != nil {
		fixture.close()
		test.Fatal(err)
	}
	fixture.message = fixture.session.boundedIn.parsed.Message()
	if reject := fixture.session.Validate(fixture.message); reject != nil {
		fixture.close()
		test.Fatalf("invalid required-validation fixture: %v", reject)
	}
	if missingSide {
		fixture.message.Body.Remove(54)
		reject := fixture.session.Validate(fixture.message)
		if reject == nil || reject.RejectReason() != 1 || reject.Error() != "Required tag missing" {
			fixture.close()
			test.Fatalf("expected missing-Side validation reject: %v", reject)
		}
		if tag, present := reject.RefTagID(); !present || tag != 54 {
			fixture.close()
			test.Fatalf("unexpected reference: %d, %v", tag, present)
		}
	}
	return fixture
}

// This includes validation, reject construction, callback, persistence, encoding,
// completed writer copy, slot return, independent peer parse and journal retire.
func completeRequiredValidationReject(fixture *scopedOutboundBenchmark) error {
	reject := fixture.session.Validate(fixture.message)
	if reject == nil {
		return io.ErrUnexpectedEOF
	}
	sequence := fixture.store.NextSenderMsgSeqNum()
	if err := fixture.session.doReject(fixture.message, reject); err != nil {
		return err
	}
	if err := completeScopedControl(fixture); err != nil {
		return err
	}
	message := fixture.peer.Message()
	messageType, fieldErr := message.Header.GetBytes(tagMsgType)
	if fieldErr != nil || string(messageType) != "3" {
		return io.ErrUnexpectedEOF
	}
	actualSequence, fieldErr := message.Header.GetInt(tagMsgSeqNum)
	if fieldErr != nil || actualSequence != sequence || fixture.store.NextSenderMsgSeqNum() != sequence+1 {
		return ErrInvalidJournalSequence
	}
	reference, fieldErr := message.Body.GetInt(tagRefTagID)
	if fieldErr != nil || reference != 54 {
		return io.ErrUnexpectedEOF
	}
	reason, fieldErr := message.Body.GetInt(tagSessionRejectReason)
	if fieldErr != nil || reason != 1 {
		return io.ErrUnexpectedEOF
	}
	index := fixture.store.nextEntry(sequence, sequence)
	if index < 0 || !bytes.Equal(fixture.store.entryBytes(index), fixture.connection.wire[:fixture.connection.used]) {
		return io.ErrUnexpectedEOF
	}
	fixture.connection.used = 0
	fixture.floor++
	return fixture.store.RetireBefore(fixture.floor)
}

func BenchmarkSessionRequiredValidation(benchmark *testing.B) {
	for _, scenario := range []struct {
		name        string
		missingSide bool
	}{{"valid", false}, {"missing_side", true}} {
		benchmark.Run(scenario.name, func(benchmark *testing.B) {
			fixture := newRequiredValidationFixture(benchmark, scenario.missingSide)
			defer fixture.close()
			for range 66 {
				requiredValidationResult = fixture.session.Validate(fixture.message)
			}
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for range benchmark.N {
				requiredValidationResult = fixture.session.Validate(fixture.message)
				if requiredValidationResult != nil {
					requiredValidationText = requiredValidationResult.Error()
					requiredValidationText = requiredValidationResult.Error()
				}
			}
			benchmark.StopTimer()
		})
	}
}

func BenchmarkSessionRequiredReject(benchmark *testing.B) {
	fixture := newRequiredValidationFixture(benchmark, true)
	defer fixture.close()
	for range 66 {
		if err := completeRequiredValidationReject(fixture); err != nil {
			benchmark.Fatal(err)
		}
	}
	benchmark.ReportAllocs()
	benchmark.ResetTimer()
	for range benchmark.N {
		if err := completeRequiredValidationReject(fixture); err != nil {
			benchmark.Fatal(err)
		}
	}
	benchmark.StopTimer()
}
