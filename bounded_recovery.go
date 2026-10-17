package quickfix

// Recovery state and sequence rejects belong to the session goroutine. Returning
// pointers to these fixed cells avoids boxing a fresh value for each message.
// Legacy sessions keep their existing value states and reject values.
func (s *session) recoveryState(state resendState) sessionState {
	if s.boundedIn == nil {
		return state
	}
	s.boundedIn.recovery = state
	return &s.boundedIn.recovery
}

func (s *session) timeoutState(state sessionState) sessionState {
	if s.boundedIn == nil {
		return pendingTimeout{state}
	}
	// Reusing the wrapper must never make it its own embedded state.
	if state == &s.boundedIn.pending {
		return state
	}
	s.boundedIn.pending.sessionState = state
	return &s.boundedIn.pending
}

func (s *session) sequenceTooHigh(received, expected int) MessageRejectError {
	reject := targetTooHigh{ReceivedTarget: received, ExpectedTarget: expected}
	if s.boundedIn == nil {
		return reject
	}
	s.boundedIn.tooHigh = reject
	return &s.boundedIn.tooHigh
}

func (s *session) sequenceTooLow(received, expected int) MessageRejectError {
	reject := targetTooLow{ReceivedTarget: received, ExpectedTarget: expected}
	if s.boundedIn == nil {
		return reject
	}
	s.boundedIn.tooLow = reject
	return &s.boundedIn.tooLow
}
