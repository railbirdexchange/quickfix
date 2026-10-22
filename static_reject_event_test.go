package quickfix

import (
	"fmt"
	"testing"
)

func fixedRejectEventReasons() []string {
	return []string{
		"Required tag missing",
		"Incorrect data format for value",
		"Value is incorrect (out of range) for this tag",
		"Tag not defined for this message type",
		"Tag appears more than once",
		"Incorrect NumInGroup count for repeating group",
		"Tag specified out of required order",
		"Tag specified without a value",
		"Invalid MsgType",
		"Unsupported Message Type",
		"Invalid tag number",
		"CompID problem",
		"SendingTime accuracy problem",
		"Repeating group fields out of order",
	}
}

func TestStaticRejectEventTextAndRetainedLifetime(t *testing.T) {
	reasons := append(fixedRejectEventReasons(),
		"", "custom client A-123 rejected", "quotes\" slash\\ newline\n SOH\x01 café 世界", "invalid\xffutf8")
	for _, reason := range reasons {
		t.Run(reason, func(t *testing.T) {
			s, log := newRetainedRejectEventSession(256, true, true)
			s.logControlReject(reason)
			retained := log.messages[0]
			expected := "Message Rejected: " + reason
			if retained != expected || log.next != 1 || log.formattedCalls != 0 {
				t.Fatalf("event=%q count=%d formatted=%d, want %q", retained, log.next, log.formattedCalls, expected)
			}
			clear(s.controls.events)
			for range 2 * len(log.messages) {
				s.logControlReject("replacement diagnostic")
			}
			clear(s.controls.events)
			if retained != expected {
				t.Fatalf("retained event changed across scratch/ring reuse: %q", retained)
			}
		})
	}
}

func TestStaticRejectEventDynamicDispatch(t *testing.T) {
	for _, reason := range []string{"", "custom client A-123 rejected", "Required tag missing ", "Invalid MsgType\n"} {
		s, log := newRetainedRejectEventSession(256, true, true)
		s.logControlReject(reason)
		if log.borrowedCalls != 1 || log.ownedCalls != 0 || log.formattedCalls != 0 {
			t.Fatalf("dynamic reason %q changed dispatch: owned=%d borrowed=%d formatted=%d",
				reason, log.ownedCalls, log.borrowedCalls, log.formattedCalls)
		}
	}
}

func TestStaticRejectEventCapacityAndLegacyLifetime(t *testing.T) {
	const reason = "Value is incorrect (out of range) for this tag"
	const expected = "Message Rejected: " + reason
	for _, capacity := range []int{len(expected), len(expected) - 1, len("Message Rejected: ") - 1, 0} {
		for _, borrowed := range []bool{false, true} {
			s, log := newRetainedRejectEventSession(capacity, true, borrowed)
			s.logControlReject(reason)
			if log.messages[0] != expected {
				t.Fatalf("capacity=%d borrowed=%v event=%q", capacity, borrowed, log.messages[0])
			}
			if borrowed && capacity >= len(expected) {
				if log.formattedCalls != 0 || log.ownedCalls+log.borrowedCalls != 1 {
					t.Fatal("sufficient bounded storage used the legacy formatter")
				}
				continue
			}
			if log.formattedCalls != 1 || log.ownedCalls != 0 || log.borrowedCalls != 0 ||
				log.format != "Message Rejected: %v" || len(log.args) != 1 || log.args[0] != reason {
				t.Fatal("legacy formatter callback or retained arguments changed")
			}
			retainedFormat, retainedArgs := log.format, log.args
			clear(s.controls.events)
			s.logControlReject("replacement")
			if fmt.Sprintf(retainedFormat, retainedArgs...) != expected {
				t.Fatal("legacy arguments changed after subsequent event")
			}
		}
	}
}

func TestStaticRejectEventCapabilityNeedsBoundedSession(t *testing.T) {
	for _, bounded := range []bool{false, true} {
		log := &retainedRejectEventLog{enabled: true}
		s := &session{log: log}
		if bounded {
			s.boundedIn = &boundedInboundSession{}
		}
		s.logControlReject("Required tag missing")
		if log.formattedCalls != 1 || log.ownedCalls != 0 || log.borrowedCalls != 0 {
			t.Fatal("missing bounded capability changed legacy dispatch")
		}
	}
}

func TestStaticRejectEventFilteredHook(t *testing.T) {
	for _, reason := range append(fixedRejectEventReasons(), "custom rejection") {
		s, log := newRetainedRejectEventSession(256, false, true)
		s.logControlReject(reason)
		if log.next != 0 || log.formattedCalls != 0 || log.ownedCalls+log.borrowedCalls != 1 {
			t.Fatal("filtered event was retained or delivered more than once")
		}
	}
}
