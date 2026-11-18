package quickfix

import "context"

// SessionConnection is a handle to one connection of a dynamic acceptor session.
type SessionConnection struct {
	s         *session
	terminate context.CancelFunc
}

// LookupSessionConnection returns the current connection of a registered dynamic acceptor session.
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

// Terminate closes the connection without sending a Logout.
func (c SessionConnection) Terminate() {
	if c.terminate != nil {
		c.terminate()
	}
}

// Registered reports whether the handle's session has not retired yet.
func (c SessionConnection) Registered() bool {
	if c.s == nil {
		return false
	}
	current, ok := lookupSession(c.s.sessionID)
	return ok && current == c.s
}
