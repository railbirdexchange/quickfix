package quickfix

import "strconv"

type sequenceEvent uint8

const (
	eventSequenceTooHigh sequenceEvent = iota
	eventSentResendRequest
	eventReceivedResendRequest
	eventSentSequenceReset
	eventReceivedSequenceReset
	eventResendingMessage
	eventDrainingInbound
)

// Numeric diagnostics have a closed set of literal fragments and at most two
// signed ints. Their longest representation fits within 128 bytes, including
// two 64-bit minimum integers. Only the session owner accesses this scratch.
type boundedEventBuffer struct{ bytes [128]byte }

type sequenceEventFormat struct {
	legacyFormat string
	prefix       string
	separator    string
	suffix       string
	twoIntegers  bool
}

var sequenceEventFormats = [...]sequenceEventFormat{
	eventSequenceTooHigh:       {"MsgSeqNum too high, expecting %v but received %v", "MsgSeqNum too high, expecting ", " but received ", "", true},
	eventSentResendRequest:     {"Sent ResendRequest FROM: %v TO: %v", "Sent ResendRequest FROM: ", " TO: ", "", true},
	eventReceivedResendRequest: {"Received ResendRequest FROM: %d TO: %d", "Received ResendRequest FROM: ", " TO: ", "", true},
	eventSentSequenceReset:     {"Sent SequenceReset TO: %v", "Sent SequenceReset TO: ", "", "", false},
	eventReceivedSequenceReset: {"Received SequenceReset FROM: %v TO: %v", "Received SequenceReset FROM: ", " TO: ", "", true},
	eventResendingMessage:      {"Resending Message: %v", "Resending Message: ", "", "", false},
	eventDrainingInbound:       {"Draining %d messages from inbound channel...", "Draining ", "", " messages from inbound channel...", false},
}

func (buffer *boundedEventBuffer) format(event sequenceEvent, first, second int) []byte {
	parts := sequenceEventFormats[event]
	length := len(parts.prefix) + intTextLength(first) + len(parts.suffix)
	if parts.twoIntegers {
		length += len(parts.separator) + intTextLength(second)
	}
	// Keep the fixed-capacity invariant explicit if a future diagnostic changes
	// its literal text. This is a programming error, never a runtime limit.
	if length > len(buffer.bytes) {
		panic("quickfix: bounded event exceeds fixed scratch capacity")
	}
	encoded := append(buffer.bytes[:0], parts.prefix...)
	encoded = strconv.AppendInt(encoded, int64(first), 10)
	if parts.twoIntegers {
		encoded = append(encoded, parts.separator...)
		encoded = strconv.AppendInt(encoded, int64(second), 10)
	}
	encoded = append(encoded, parts.suffix...)
	return encoded[:len(encoded):len(encoded)]
}

func (s *session) logSequenceEvent(event sequenceEvent, first, second int) {
	if s.boundedIn != nil && s.boundedIn.eventLog != nil {
		s.boundedIn.eventLog.OnEventBorrowed(s.boundedIn.events.format(event, first, second))
		return
	}
	// Keep format strings and typed arguments intact for legacy loggers that
	// retain them. Never expose the session scratch through that interface.
	parts := sequenceEventFormats[event]
	if parts.twoIntegers {
		s.log.OnEventf(parts.legacyFormat, first, second)
		return
	}
	s.log.OnEventf(parts.legacyFormat, first)
}

func (nullLog) OnEventBorrowed([]byte) {}
