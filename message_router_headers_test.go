// Copyright (c) quickfixengine.org All rights reserved.
// See LICENSE for licensing information.

package quickfix

import "testing"

func TestMessageRouterMissingRequiredHeaders(t *testing.T) {
	for _, fixture := range []struct {
		name, beginString, msgType string
		missing                    Tag
	}{
		{"application_without_begin_string", "", "D", tagBeginString},
		{"admin_without_begin_string", "", "0", tagBeginString},
		{"business_reject_without_begin_string", "", "j", tagBeginString},
		{"both_headers_missing", "", "", tagBeginString},
		{"message_type_missing", BeginStringFIX44, "", tagMsgType},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			message := NewMessage()
			if fixture.beginString != "" {
				message.Header.SetString(tagBeginString, fixture.beginString)
			}
			if fixture.msgType != "" {
				message.Header.SetString(tagMsgType, fixture.msgType)
			}
			router := NewMessageRouter()
			router.AddRoute(BeginStringFIX44, fixture.msgType, func(*Message, SessionID) MessageRejectError {
				t.Error("handler called with a missing routing header")
				return nil
			})
			// A caller-supplied SessionID must not hide a missing message header.
			reject := router.Route(message, SessionID{BeginString: BeginStringFIX44})
			if reject == nil {
				t.Fatal("missing routing header was reported as success")
			}
			refTag, present := reject.RefTagID()
			want := ConditionallyRequiredFieldMissing(fixture.missing)
			if !present || refTag != fixture.missing || reject.RejectReason() != want.RejectReason() || reject.IsBusinessReject() != want.IsBusinessReject() || reject.Error() != want.Error() {
				t.Fatalf("got %v with reference (%d, %t), want %v", reject, refTag, present, want)
			}
		})
	}
}

func routingMissingBeginStringMessage(t testing.TB, prepared bool) *Message {
	t.Helper()
	if !prepared {
		message := NewMessage()
		message.Header.SetString(tagMsgType, "D")
		return message
	}
	wire, dictionary := boundedOwnershipFixture(t, false)
	slot, err := NewMessageSlot(dictionary, dictionary, DefaultParseLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := slot.Parse(wire); err != nil {
		t.Fatal(err)
	}
	message := slot.Message()
	message.Header.Remove(tagBeginString)
	return message
}

func TestMessageRouterReturnsPreparedHeaderReject(t *testing.T) {
	message := routingMissingBeginStringMessage(t, true)
	want := message.Header.metadata.missingRejects[tagBeginString]
	if want == nil {
		t.Fatal("fixture has no prepared BeginString reject")
	}
	router := NewMessageRouter()
	reject := router.Route(message, SessionID{})
	if reject != want {
		t.Fatalf("router did not return the existing getter error: %v", reject)
	}
	if allocations := testing.AllocsPerRun(100, func() {
		missingFieldRejectResult = router.Route(message, SessionID{})
	}); allocations != 0 {
		t.Fatalf("prepared routing rejection allocated %v objects", allocations)
	}
	message.parseSlot.Reset()
	if refTag, present := reject.RefTagID(); !present || refTag != tagBeginString || reject.Error() != "Conditionally Required Field Missing (8)" {
		t.Fatalf("retained reject changed after message reset: %v", reject)
	}
}

// Both versions retain the Route result. The parent discards the getter's
// missing-field error; the candidate returns it. Setup is outside timing.
func BenchmarkMessageRouterMissingBeginString(b *testing.B) {
	for _, prepared := range []bool{false, true} {
		name := "owned"
		if prepared {
			name = "prepared"
		}
		b.Run(name, func(b *testing.B) {
			message := routingMissingBeginStringMessage(b, prepared)
			router := NewMessageRouter()
			missingFieldRejectResult = router.Route(message, SessionID{})
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				missingFieldRejectResult = router.Route(message, SessionID{})
			}
		})
	}
}
