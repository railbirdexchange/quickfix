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

import "strings"

// SessionID is a unique identifier of a Session.
type SessionID struct {
	BeginString, TargetCompID, TargetSubID, TargetLocationID, SenderCompID, SenderSubID, SenderLocationID, Qualifier string
}

// IsFIXT returns true if the SessionID has a FIXT BeginString.
func (s SessionID) IsFIXT() bool {
	return s.BeginString == BeginStringFIXT11
}

func appendOptional(b *strings.Builder, delim, v string) {
	if len(v) == 0 {
		return
	}

	b.WriteString(delim)
	b.WriteString(v)
}

func (s SessionID) String() string {
	if names := registeredSessionNames.Load(); names != nil {
		if name, ok := (*names)[s]; ok {
			return name
		}
	}
	return s.format()
}

// format owns the returned string. Registered sessions prepare it once; IDs
// outside the registry retain the same formatting and independent lifetime.
func (s SessionID) format() string {
	size := len(s.BeginString) + len(s.SenderCompID) + len(s.TargetCompID) + 3
	for _, optional := range [...]string{s.SenderSubID, s.SenderLocationID, s.TargetSubID, s.TargetLocationID, s.Qualifier} {
		if optional != "" {
			size += 1 + len(optional)
		}
	}
	var b strings.Builder
	b.Grow(size)
	b.WriteString(s.BeginString)
	b.WriteString(":")
	b.WriteString(s.SenderCompID)

	appendOptional(&b, "/", s.SenderSubID)
	appendOptional(&b, "/", s.SenderLocationID)

	b.WriteString("->")
	b.WriteString(s.TargetCompID)

	appendOptional(&b, "/", s.TargetSubID)
	appendOptional(&b, "/", s.TargetLocationID)

	appendOptional(&b, ":", s.Qualifier)
	return b.String()
}
