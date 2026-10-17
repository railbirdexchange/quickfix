package quickfix

import (
	"bytes"

	"github.com/quickfixgo/quickfix/config"
)

type boundedInboundSession struct {
	limits   ParseLimits
	ring     *inboundRing
	parsed   *MessageSlot
	log      BorrowedLog
	recovery resendState
	pending  pendingTimeout
	tooHigh  targetTooHigh
	tooLow   targetTooLow
	events   boundedEventBuffer
	eventLog BorrowedEventLog
}

func (s *session) configureBoundedInbound(settings *SessionSettings) error {
	limits := DefaultParseLimits()
	stashCapacity := 32
	queueAllowance := min(s.InChanCapacity, 32)
	for _, setting := range []struct {
		key    string
		target *int
	}{
		{config.BoundedMaxFrameBytes, &limits.MaxFrameBytes},
		{config.BoundedMaxFields, &limits.MaxFields},
		{config.BoundedMaxGroupEntries, &limits.MaxGroupEntries},
		{config.BoundedMaxGroupDepth, &limits.MaxGroupDepth},
		{config.BoundedInboundStashCapacity, &stashCapacity},
		{config.InboundFrameQueueCapacity, &queueAllowance},
	} {
		if settings.HasSetting(setting.key) {
			value, err := settings.IntSetting(setting.key)
			if err != nil {
				return err
			}
			*setting.target = value
		}
	}
	if queueAllowance < 0 {
		return ErrInvalidInboundLimits
	}
	queueAllowance = min(queueAllowance, s.InChanCapacity)
	ring, err := newInboundRing(limits, queueAllowance, stashCapacity)
	if err != nil {
		return err
	}
	parsed, err := NewMessageSlot(s.transportDataDictionary, s.appDataDictionary, limits)
	if err != nil {
		return err
	}
	parsed.enableScopedCallbacks()
	borrowedLog, _ := s.log.(BorrowedLog)
	s.boundedIn = &boundedInboundSession{limits: limits, ring: ring, parsed: parsed, log: borrowedLog}
	// Resolve borrowed event logging before the session begins processing.
	s.boundedIn.eventLog, _ = s.log.(BorrowedEventLog)
	return nil
}

func (s *session) parseInboundSlot(slot *inboundSlot) (*Message, error) {
	parsed := s.boundedIn.parsed
	// A previous callback may have used allocating setters. Restore the fixed
	// scratch before parsing the next frame, including retained-frame replay.
	parsed.Reset()
	if err := parsed.Parse(slot.frame); err != nil {
		return nil, err
	}
	message := parsed.Message()
	if !applicationBorrowsMessages(s.application) {
		// The ordinary Application contract permits retention. Materialize one
		// owned frame and message before state dispatch, so callback mutations
		// are also the values used by subsequent protocol handling.
		message = materializeInboundMessage(message)
	}
	message.inbound = slot
	message.ReceiveTime = slot.receiveTime
	return message, nil
}

func (s *session) logIncomingFrame(frame []byte) {
	if s.boundedIn.log != nil {
		s.boundedIn.log.OnIncomingBorrowed(frame)
		return
	}
	// Arbitrary loggers retain their existing byte ownership. This allocating
	// adapter is outside the zero-allocation logger contract.
	s.log.OnIncoming(bytes.Clone(frame))
}

func isResendRecovery(state sessionState) bool {
	_, ok := boundedResendState(state)
	return ok
}

func boundedResendState(state sessionState) (resendState, bool) {
	switch state := state.(type) {
	case resendState:
		return state, true
	case *resendState:
		return *state, true
	case pendingTimeout:
		return boundedResendState(state.sessionState)
	case *pendingTimeout:
		return boundedResendState(state.sessionState)
	}
	return resendState{}, false
}

func isInboundCapacityError(err error) bool {
	return err == ErrFrameCapacity || err == ErrFieldCapacity || err == ErrGroupCapacity || err == ErrGroupDepth || err == ErrInboundStashCapacity
}

func (s *session) failBoundedInbound(err error) sessionState {
	if isInboundCapacityError(err) {
		// A configured capacity failure must not reset or advance store state.
		// The peer may reconnect after the operator adjusts the limits.
		s.preserveOnDisconnect = true
	}
	return handleStateError(s, err)
}

func logMessageArgument(message *Message) interface{} {
	if message.inbound != nil {
		// Event loggers retain their legacy argument lifetime. This diagnostic
		// path deliberately copies, and is outside the valid-message guarantee.
		return string(message.inbound.frame)
	}
	return message
}
