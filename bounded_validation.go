package quickfix

import (
	"errors"

	"github.com/quickfixgo/quickfix/datadictionary"
)

// ErrValidationCapacity means the message exceeds the configured validation
// field capacity. It is an operational capacity error, not a FIX reject reason.
var ErrValidationCapacity = errors.New("quickfix: validation field capacity exceeded")

// ErrInvalidValidationLimits indicates an invalid fixed scratch capacity.
var ErrInvalidValidationLimits = errors.New("quickfix: invalid validation field capacity")

// BoundedValidator owns fixed duplicate-tag scratch for one session owner. It
// applies the same settings, validation passes and FIX rejects as NewValidator.
// Dictionaries must remain immutable. Calls must be serialized by ownership;
// concurrent callers should use separate validators or the legacy NewValidator.
type BoundedValidator struct {
	settings      ValidatorSettings
	application   *datadictionary.DataDictionary
	transport     *datadictionary.DataDictionary
	maxFields     int
	seen          validationTagSet
	requiredApp   *validationRequiredMetadata
	requiredAdmin *validationRequiredMetadata
}

// NewBoundedValidator allocates scratch once. maxFields counts all parsed
// fields, including group members, header and trailer. Construct before use.
func NewBoundedValidator(settings ValidatorSettings, application, transport *datadictionary.DataDictionary, maxFields int) (*BoundedValidator, error) {
	if maxFields <= 0 || maxFields > int(^uint(0)>>1)/1024 {
		return nil, ErrInvalidValidationLimits
	}
	validator := &BoundedValidator{settings: settings, application: application, transport: transport, maxFields: maxFields}
	validator.seen.initFixed(maxFields)
	return validator, nil
}

// Validate checks a parsed message. A non-nil first result is the usual FIX
// rejection. A non-nil second result is capacity exhaustion, detected before
// validation or scratch mutation. Neither path changes message/store/sequence
// state. The validator and message must have one owner during the call.
func (validator *BoundedValidator) Validate(message *Message) (MessageRejectError, error) {
	if len(message.fields) > validator.maxFields {
		return nil, ErrValidationCapacity
	}
	return validator.validateWithinCapacity(message), nil
}

// validateWithinCapacity is also used by a session whose MessageSlots enforce
// the same field capacity before validation. The session owns both objects.
func (validator *BoundedValidator) validateWithinCapacity(message *Message) MessageRejectError {
	validator.seen.reset()
	if !message.Header.Has(tagMsgType) {
		return RequiredTagMissing(tagMsgType)
	}
	msgType, reject := message.Header.GetBytes(tagMsgType)
	if reject != nil {
		return reject
	}
	if validator.transport == nil {
		return validateFIXWithScratch(validator.application, validator.settings, string(msgType), message, &validator.seen, validator.requiredApp)
	}
	if isAdminMessageType(msgType) {
		return validateFIXWithScratch(validator.transport, validator.settings, string(msgType), message, &validator.seen, validator.requiredAdmin)
	}
	return validateFIXTWithScratch(validator.transport, validator.application, validator.settings, string(msgType), message, &validator.seen, validator.requiredApp)
}

// sessionValidator is private because its scratch belongs to the session's
// single processing owner. NewValidator remains safe for concurrent callers.
type sessionValidator struct {
	bounded *BoundedValidator
	legacy  Validator
}

func newSessionValidator(settings ValidatorSettings, application, transport *datadictionary.DataDictionary, maxFields int) (Validator, error) {
	bounded, err := NewBoundedValidator(settings, application, transport, maxFields)
	if err != nil {
		return nil, err
	}
	if transport == nil {
		bounded.requiredApp = newValidationRequiredMetadata(application, application)
	} else {
		bounded.requiredApp = newValidationRequiredMetadata(transport, application)
		bounded.requiredAdmin = newValidationRequiredMetadata(transport, transport)
	}
	return &sessionValidator{bounded: bounded, legacy: NewValidator(settings, application, transport)}, nil
}

func (validator *sessionValidator) Validate(message *Message) MessageRejectError {
	if len(message.fields) > validator.bounded.maxFields {
		// Framed session input cannot exceed its parser's field limit. Keep
		// direct validation of legacy messages compatible with its old API.
		return validator.legacy.Validate(message)
	}
	return validator.bounded.validateWithinCapacity(message)
}
