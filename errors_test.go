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
	"fmt"
	"strings"
	"testing"
)

func TestNewMessageRejectError(t *testing.T) {
	var (
		expectedErrorString          = "Custom error"
		expectedRejectReason         = 5
		expectedRefTagID         Tag = 44
		expectedIsBusinessReject     = false
	)
	msgRej := NewMessageRejectError(expectedErrorString, expectedRejectReason, &expectedRefTagID)

	if strings.Compare(msgRej.Error(), expectedErrorString) != 0 {
		t.Errorf("expected: %s, got: %s\n", expectedErrorString, msgRej.Error())
	}
	if msgRej.RejectReason() != expectedRejectReason {
		t.Errorf("expected: %d, got: %d\n", expectedRejectReason, msgRej.RejectReason())
	}
	if refTagID, present := msgRej.RefTagID(); !present || refTagID != expectedRefTagID {
		t.Errorf("expected: %d, got: %d (present: %v)\n", expectedRefTagID, refTagID, present)
	}
	if msgRej.IsBusinessReject() != expectedIsBusinessReject {
		t.Error("Expected IsBusinessReject to be false\n")
	}
}

func TestNewBusinessMessageRejectError(t *testing.T) {
	var (
		expectedErrorString          = "Custom error"
		expectedRejectReason         = 5
		expectedRefTagID         Tag = 44
		expectedIsBusinessReject     = true
	)
	msgRej := NewBusinessMessageRejectError(expectedErrorString, expectedRejectReason, &expectedRefTagID)

	if strings.Compare(msgRej.Error(), expectedErrorString) != 0 {
		t.Errorf("expected: %s, got: %s\n", expectedErrorString, msgRej.Error())
	}
	if msgRej.RejectReason() != expectedRejectReason {
		t.Errorf("expected: %d, got: %d\n", expectedRejectReason, msgRej.RejectReason())
	}
	if refTagID, present := msgRej.RefTagID(); !present || refTagID != expectedRefTagID {
		t.Errorf("expected: %d, got: %d (present: %v)\n", expectedRefTagID, refTagID, present)
	}
	if msgRej.IsBusinessReject() != expectedIsBusinessReject {
		t.Error("Expected IsBusinessReject to be true\n")
	}
}

func TestNewBusinessMessageRejectErrorWithRefID(t *testing.T) {
	var (
		expectedErrorString             = "Custom error"
		expectedRejectReason            = 5
		expectedbusinessRejectRefID     = "1"
		expectedRefTagID            Tag = 44
		expectedIsBusinessReject        = true
	)
	msgRej := NewBusinessMessageRejectErrorWithRefID(expectedErrorString, expectedRejectReason, expectedbusinessRejectRefID, &expectedRefTagID)

	if strings.Compare(msgRej.Error(), expectedErrorString) != 0 {
		t.Errorf("expected: %s, got: %s\n", expectedErrorString, msgRej.Error())
	}
	if msgRej.RejectReason() != expectedRejectReason {
		t.Errorf("expected: %d, got: %d\n", expectedRejectReason, msgRej.RejectReason())
	}
	if strings.Compare(msgRej.BusinessRejectRefID(), expectedbusinessRejectRefID) != 0 {
		t.Errorf("expected: %s, got: %s\n", expectedbusinessRejectRefID, msgRej.BusinessRejectRefID())
	}
	if refTagID, present := msgRej.RefTagID(); !present || refTagID != expectedRefTagID {
		t.Errorf("expected: %d, got: %d (present: %v)\n", expectedRefTagID, refTagID, present)
	}
	if msgRej.IsBusinessReject() != expectedIsBusinessReject {
		t.Error("Expected IsBusinessReject to be true\n")
	}
}

func TestIncorrectDataFormatForValue(t *testing.T) {
	var (
		expectedErrorString          = "Incorrect data format for value"
		expectedRejectReason         = 6
		expectedRefTagID         Tag = 44
		expectedIsBusinessReject     = false
	)
	msgRej := IncorrectDataFormatForValue(expectedRefTagID)

	if strings.Compare(msgRej.Error(), expectedErrorString) != 0 {
		t.Errorf("expected: %s, got: %s\n", expectedErrorString, msgRej.Error())
	}
	if msgRej.RejectReason() != expectedRejectReason {
		t.Errorf("expected: %d, got: %d\n", expectedRejectReason, msgRej.RejectReason())
	}
	if refTagID, present := msgRej.RefTagID(); !present || refTagID != expectedRefTagID {
		t.Errorf("expected: %d, got: %d (present: %v)\n", expectedRefTagID, refTagID, present)
	}
	if msgRej.IsBusinessReject() != expectedIsBusinessReject {
		t.Error("Expected IsBusinessReject to be false\n")
	}
}

func TestValueIsIncorrect(t *testing.T) {
	var (
		expectedErrorString          = "Value is incorrect (out of range) for this tag"
		expectedRejectReason         = 5
		expectedRefTagID         Tag = 44
		expectedIsBusinessReject     = false
	)
	msgRej := ValueIsIncorrect(expectedRefTagID)
	if refTagID, present := msgRej.RefTagID(); !present || refTagID != expectedRefTagID {
		t.Errorf("expected: %d, got: %d (present: %v)\n", expectedRefTagID, refTagID, present)
	}
	if strings.Compare(msgRej.Error(), expectedErrorString) != 0 {
		t.Errorf("expected: %s, got: %s\n", expectedErrorString, msgRej.Error())
	}
	if msgRej.RejectReason() != expectedRejectReason {
		t.Errorf("expected: %d, got: %d\n", expectedRejectReason, msgRej.RejectReason())
	}
	if msgRej.IsBusinessReject() != expectedIsBusinessReject {
		t.Error("Expected IsBusinessReject to be false\n")
	}
}

func TestConditionallyRequiredFieldMissing(t *testing.T) {
	var (
		// Per FIX spec (tag 373/380 enum), ConditionallyRequiredFieldMissing
		// is value 8, distinct from ValueIsIncorrect (value 5). See issue #721.
		expectedRejectReason         = 8
		expectedRefTagID         Tag = 44
		expectedErrorString          = fmt.Sprintf("Conditionally Required Field Missing (%d)", expectedRefTagID)
		expectedIsBusinessReject     = true
	)
	msgRej := ConditionallyRequiredFieldMissing(expectedRefTagID)
	if refTagID, present := msgRej.RefTagID(); !present || refTagID != expectedRefTagID {
		t.Errorf("expected: %d, got: %d (present: %v)\n", expectedRefTagID, refTagID, present)
	}
	if strings.Compare(msgRej.Error(), expectedErrorString) != 0 {
		t.Errorf("expected: %s, got: %s\n", expectedErrorString, msgRej.Error())
	}
	if msgRej.RejectReason() != expectedRejectReason {
		t.Errorf("expected: %d, got: %d\n", expectedRejectReason, msgRej.RejectReason())
	}
	if msgRej.IsBusinessReject() != expectedIsBusinessReject {
		t.Error("Expected IsBusinessReject to be true\n")
	}
	// Regression guard: the two reject reasons must not collide.
	if msgRej.RejectReason() == ValueIsIncorrect(expectedRefTagID).RejectReason() {
		t.Errorf("ConditionallyRequiredFieldMissing and ValueIsIncorrect must not share a reject reason value")
	}
}

func TestInvalidMessageType(t *testing.T) {
	var (
		expectedErrorString      = "Invalid MsgType"
		expectedRejectReason     = 11
		expectedIsBusinessReject = false
	)
	msgRej := InvalidMessageType()

	if strings.Compare(msgRej.Error(), expectedErrorString) != 0 {
		t.Errorf("expected: %s, got: %s\n", expectedErrorString, msgRej.Error())
	}
	if msgRej.RejectReason() != expectedRejectReason {
		t.Errorf("expected: %d, got: %d\n", expectedRejectReason, msgRej.RejectReason())
	}
	if refTagID, present := msgRej.RefTagID(); present {
		t.Errorf("expected: absent, got: %d\n", refTagID)
	}
	if msgRej.IsBusinessReject() != expectedIsBusinessReject {
		t.Error("Expected IsBusinessReject to be false\n")
	}
}

func TestUnsupportedMessageType(t *testing.T) {
	var (
		expectedRejectReason     = 3
		expectedErrorString      = "Unsupported Message Type"
		expectedIsBusinessReject = true
	)
	msgRej := UnsupportedMessageType()
	if refTagID, present := msgRej.RefTagID(); present {
		t.Errorf("expected: absent, got: %d\n", refTagID)
	}
	if strings.Compare(msgRej.Error(), expectedErrorString) != 0 {
		t.Errorf("expected: %s, got: %s\n", expectedErrorString, msgRej.Error())
	}
	if msgRej.RejectReason() != expectedRejectReason {
		t.Errorf("expected: %d, got: %d\n", expectedRejectReason, msgRej.RejectReason())
	}
	if msgRej.IsBusinessReject() != expectedIsBusinessReject {
		t.Error("Expected IsBusinessReject to be true\n")
	}
}

func TestTagNotDefinedForThisMessageType(t *testing.T) {
	var (
		expectedErrorString          = "Tag not defined for this message type"
		expectedRejectReason         = 2
		expectedRefTagID         Tag = 44
		expectedIsBusinessReject     = false
	)
	msgRej := TagNotDefinedForThisMessageType(expectedRefTagID)

	if strings.Compare(msgRej.Error(), expectedErrorString) != 0 {
		t.Errorf("expected: %s, got: %s\n", expectedErrorString, msgRej.Error())
	}
	if msgRej.RejectReason() != expectedRejectReason {
		t.Errorf("expected: %d, got: %d\n", expectedRejectReason, msgRej.RejectReason())
	}
	if refTagID, present := msgRej.RefTagID(); !present || refTagID != expectedRefTagID {
		t.Errorf("expected: %d, got: %d (present: %v)\n", expectedRefTagID, refTagID, present)
	}
	if msgRej.IsBusinessReject() != expectedIsBusinessReject {
		t.Error("Expected IsBusinessReject to be false\n")
	}
}

func TestRequiredTagMissing(t *testing.T) {
	var (
		expectedErrorString          = "Required tag missing"
		expectedRejectReason         = 1
		expectedRefTagID         Tag = 44
		expectedIsBusinessReject     = false
	)
	msgRej := RequiredTagMissing(expectedRefTagID)

	if strings.Compare(msgRej.Error(), expectedErrorString) != 0 {
		t.Errorf("expected: %s, got: %s\n", expectedErrorString, msgRej.Error())
	}
	if msgRej.RejectReason() != expectedRejectReason {
		t.Errorf("expected: %d, got: %d\n", expectedRejectReason, msgRej.RejectReason())
	}
	if refTagID, present := msgRej.RefTagID(); !present || refTagID != expectedRefTagID {
		t.Errorf("expected: %d, got: %d (present: %v)\n", expectedRefTagID, refTagID, present)
	}
	if msgRej.IsBusinessReject() != expectedIsBusinessReject {
		t.Error("Expected IsBusinessReject to be false\n")
	}
}

func TestTagSpecifiedWithoutAValue(t *testing.T) {
	var (
		expectedErrorString          = "Tag specified without a value"
		expectedRejectReason         = 4
		expectedRefTagID         Tag = 44
		expectedIsBusinessReject     = false
	)
	msgRej := TagSpecifiedWithoutAValue(expectedRefTagID)

	if strings.Compare(msgRej.Error(), expectedErrorString) != 0 {
		t.Errorf("expected: %s, got: %s\n", expectedErrorString, msgRej.Error())
	}
	if msgRej.RejectReason() != expectedRejectReason {
		t.Errorf("expected: %d, got: %d\n", expectedRejectReason, msgRej.RejectReason())
	}
	if refTagID, present := msgRej.RefTagID(); !present || refTagID != expectedRefTagID {
		t.Errorf("expected: %d, got: %d (present: %v)\n", expectedRefTagID, refTagID, present)
	}
	if msgRej.IsBusinessReject() != expectedIsBusinessReject {
		t.Error("Expected IsBusinessReject to be false\n")
	}
}

func TestInvalidTagNumber(t *testing.T) {
	var (
		expectedErrorString          = "Invalid tag number"
		expectedRejectReason         = 0
		expectedRefTagID         Tag = 44
		expectedIsBusinessReject     = false
	)
	msgRej := InvalidTagNumber(expectedRefTagID)

	if strings.Compare(msgRej.Error(), expectedErrorString) != 0 {
		t.Errorf("expected: %s, got: %s\n", expectedErrorString, msgRej.Error())
	}
	if msgRej.RejectReason() != expectedRejectReason {
		t.Errorf("expected: %d, got: %d\n", expectedRejectReason, msgRej.RejectReason())
	}
	if refTagID, present := msgRej.RefTagID(); !present || refTagID != expectedRefTagID {
		t.Errorf("expected: %d, got: %d (present: %v)\n", expectedRefTagID, refTagID, present)
	}
	if msgRej.IsBusinessReject() != expectedIsBusinessReject {
		t.Error("Expected IsBusinessReject to be false\n")
	}
}

func TestMessageRejectConstructorsSnapshotReferenceTag(t *testing.T) {
	for _, constructor := range []struct {
		name string
		make func(*Tag) MessageRejectError
	}{
		{"session", func(tag *Tag) MessageRejectError { return NewMessageRejectError("custom", 5, tag) }},
		{"business", func(tag *Tag) MessageRejectError { return NewBusinessMessageRejectError("custom", 5, tag) }},
		{"business_ref", func(tag *Tag) MessageRejectError {
			return NewBusinessMessageRejectErrorWithRefID("custom", 5, "ref", tag)
		}},
	} {
		t.Run(constructor.name, func(t *testing.T) {
			if tag, present := constructor.make(nil).RefTagID(); present || tag != 0 {
				t.Fatalf("nil input returned (%d, %v), want (0, false)", tag, present)
			}
			for _, want := range []Tag{0, 44, -1} {
				input := want
				reject := constructor.make(&input)
				input = 999
				value, present := reject.RefTagID()
				if !present || value != want {
					t.Fatalf("constructor retained input storage: got (%d, %v), want (%d, true)", value, present, want)
				}
				value = 1000
				if got, present := reject.RefTagID(); !present || got != want || got == value {
					t.Fatalf("returned reference value changed error: got (%d, %v), want (%d, true)", got, present, want)
				}
			}
		})
	}
}

func TestRejectLogonHasNoReferenceTag(t *testing.T) {
	if tag, present := (RejectLogon{Text: "no session"}).RefTagID(); tag != 0 || present {
		t.Fatalf("logon reject reference = (%d, %v), want (0, false)", tag, present)
	}
}
