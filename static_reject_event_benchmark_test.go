package quickfix

import (
	"fmt"
	"testing"
)

// This logger models an enabled hook retaining its message string. Borrowed
// bytes must be copied; an owned string can be retained directly. The fixed
// ring avoids adding collection growth to the measured logging boundary.
type retainedRejectEventLog struct {
	messages                                  [4]string
	next                                      int
	enabled                                   bool
	ownedCalls, borrowedCalls, formattedCalls int
	format                                    string
	args                                      []interface{}
}

func (*retainedRejectEventLog) OnIncoming([]byte) {}
func (*retainedRejectEventLog) OnOutgoing([]byte) {}

func (log *retainedRejectEventLog) retain(message string) {
	log.messages[log.next%len(log.messages)] = message
	log.next++
}

func (log *retainedRejectEventLog) OnEvent(message string) {
	log.ownedCalls++
	if log.enabled {
		log.retain(message)
	}
}

func (log *retainedRejectEventLog) OnEventBorrowed(message []byte) {
	log.borrowedCalls++
	if log.enabled {
		log.retain(string(message))
	}
}

func (log *retainedRejectEventLog) OnEventf(format string, args ...interface{}) {
	log.formattedCalls++
	log.format, log.args = format, args
	if log.enabled {
		log.retain(fmt.Sprintf(format, args...))
	}
}

// Explicit forwarding deliberately does not promote OnEventBorrowed.
type retainedRejectLegacyLog struct {
	target *retainedRejectEventLog
}

func (*retainedRejectLegacyLog) OnIncoming([]byte)          {}
func (*retainedRejectLegacyLog) OnOutgoing([]byte)          {}
func (log *retainedRejectLegacyLog) OnEvent(message string) { log.target.OnEvent(message) }
func (log *retainedRejectLegacyLog) OnEventf(format string, args ...interface{}) {
	log.target.OnEventf(format, args...)
}

func newRetainedRejectEventSession(capacity int, enabled, borrowed bool) (*session, *retainedRejectEventLog) {
	log := &retainedRejectEventLog{enabled: enabled}
	s := &session{
		log:       log,
		boundedIn: &boundedInboundSession{},
	}
	s.controls.events = make([]byte, capacity)
	if borrowed {
		s.boundedIn.eventLog = log
	} else {
		s.log = &retainedRejectLegacyLog{target: log}
	}
	return s, log
}

func BenchmarkStaticRejectEvent(benchmark *testing.B) {
	for _, scenario := range []struct {
		name, reason      string
		enabled, borrowed bool
	}{
		{"required", "Required tag missing", true, true},
		{"enum", "Value is incorrect (out of range) for this tag", true, true},
		{"format", "Incorrect data format for value", true, true},
		{"dynamic", "custom reject for client order A-123", true, true},
		{"filtered", "Value is incorrect (out of range) for this tag", false, true},
		{"legacy", "Value is incorrect (out of range) for this tag", true, false},
	} {
		benchmark.Run(scenario.name, func(benchmark *testing.B) {
			s, log := newRetainedRejectEventSession(256, scenario.enabled, scenario.borrowed)
			expected := "Message Rejected: " + scenario.reason
			for range 66 {
				s.logControlReject(scenario.reason)
			}
			if scenario.enabled && log.messages[(log.next-1)%len(log.messages)] != expected {
				benchmark.Fatal("warm-up event text changed")
			}
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for range benchmark.N {
				s.logControlReject(scenario.reason)
			}
			benchmark.StopTimer()
			if scenario.enabled && log.messages[(log.next-1)%len(log.messages)] != expected {
				benchmark.Fatal("retained event text changed")
			}
			if !scenario.enabled && log.next != 0 {
				benchmark.Fatal("filtered event reached retaining hook")
			}
		})
	}
}
