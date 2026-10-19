// Copyright (c) quickfixengine.org  All rights reserved.
//
// This file may be distributed under the terms of the quickfixengine.org
// license as defined by quickfixengine.org and appearing in the file
// LICENSE included in the packaging of this file.
//
// This file is provided AS IS with NO WARRANTY OF ANY KIND, INCLUDING
// THE WARRANTY OF DESIGN, MERCHANTABILITY AND FITNESS FOR A
// PARTICULAR PURPOSE.
//
// See http://www.quickfixengine.org/LICENSE for licensing information.
//
// Contact ask@quickfixengine.org if any conditions of this licensing
// are not clear to you.

package quickfix

import (
	"errors"
	"fmt"
	"strconv"
)

// ErrDoNotSend is a convenience error to indicate a DoNotSend in ToApp.
var ErrDoNotSend = errors.New("Do Not Send")

// rejectReason enum values.
const (
	rejectReasonInvalidTagNumber                          = 0
	rejectReasonRequiredTagMissing                        = 1
	rejectReasonTagNotDefinedForThisMessageType           = 2
	rejectReasonUnsupportedMessageType                    = 3
	rejectReasonTagSpecifiedWithoutAValue                 = 4
	rejectReasonValueIsIncorrect                          = 5
	rejectReasonConditionallyRequiredFieldMissing         = 8
	rejectReasonIncorrectDataFormatForValue               = 6
	rejectReasonCompIDProblem                             = 9
	rejectReasonSendingTimeAccuracyProblem                = 10
	rejectReasonInvalidMsgType                            = 11
	rejectReasonTagAppearsMoreThanOnce                    = 13
	rejectReasonTagSpecifiedOutOfRequiredOrder            = 14
	rejectReasonRepeatingGroupFieldsOutOfOrder            = 15
	rejectReasonIncorrectNumInGroupCountForRepeatingGroup = 16
)

// MessageRejectError is a type of error that can correlate to a message reject.
type MessageRejectError interface {
	error

	// RejectReason, tag 373 for session rejects, tag 380 for business rejects.
	RejectReason() int
	BusinessRejectRefID() string
	// RefTagID identifies the field associated with the rejection, when present.
	// The returned value does not expose mutable storage owned by the error.
	RefTagID() (Tag, bool)
	IsBusinessReject() bool
}

// RejectLogon indicates the application is rejecting permission to logon. Implements MessageRejectError.
type RejectLogon struct {
	Text string
}

func (e RejectLogon) Error() string { return e.Text }

// RefTagID implements MessageRejectError.
func (RejectLogon) RefTagID() (Tag, bool) { return 0, false }

// RejectReason implements MessageRejectError.
func (RejectLogon) RejectReason() int { return 0 }

// BusinessRejectRefID implements MessageRejectError.
func (RejectLogon) BusinessRejectRefID() string { return "" }

// IsBusinessReject implements MessageRejectError.
func (RejectLogon) IsBusinessReject() bool { return false }

type messageRejectError struct {
	rejectReason        int
	text                string
	businessRejectRefID string
	refTagID            Tag
	hasRefTagID         bool
	isBusinessReject    bool
}

func (e messageRejectError) Error() string               { return e.text }
func (e messageRejectError) RefTagID() (Tag, bool)       { return e.refTagID, e.hasRefTagID }
func (e messageRejectError) RejectReason() int           { return e.rejectReason }
func (e messageRejectError) BusinessRejectRefID() string { return e.businessRejectRefID }
func (e messageRejectError) IsBusinessReject() bool      { return e.isBusinessReject }

// NewMessageRejectError returns a MessageRejectError with the given error message, reject reason, and optional reftagid.
func NewMessageRejectError(err string, rejectReason int, refTagID *Tag) MessageRejectError {
	return newMessageRejectError(err, rejectReason, refTagID)
}

func newMessageRejectError(text string, rejectReason int, refTagID *Tag) messageRejectError {
	reject := messageRejectError{text: text, rejectReason: rejectReason}
	if refTagID != nil {
		reject.refTagID, reject.hasRefTagID = *refTagID, true
	}
	return reject
}

// NewBusinessMessageRejectError returns a MessageRejectError with the given error mesage, reject reason, and optional reftagid.
// Reject is treated as a business level reject.
func NewBusinessMessageRejectError(err string, rejectReason int, refTagID *Tag) MessageRejectError {
	reject := newMessageRejectError(err, rejectReason, refTagID)
	reject.isBusinessReject = true
	return reject
}

// NewBusinessMessageRejectErrorWithRefID returns a MessageRejectError with the given error mesage, reject reason, refID, and optional reftagid.
// Reject is treated as a business level reject.
func NewBusinessMessageRejectErrorWithRefID(err string, rejectReason int, businessRejectRefID string, refTagID *Tag) MessageRejectError {
	reject := newMessageRejectError(err, rejectReason, refTagID)
	reject.businessRejectRefID, reject.isBusinessReject = businessRejectRefID, true
	return reject
}

// IncorrectDataFormatForValue returns an error indicating a field that cannot be parsed as the type required.
func IncorrectDataFormatForValue(tag Tag) MessageRejectError {
	return NewMessageRejectError("Incorrect data format for value", rejectReasonIncorrectDataFormatForValue, &tag)
}

// repeatingGroupFieldsOutOfOrder returns an error indicating a problem parsing repeating groups fields.
func repeatingGroupFieldsOutOfOrder(tag Tag, reason string) MessageRejectError {
	if reason != "" {
		reason = fmt.Sprintf("Repeating group fields out of order (%s)", reason)
	} else {
		reason = "Repeating group fields out of order"
	}
	return NewMessageRejectError(reason, rejectReasonRepeatingGroupFieldsOutOfOrder, &tag)
}

// ValueIsIncorrect returns an error indicating a field with value that is not valid.
func ValueIsIncorrect(tag Tag) MessageRejectError {
	return NewMessageRejectError("Value is incorrect (out of range) for this tag", rejectReasonValueIsIncorrect, &tag)
}

// ConditionallyRequiredFieldMissing indicates that the requested field could not be found in the FIX message.
func ConditionallyRequiredFieldMissing(tag Tag) MessageRejectError {
	reject := newMissingFieldReject(tag)
	return &reject
}

// Missing-field rejects are immutable and can be shared for configured tags.
// Prepare their text once so rendering and retaining the error need no new work.
type missingFieldReject struct {
	tag  Tag
	text string
}

func newMissingFieldReject(tag Tag) missingFieldReject {
	const prefix = "Conditionally Required Field Missing ("
	// A signed 64-bit tag needs at most 20 bytes, including the sign. This also
	// covers 32-bit platforms without growing the local formatting buffer.
	var buffer [len(prefix) + 20 + 1]byte
	text := append(buffer[:0], prefix...)
	text = strconv.AppendInt(text, int64(tag), 10)
	text = append(text, ')')
	return missingFieldReject{tag: tag, text: string(text)}
}

func (reject *missingFieldReject) Error() string         { return reject.text }
func (reject *missingFieldReject) RefTagID() (Tag, bool) { return reject.tag, true }
func (*missingFieldReject) RejectReason() int {
	return rejectReasonConditionallyRequiredFieldMissing
}
func (*missingFieldReject) BusinessRejectRefID() string { return "" }
func (*missingFieldReject) IsBusinessReject() bool      { return true }

// valueIsIncorrectNoTag returns an error indicating a field with value that is not valid.
// FIXME: to be compliant with legacy tests, for certain value issues, do not include reftag? (11c_NewSeqNoLess).
func valueIsIncorrectNoTag() MessageRejectError {
	return NewMessageRejectError("Value is incorrect (out of range) for this tag", rejectReasonValueIsIncorrect, nil)
}

// InvalidMessageType returns an error to indicate an invalid message type.
func InvalidMessageType() MessageRejectError {
	return NewMessageRejectError("Invalid MsgType", rejectReasonInvalidMsgType, nil)
}

// UnsupportedMessageType returns an error to indicate an unhandled message.
func UnsupportedMessageType() MessageRejectError {
	return NewBusinessMessageRejectError("Unsupported Message Type", rejectReasonUnsupportedMessageType, nil)
}

// TagNotDefinedForThisMessageType returns an error for an invalid tag appearing in a message.
func TagNotDefinedForThisMessageType(tag Tag) MessageRejectError {
	return NewMessageRejectError("Tag not defined for this message type", rejectReasonTagNotDefinedForThisMessageType, &tag)
}

// tagAppearsMoreThanOnce return an error for multiple tags in a message not detected as a repeating group.
func tagAppearsMoreThanOnce(tag Tag) MessageRejectError {
	return NewMessageRejectError("Tag appears more than once", rejectReasonTagAppearsMoreThanOnce, &tag)
}

// RequiredTagMissing returns a validation error when a required field cannot be found in a message.
func RequiredTagMissing(tag Tag) MessageRejectError {
	return NewMessageRejectError("Required tag missing", rejectReasonRequiredTagMissing, &tag)
}

// incorrectNumInGroupCountForRepeatingGroup returns a validation error when the num in group value for a group does not match actual group size.
func incorrectNumInGroupCountForRepeatingGroup(tag Tag) MessageRejectError {
	return NewMessageRejectError("Incorrect NumInGroup count for repeating group", rejectReasonIncorrectNumInGroupCountForRepeatingGroup, &tag)
}

// tagSpecifiedOutOfRequiredOrder returns validation error when the group order does not match the spec.
func tagSpecifiedOutOfRequiredOrder(tag Tag) MessageRejectError {
	return NewMessageRejectError("Tag specified out of required order", rejectReasonTagSpecifiedOutOfRequiredOrder, &tag)
}

// TagSpecifiedWithoutAValue returns a validation error for when a field has no value.
func TagSpecifiedWithoutAValue(tag Tag) MessageRejectError {
	return NewMessageRejectError("Tag specified without a value", rejectReasonTagSpecifiedWithoutAValue, &tag)
}

// InvalidTagNumber returns a validation error for messages with invalid tags.
func InvalidTagNumber(tag Tag) MessageRejectError {
	return NewMessageRejectError("Invalid tag number", rejectReasonInvalidTagNumber, &tag)
}

// compIDProblem creates a reject for msg where msg has invalid comp id values.
func compIDProblem() MessageRejectError {
	return NewMessageRejectError("CompID problem", rejectReasonCompIDProblem, nil)
}

// sendingTimeAccuracyProblem creates a reject for a msg with stale or invalid sending time.
func sendingTimeAccuracyProblem() MessageRejectError {
	return NewMessageRejectError("SendingTime accuracy problem", rejectReasonSendingTimeAccuracyProblem, nil)
}
