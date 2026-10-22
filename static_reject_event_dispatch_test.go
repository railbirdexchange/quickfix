package quickfix

import "testing"

func TestStaticRejectEventOwnedDispatch(t *testing.T) {
	for _, reason := range fixedRejectEventReasons() {
		t.Run(reason, func(t *testing.T) {
			s, log := newRetainedRejectEventSession(256, true, true)
			s.logControlReject(reason)
			if log.ownedCalls != 1 || log.borrowedCalls != 0 || log.formattedCalls != 0 {
				t.Fatalf("fixed event dispatch: owned=%d borrowed=%d formatted=%d",
					log.ownedCalls, log.borrowedCalls, log.formattedCalls)
			}
			if log.messages[0] != "Message Rejected: "+reason {
				t.Fatal("fixed owned event text changed")
			}
		})
	}
}

func TestStaticRejectEventUsesSelectedCapability(t *testing.T) {
	s, selected := newRetainedRejectEventSession(256, true, true)
	replacement := &retainedRejectEventLog{enabled: true}
	s.log = replacement
	s.logControlReject("Required tag missing")
	if selected.ownedCalls != 1 || selected.messages[0] != "Message Rejected: Required tag missing" ||
		replacement.next != 0 || replacement.formattedCalls != 0 {
		t.Fatal("fixed event bypassed the selected bounded logging capability")
	}
}
