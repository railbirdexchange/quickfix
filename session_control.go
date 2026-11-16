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

import "context"

// SessionConnection is a handle to the current connection of one session instance.
type SessionConnection struct {
	s         *session
	terminate context.CancelFunc
}

// LookupSessionConnection returns the current connection of a registered session.
// ok is false when the session isn't registered or has no stored connection;
// only dynamic acceptor sessions store one.
//
// The lookup never takes the session's send or resend locks, so it does not
// block behind an in-progress resend.
func LookupSessionConnection(sessionID SessionID) (SessionConnection, bool) {
	s, ok := lookupSession(sessionID)
	if !ok {
		return SessionConnection{}, false
	}
	s.lifetimeMu.Lock()
	terminate := s.connTerminate
	s.lifetimeMu.Unlock()
	if terminate == nil {
		return SessionConnection{}, false
	}
	return SessionConnection{s: s, terminate: terminate}, true
}

// Terminate closes the connection's socket and cancels its lifetime without
// sending a Logout. The session then runs OnLogout and retires as it does for
// any dropped connection. Terminate is idempotent and only affects the
// connection the handle was taken from.
func (c SessionConnection) Terminate() {
	if c.terminate != nil {
		c.terminate()
	}
}

// Registered reports whether the handle's session instance still holds its
// registry slot, i.e. it has not retired yet.
func (c SessionConnection) Registered() bool {
	if c.s == nil {
		return false
	}
	current, ok := lookupSession(c.s.sessionID)
	return ok && current == c.s
}
