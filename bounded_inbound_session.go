package quickfix

import (
	"errors"

	"github.com/quickfixgo/quickfix/config"
)

var (
	ErrBorrowedApplicationRequired = errors.New("quickfix: bounded inbound requires BorrowedApplication")
	ErrBorrowedLogRequired         = errors.New("quickfix: bounded inbound requires BorrowedLog")
)

type boundedInboundSession struct {
	limits      ParseLimits
	ring        *inboundRing
	validator   *BoundedValidator
	application BorrowedApplication
	log         BorrowedLog
	recovery    resendState
	pending     pendingTimeout
	tooHigh     targetTooHigh
	tooLow      targetTooLow
	events      boundedEventBuffer
	eventLog    BorrowedEventLog
}

func (s *session) configureBoundedInbound(settings *SessionSettings, application Application, validation ValidatorSettings) error {
	if !settings.HasSetting(config.BoundedInbound) {
		return nil
	}
	enabled, err := settings.BoolSetting(config.BoundedInbound)
	if err != nil || !enabled {
		return err
	}
	borrowedApplication, ok := application.(BorrowedApplication)
	if !ok {
		return ErrBorrowedApplicationRequired
	}
	borrowedLog, ok := s.log.(BorrowedLog)
	if !ok {
		return ErrBorrowedLogRequired
	}
	limits := DefaultParseLimits()
	stashCapacity := 32
	for _, setting := range []struct {
		key    string
		target *int
	}{
		{config.BoundedMaxFrameBytes, &limits.MaxFrameBytes},
		{config.BoundedMaxFields, &limits.MaxFields},
		{config.BoundedMaxGroupEntries, &limits.MaxGroupEntries},
		{config.BoundedMaxGroupDepth, &limits.MaxGroupDepth},
		{config.BoundedInboundStashCapacity, &stashCapacity},
	} {
		if settings.HasSetting(setting.key) {
			value, err := settings.IntSetting(setting.key)
			if err != nil {
				return err
			}
			*setting.target = value
		}
	}
	if !settings.HasSetting(config.InChanCapacity) {
		s.InChanCapacity = 32
	}
	ring, err := newInboundRing(s.transportDataDictionary, s.appDataDictionary, limits, s.InChanCapacity, stashCapacity)
	if err != nil {
		return err
	}
	validator, err := NewBoundedValidator(validation, s.appDataDictionary, s.transportDataDictionary, limits.MaxFields)
	if err != nil {
		return err
	}
	s.boundedIn = &boundedInboundSession{limits: limits, ring: ring, validator: validator, application: borrowedApplication, log: borrowedLog}
	// Resolve the optional interface once. A hot interface assertion may lazily
	// allocate its runtime cache even after many otherwise allocation-free calls.
	s.boundedIn.eventLog, _ = s.log.(BorrowedEventLog)
	return nil
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
