package quickfix

import (
	"fmt"
	"strconv"
	"testing"
)

var boundedEventGoldenFixtures = []struct {
	event  sequenceEvent
	format string
	text   string
	count  int
}{
	{eventSequenceTooHigh, "MsgSeqNum too high, expecting %v but received %v", "MsgSeqNum too high, expecting 12 but received 34", 2},
	{eventSentResendRequest, "Sent ResendRequest FROM: %v TO: %v", "Sent ResendRequest FROM: 12 TO: 34", 2},
	{eventReceivedResendRequest, "Received ResendRequest FROM: %d TO: %d", "Received ResendRequest FROM: 12 TO: 34", 2},
	{eventSentSequenceReset, "Sent SequenceReset TO: %v", "Sent SequenceReset TO: 12", 1},
	{eventReceivedSequenceReset, "Received SequenceReset FROM: %v TO: %v", "Received SequenceReset FROM: 12 TO: 34", 2},
	{eventResendingMessage, "Resending Message: %v", "Resending Message: 12", 1},
	{eventDrainingInbound, "Draining %d messages from inbound channel...", "Draining 12 messages from inbound channel...", 1},
}

// Deliberately keep legacy event arguments after the callback. This logger
// does not implement the borrowed capability, so that lifetime stays valid.
type boundedEventLegacyLog struct {
	format string
	args   []interface{}
	calls  int
}

func (*boundedEventLegacyLog) OnIncoming([]byte) {}
func (*boundedEventLegacyLog) OnOutgoing([]byte) {}
func (*boundedEventLegacyLog) OnEvent(string)    {}
func (log *boundedEventLegacyLog) OnEventf(format string, args ...interface{}) {
	log.format, log.args = format, args
	log.calls++
}

type boundedEventCaptureLog struct {
	boundedEventLegacyLog
	bytes         [128]byte
	length        int
	borrowedCalls int
	invalidCap    bool
}

func (log *boundedEventCaptureLog) OnEventBorrowed(event []byte) {
	log.length = copy(log.bytes[:], event)
	log.borrowedCalls++
	log.invalidCap = log.invalidCap || cap(event) != len(event) || len(event) > len(log.bytes)
}

func TestBoundedSequenceEventGoldenText(t *testing.T) {
	for _, fixture := range boundedEventGoldenFixtures {
		t.Run(fixture.text, func(t *testing.T) {
			log := &boundedEventCaptureLog{}
			s := &session{log: log, boundedIn: &boundedInboundSession{eventLog: log}}
			s.logSequenceEvent(fixture.event, 12, 34)
			if actual := string(log.bytes[:log.length]); actual != fixture.text {
				t.Fatalf("event = %q, want %q", actual, fixture.text)
			}
			if log.borrowedCalls != 1 || log.calls != 0 || log.invalidCap {
				t.Fatalf("borrowed calls = %d, legacy calls = %d, invalid view = %v", log.borrowedCalls, log.calls, log.invalidCap)
			}
		})
	}
}

func TestBoundedSequenceEventIntegerBounds(t *testing.T) {
	maximum := int(^uint(0) >> 1)
	minimum := -maximum - 1
	var buffer boundedEventBuffer
	for _, fixture := range boundedEventGoldenFixtures {
		for _, pair := range [][2]int{{minimum, maximum}, {maximum, minimum}, {minimum, minimum}, {0, -1}} {
			var expected string
			if fixture.count == 2 {
				expected = fmt.Sprintf(fixture.format, pair[0], pair[1])
			} else {
				expected = fmt.Sprintf(fixture.format, pair[0])
			}
			actual := buffer.format(fixture.event, pair[0], pair[1])
			if string(actual) != expected || cap(actual) != len(actual) || len(actual) > len(buffer.bytes) {
				t.Fatalf("event %d (%d, %d) = %q with capacity %d, want %q", fixture.event, pair[0], pair[1], actual, cap(actual), expected)
			}
		}
	}
	// Explicitly pin the longest signed representations, independently of the
	// legacy formatter used by the differential cases above.
	expected := "MsgSeqNum too high, expecting -9223372036854775808 but received 9223372036854775807"
	if strconv.IntSize == 32 {
		expected = "MsgSeqNum too high, expecting -2147483648 but received 2147483647"
	}
	if actual := string(buffer.format(eventSequenceTooHigh, minimum, maximum)); actual != expected {
		t.Fatalf("integer-bound golden = %q, want %q", actual, expected)
	}
}

func TestBoundedSequenceEventLegacyLifetime(t *testing.T) {
	for _, fixture := range boundedEventGoldenFixtures {
		for _, bounded := range []bool{false, true} {
			log := &boundedEventLegacyLog{}
			s := &session{log: log}
			if bounded {
				s.boundedIn = &boundedInboundSession{}
			}
			s.logSequenceEvent(fixture.event, 12, 34)
			retainedFormat, retainedArgs := log.format, log.args
			if retainedFormat != fixture.format || len(retainedArgs) != fixture.count || retainedArgs[0] != 12 {
				t.Fatalf("legacy callback = %q, %v; want %q with %d integers", retainedFormat, retainedArgs, fixture.format, fixture.count)
			}
			if fixture.count == 2 && retainedArgs[1] != 34 {
				t.Fatalf("second legacy argument = %v, want 34", retainedArgs[1])
			}
			s.logSequenceEvent(eventResendingMessage, -1, 0)
			if s.boundedIn != nil {
				for index := range s.boundedIn.events.bytes {
					s.boundedIn.events.bytes[index] = 'x'
				}
			}
			if actual := fmt.Sprintf(retainedFormat, retainedArgs...); actual != fixture.text {
				t.Fatalf("retained legacy event = %q, want %q", actual, fixture.text)
			}
		}
	}
}

func TestBoundedSequenceEventCapabilityRequiresOwnedScratch(t *testing.T) {
	log := &boundedEventCaptureLog{}
	s := &session{log: log}
	s.logSequenceEvent(eventResendingMessage, 12, 0)
	if log.borrowedCalls != 0 || log.calls != 1 || log.format != "Resending Message: %v" {
		t.Fatal("an unbounded session must retain its legacy logging path")
	}
	var _ BorrowedEventLog = nullLog{}
	var _ BorrowedLog = nullLog{}
}

func TestBoundedSequenceEventsWarmedNoAllocations(t *testing.T) {
	log := &boundedEventCaptureLog{}
	s := &session{log: log, boundedIn: &boundedInboundSession{eventLog: log}}
	minimum := -int(^uint(0)>>1) - 1
	maximum := int(^uint(0) >> 1)
	if allocations := testing.AllocsPerRun(1000, func() {
		for _, fixture := range boundedEventGoldenFixtures {
			s.logSequenceEvent(fixture.event, minimum, maximum)
		}
	}); allocations != 0 {
		t.Fatalf("numeric events allocated %g times", allocations)
	}
	if log.calls != 0 || log.invalidCap {
		t.Fatal("bounded event path used legacy logging or invalid byte views")
	}
}
