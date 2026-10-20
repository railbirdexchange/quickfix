package quickfix

import "testing"

type staticRejectScenario struct {
	name      string
	construct func() MessageRejectError
	expected  boundedRejectSnapshot
}

func staticRejectScenarios() []staticRejectScenario {
	return []staticRejectScenario{
		{
			name: "invalid_message_type", construct: InvalidMessageType,
			expected: boundedRejectSnapshot{text: "Invalid MsgType", reason: 11, present: true},
		},
		{
			name: "unsupported_message_type", construct: UnsupportedMessageType,
			expected: boundedRejectSnapshot{text: "Unsupported Message Type", reason: 3, present: true, business: true},
		},
		{
			name: "value_incorrect_no_tag", construct: valueIsIncorrectNoTag,
			expected: boundedRejectSnapshot{text: "Value is incorrect (out of range) for this tag", reason: 5, present: true},
		},
	}
}

func BenchmarkStaticRejectConstructor(benchmark *testing.B) {
	for _, scenario := range staticRejectScenarios() {
		benchmark.Run(scenario.name, func(benchmark *testing.B) {
			if actual := snapshotBoundedReject(scenario.construct()); actual != scenario.expected {
				benchmark.Fatalf("constructor parity: got %#v, want %#v", actual, scenario.expected)
			}
			for range 66 {
				requiredValidationResult = scenario.construct()
			}
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for range benchmark.N {
				requiredValidationResult = scenario.construct()
				requiredValidationText = requiredValidationResult.Error()
				requiredValidationText = requiredValidationResult.Error()
			}
			benchmark.StopTimer()
		})
	}
}

func newInvalidMessageTypeFixture(test testing.TB) *configuredValidationFixture {
	test.Helper()
	fixture := newConfiguredValidationFixture(test, configuredValidationScenario{name: "valid"})
	fixture.slot.Message().Header.SetString(tagMsgType, "UNKNOWN.TYPE")
	assertBoundedRejectEqual(test, fixture.validator.Validate(fixture.slot.Message()), InvalidMessageType())
	return fixture
}

var staticRejectOwnedMessageType string

func BenchmarkStaticRejectInvalidMessageType(benchmark *testing.B) {
	for _, ownedTypeRead := range []bool{false, true} {
		name := "validation"
		if ownedTypeRead {
			name = "validation_and_owned_type_read"
		}
		benchmark.Run(name, func(benchmark *testing.B) {
			fixture := newInvalidMessageTypeFixture(benchmark)
			for range 66 {
				requiredValidationResult = fixture.validator.Validate(fixture.slot.Message())
				if ownedTypeRead {
					staticRejectOwnedMessageType, _ = fixture.slot.Message().Header.GetString(tagMsgType)
				}
			}
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for range benchmark.N {
				requiredValidationResult = fixture.validator.Validate(fixture.slot.Message())
				requiredValidationText = requiredValidationResult.Error()
				requiredValidationText = requiredValidationResult.Error()
				if ownedTypeRead {
					staticRejectOwnedMessageType, _ = fixture.slot.Message().Header.GetString(tagMsgType)
				}
			}
			benchmark.StopTimer()
		})
	}
}
