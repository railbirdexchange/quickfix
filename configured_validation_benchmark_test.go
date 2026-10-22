package quickfix

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/quickfixgo/quickfix/datadictionary"
)

type configuredValidationScenario struct {
	name, old, replacement string
	expected               MessageRejectError
	nested                 bool
}

func configuredValidationScenarios() []configuredValidationScenario {
	return []configuredValidationScenario{
		{name: "valid"},
		{name: "format", old: "38=1\x01", replacement: "38=bad\x01", expected: IncorrectDataFormatForValue(38)},
		{name: "enum", old: "54=1\x01", replacement: "54=Z\x01", expected: ValueIsIncorrect(54)},
		{name: "empty", old: "11=13976\x01", replacement: "11=\x01", expected: TagSpecifiedWithoutAValue(11)},
		{name: "duplicate", old: "11=13976\x01", replacement: "11=13976\x0111=second\x01", expected: tagAppearsMoreThanOnce(11)},
		{name: "group_count", old: "453=1\x01", replacement: "453=2\x01", expected: incorrectNumInGroupCountForRepeatingGroup(453)},
		{name: "order", old: "49=01001\x01", replacement: "", expected: tagSpecifiedOutOfRequiredOrder(49)},
		{name: "not_defined", old: "38=1\x01", replacement: "38=1\x017=1\x01", expected: TagNotDefinedForThisMessageType(7)},
		{name: "required_group", old: "2002=av\x01", replacement: "", expected: RequiredTagMissing(2002), nested: true},
		{name: "unknown_tag", old: "38=1\x01", replacement: "38=1\x014999=unknown\x01", expected: InvalidTagNumber(4999)},
		{name: "invalid_timestamp", old: "52=20231231-20:19:41\x01", replacement: "52=bad\x01", expected: IncorrectDataFormatForValue(52)},
	}
}

func configuredValidationInput(test testing.TB, scenario configuredValidationScenario) (wire, valid []byte, dictionary *datadictionary.DataDictionary) {
	test.Helper()
	var body string
	if scenario.nested {
		dictionary = boundedValidationNestedDictionary()
		child := dictionary.Messages["D"].Fields[1000].Fields[2].Fields[1]
		dictionary.Messages["D"].Fields[1000].Fields[2].Fields[1] = datadictionary.NewFieldDef(child.FieldType, true)
		body = borrowedNestedWire
	} else {
		_, dictionary = boundedOwnershipFixture(test, true)
		body = strings.SplitN(allocationBaselineRepeatingGroupMessage, "\x01", 4)[3]
		body = body[:strings.LastIndex(body, "10=")]
	}
	valid = boundedValidationNestedWire(body)
	if scenario.old != "" {
		if !strings.Contains(body, scenario.old) {
			test.Fatalf("missing fixture replacement %q", scenario.old)
		}
		body = strings.Replace(body, scenario.old, scenario.replacement, 1)
	}
	if scenario.name == "order" {
		body += "49=01001\x01"
	}
	return boundedValidationNestedWire(body), valid, dictionary
}

type configuredValidationFixture struct {
	slot      *MessageSlot
	validator Validator
	valid     []byte
}

func newConfiguredValidationFixture(test testing.TB, scenario configuredValidationScenario) *configuredValidationFixture {
	test.Helper()
	wire, valid, dictionary := configuredValidationInput(test, scenario)
	slot, err := NewMessageSlot(dictionary, dictionary, DefaultParseLimits())
	if err != nil {
		test.Fatal(err)
	}
	if err := slot.Parse(wire); err != nil {
		test.Fatal(err)
	}
	validator, err := newSessionValidator(defaultValidatorSettings, dictionary, nil, DefaultParseLimits().MaxFields)
	if err != nil {
		test.Fatal(err)
	}
	assertBoundedRejectEqual(test, validator.Validate(slot.Message()), scenario.expected)
	assertBoundedRejectEqual(test, NewValidator(defaultValidatorSettings, dictionary, nil).Validate(slot.Message()), scenario.expected)
	return &configuredValidationFixture{slot: slot, validator: validator, valid: valid}
}

// The retained result and repeated text reads prevent a benchmark from hiding
// construction through dead-result elimination. Setup and warm-up are excluded.
func BenchmarkConfiguredValidationReject(benchmark *testing.B) {
	for _, scenario := range configuredValidationScenarios() {
		benchmark.Run(scenario.name, func(benchmark *testing.B) {
			fixture := newConfiguredValidationFixture(benchmark, scenario)
			for range 66 {
				requiredValidationResult = fixture.validator.Validate(fixture.slot.Message())
			}
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for range benchmark.N {
				requiredValidationResult = fixture.validator.Validate(fixture.slot.Message())
				if requiredValidationResult != nil {
					requiredValidationText = requiredValidationResult.Error()
					requiredValidationText = requiredValidationResult.Error()
				}
			}
			benchmark.StopTimer()
		})
	}
}

type configuredCompletedRejectFixture struct {
	*scopedOutboundBenchmark
	application *scopedControlApplication
	expected    MessageRejectError
}

func newConfiguredCompletedRejectFixture(test testing.TB, scenario configuredValidationScenario) *configuredCompletedRejectFixture {
	test.Helper()
	fixture := newScopedOutboundBenchmarkWithDelivery(test, true, "", true)
	wire, _, _ := configuredValidationInput(test, scenario)
	if err := fixture.session.boundedIn.parsed.Parse(wire); err != nil {
		fixture.close()
		test.Fatal(err)
	}
	fixture.message = fixture.session.boundedIn.parsed.Message()
	assertBoundedRejectEqual(test, fixture.session.Validate(fixture.message), scenario.expected)
	application := &scopedControlApplication{}
	fixture.session.application = BorrowApplication(application)
	return &configuredCompletedRejectFixture{scopedOutboundBenchmark: fixture, application: application, expected: scenario.expected}
}

// Includes validation, reject construction, borrowed callback, bounded journal,
// encoding, completed writer copy, slot return, peer parse and journal retirement.
// This is a QuickFIX completed-write component, not TCP or round-trip latency.
func (fixture *configuredCompletedRejectFixture) complete() error {
	reject := fixture.session.Validate(fixture.message)
	if snapshotBoundedReject(reject) != snapshotBoundedReject(fixture.expected) {
		return io.ErrUnexpectedEOF
	}
	sequence := fixture.store.NextSenderMsgSeqNum()
	callbacks := fixture.application.calls
	if err := fixture.session.doReject(fixture.message, reject); err != nil {
		return err
	}
	if err := completeScopedControl(fixture.scopedOutboundBenchmark); err != nil {
		return err
	}
	if fixture.application.calls != callbacks+1 || fixture.session.controls.busy {
		return io.ErrUnexpectedEOF
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
	expectedTag, _ := fixture.expected.RefTagID()
	if fieldErr != nil || reference != int(expectedTag) {
		return io.ErrUnexpectedEOF
	}
	reason, fieldErr := message.Body.GetInt(tagSessionRejectReason)
	if fieldErr != nil || reason != fixture.expected.RejectReason() {
		return io.ErrUnexpectedEOF
	}
	text, fieldErr := message.Body.GetBytes(tagText)
	if fieldErr != nil || string(text) != fixture.expected.Error() {
		return io.ErrUnexpectedEOF
	}
	referenceSequence, fieldErr := message.Body.GetInt(tagRefSeqNum)
	if fieldErr != nil || referenceSequence != 2 {
		return ErrInvalidJournalSequence
	}
	referenceType, fieldErr := message.Body.GetBytes(tagRefMsgType)
	if fieldErr != nil || string(referenceType) != "D" {
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

func BenchmarkConfiguredCompletedReject(benchmark *testing.B) {
	for _, scenario := range configuredValidationScenarios() {
		if scenario.name != "enum" && scenario.name != "group_count" {
			continue
		}
		benchmark.Run(scenario.name, func(benchmark *testing.B) {
			fixture := newConfiguredCompletedRejectFixture(benchmark, scenario)
			defer fixture.close()
			for range 66 {
				if err := fixture.complete(); err != nil {
					benchmark.Fatal(err)
				}
			}
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for range benchmark.N {
				if err := fixture.complete(); err != nil {
					benchmark.Fatal(err)
				}
			}
			benchmark.StopTimer()
		})
	}
}

var configuredValidationSetupResult Validator

func BenchmarkConfiguredValidationSetup(benchmark *testing.B) {
	_, dictionary := boundedOwnershipFixture(benchmark, true)
	benchmark.ReportAllocs()
	benchmark.ResetTimer()
	for range benchmark.N {
		var err error
		configuredValidationSetupResult, err = newSessionValidator(defaultValidatorSettings, dictionary, nil, DefaultParseLimits().MaxFields)
		if err != nil {
			benchmark.Fatal(err)
		}
	}
}
