package quickfix

import "testing"

func newGroupValuesFullSession(t *testing.T) *boundedFullSession {
	t.Helper()
	fixture := newBoundedFullSession(t, true)
	if err := fixture.configure(func(application *boundedFullApplication) {
		values := []GroupField{
			{Tag: 448, Text: "party"}, {Tag: 447, Text: "D"}, {Tag: 452, Text: "1"},
			{Tag: 802, Text: "1"}, {Tag: 523, Text: "sub-party"}, {Tag: 803, Text: "1"},
		}
		application.onOrder = func(message *Message) MessageRejectError {
			if err := application.response.Body.SetGroupValues(453, 1, values); err != nil {
				return application.fail(err)
			}
			return application.handleOrder(message)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.warm(); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestGroupValuesCompletedSession(t *testing.T) {
	fixture := newGroupValuesFullSession(t)
	for _, scenario := range []string{"groups", "outbound_resend", "inbound_gap", "slow_consumer"} {
		if scenario == "slow_consumer" {
			if err := fixture.prepareSlowConsumer(); err != nil {
				t.Fatal(err)
			}
		}
		if err := fixture.scenario(scenario)(); err != nil {
			t.Fatalf("%s: %v", scenario, err)
		}
	}
}
