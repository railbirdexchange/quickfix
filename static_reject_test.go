package quickfix

import (
	"sync"
	"testing"
)

func TestStaticRejectConstructorParityAndRetention(t *testing.T) {
	for _, scenario := range staticRejectScenarios() {
		t.Run(scenario.name, func(t *testing.T) {
			retained := scenario.construct()
			if _, sameConcreteType := retained.(messageRejectError); !sameConcreteType {
				t.Fatalf("changed concrete error representation: %T", retained)
			}
			for _, other := range staticRejectScenarios() {
				requiredValidationResult = other.construct()
			}
			var readers sync.WaitGroup
			for range 4 {
				readers.Add(1)
				go func() {
					defer readers.Done()
					for range 100 {
						if actual := snapshotBoundedReject(retained); actual != scenario.expected {
							t.Errorf("retained constructor result changed: got %#v, want %#v", actual, scenario.expected)
							return
						}
					}
				}()
			}
			readers.Wait()
		})
	}
}

func TestStaticRejectInvalidMessageTypeRetention(t *testing.T) {
	fixture := newInvalidMessageTypeFixture(t)
	retained := fixture.validator.Validate(fixture.slot.Message())
	owned, err := fixture.slot.Message().Header.GetString(tagMsgType)
	if err != nil || owned != "UNKNOWN.TYPE" {
		t.Fatalf("owned type: %q %v", owned, err)
	}
	if err := fixture.slot.Parse(fixture.valid); err != nil {
		t.Fatal(err)
	}
	if reject := fixture.validator.Validate(fixture.slot.Message()); reject != nil {
		t.Fatalf("valid message after invalid type: %v", reject)
	}
	fixture.slot.Reset()
	fixture.validator = nil
	assertBoundedRejectEqual(t, retained, InvalidMessageType())
	if owned != "UNKNOWN.TYPE" {
		t.Fatal("retained owned type changed after slot reuse")
	}
}
