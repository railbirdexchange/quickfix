//go:build !race

package quickfix

import (
	"runtime"
	"testing"
)

func TestConfiguredValidationRejectExactAllocations(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	for _, scenario := range configuredValidationScenarios() {
		if scenario.name == "unknown_tag" || scenario.name == "invalid_timestamp" {
			continue
		}
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newConfiguredValidationFixture(t, scenario)
			for range 66 {
				requiredValidationResult = fixture.validator.Validate(fixture.slot.Message())
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for range 1000 {
				requiredValidationResult = fixture.validator.Validate(fixture.slot.Message())
				if requiredValidationResult != nil {
					requiredValidationText = requiredValidationResult.Error()
					requiredValidationText = requiredValidationResult.Error()
				}
			}
			runtime.ReadMemStats(&after)
			t.Logf("validations=1000 mallocs=%d bytes=%d", after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc)
			if after.Mallocs != before.Mallocs || after.TotalAlloc != before.TotalAlloc {
				t.Fatal("configured static validation result allocated")
			}
		})
	}
}

func TestConfiguredCompletedRejectExactAllocations(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	for _, scenario := range configuredValidationScenarios() {
		if scenario.name != "enum" && scenario.name != "group_count" {
			continue
		}
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newConfiguredCompletedRejectFixture(t, scenario)
			defer fixture.close()
			for range 66 {
				if err := fixture.complete(); err != nil {
					t.Fatal(err)
				}
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for range 100 {
				if err := fixture.complete(); err != nil {
					t.Fatal(err)
				}
			}
			runtime.ReadMemStats(&after)
			t.Logf("completed_rejects=100 mallocs=%d bytes=%d", after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc)
			if after.Mallocs != before.Mallocs || after.TotalAlloc != before.TotalAlloc {
				t.Fatal("completed configured reject allocated")
			}
		})
	}
}

func TestConfiguredGroupReaderAllocationBoundary(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	for _, kind := range []string{"NUMINGROUP", "STRING"} {
		t.Run(kind, func(t *testing.T) {
			scenario := configuredValidationScenario{
				name: "group_reader", nested: true, old: "1000=2", replacement: "1000=x",
				expected: IncorrectDataFormatForValue(1000),
			}
			wire, _, dictionary := configuredValidationInput(t, scenario)
			dictionary.FieldTypeByTag[1000].Type = kind
			message := boundedValidationParse(t, wire)
			validator, err := newSessionValidator(defaultValidatorSettings, dictionary, nil, 64)
			if err != nil {
				t.Fatal(err)
			}
			assertBoundedRejectEqual(t, validator.Validate(message), scenario.expected)
			assertBoundedRejectEqual(t, NewValidator(defaultValidatorSettings, dictionary, nil).Validate(message), scenario.expected)
			for range 66 {
				requiredValidationResult = validator.Validate(message)
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for range 1000 {
				requiredValidationResult = validator.Validate(message)
				requiredValidationText = requiredValidationResult.Error()
				requiredValidationText = requiredValidationResult.Error()
			}
			runtime.ReadMemStats(&after)
			t.Logf("kind=%s validations=1000 mallocs=%d bytes=%d", kind, after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc)
			if kind == "NUMINGROUP" && (after.Mallocs != before.Mallocs || after.TotalAlloc != before.TotalAlloc) {
				t.Fatal("ordinary group format rejection allocated")
			}
			// STRING intentionally reaches the unchanged FIXInt reader. Record
			// its actual remainder without claiming this custom path is zero.
		})
	}
}

func TestStaticRejectExactAllocations(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	for _, scenario := range staticRejectScenarios() {
		t.Run(scenario.name, func(t *testing.T) {
			for range 66 {
				requiredValidationResult = scenario.construct()
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for range 1000 {
				requiredValidationResult = scenario.construct()
				requiredValidationText = requiredValidationResult.Error()
				requiredValidationText = requiredValidationResult.Error()
			}
			runtime.ReadMemStats(&after)
			t.Logf("constructors=1000 mallocs=%d bytes=%d", after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc)
			if after.Mallocs != before.Mallocs || after.TotalAlloc != before.TotalAlloc {
				t.Fatal("immutable parameter-free reject allocated")
			}
		})
	}
	t.Run("invalid_message_type_validation", func(t *testing.T) {
		fixture := newInvalidMessageTypeFixture(t)
		for range 66 {
			requiredValidationResult = fixture.validator.Validate(fixture.slot.Message())
		}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for range 1000 {
			requiredValidationResult = fixture.validator.Validate(fixture.slot.Message())
			requiredValidationText = requiredValidationResult.Error()
			requiredValidationText = requiredValidationResult.Error()
		}
		runtime.ReadMemStats(&after)
		t.Logf("validations=1000 mallocs=%d bytes=%d", after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc)
		if after.Mallocs != before.Mallocs || after.TotalAlloc != before.TotalAlloc {
			t.Fatal("invalid message type validation allocated")
		}
	})
}
