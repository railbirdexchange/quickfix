package quickfix

import (
	"sync"
	"testing"
)

func TestConfiguredValidationRejectParityAndRetention(t *testing.T) {
	for _, scenario := range configuredValidationScenarios() {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newConfiguredValidationFixture(t, scenario)
			retained := fixture.validator.Validate(fixture.slot.Message())
			expected := snapshotBoundedReject(scenario.expected)
			if err := fixture.slot.Parse(fixture.valid); err != nil {
				t.Fatal(err)
			}
			if reject := fixture.validator.Validate(fixture.slot.Message()); reject != nil {
				t.Fatalf("valid message after rejection: %v", reject)
			}
			fixture.slot.Reset()
			fixture.validator = nil
			var readers sync.WaitGroup
			for range 4 {
				readers.Add(1)
				go func() {
					defer readers.Done()
					for range 100 {
						if snapshotBoundedReject(retained) != expected {
							t.Error("retained error changed after slot/validator reuse")
							return
						}
					}
				}()
			}
			readers.Wait()
		})
	}
}

func TestConfiguredValidationCompletedRejectWire(t *testing.T) {
	for _, scenario := range configuredValidationScenarios() {
		if scenario.expected == nil || scenario.nested {
			continue
		}
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newConfiguredCompletedRejectFixture(t, scenario)
			defer fixture.close()
			retained := fixture.session.Validate(fixture.message)
			for range 3 {
				if err := fixture.complete(); err != nil {
					t.Fatal(err)
				}
			}
			fixture.session.boundedIn.parsed.Reset()
			assertBoundedRejectEqual(t, retained, scenario.expected)
		})
	}
}
