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
	"sync"

	"github.com/puzpuzpuz/xsync/v4"
)

// sessionsLock coordinates lifecycle changes, identity reservations and store
// operations that must finish before retirement. Routing and send lookups do
// not acquire it.
var sessionsLock sync.RWMutex

// Publish the immutable name with its session so readers see both together.
// Each lifecycle change replaces only its entry; it does not rebuild a second
// map containing the names of every registered session.
type registeredSession struct {
	session *session
	name    string
}

var sessions = xsync.NewMap[SessionID, registeredSession]()

// Reservations cover construction and the complete dynamic-session lifetime.
// Public unregister only removes routing; it cannot permit overlapping stores.
// A failed Close leaves this small identity-only claim quarantined until restart.
// The map and claims are protected by sessionsLock; no session is retained here.
type sessionReservation struct{ dynamic bool }

var sessionReservations = make(map[SessionID]*sessionReservation)

var errDuplicateSessionID = errors.New("Duplicate SessionID")
var errUnknownSession = errors.New("Unknown session")

// Messagable is a Message or something that can be converted to a Message.
type Messagable interface {
	ToMessage() *Message
}

// Send determines the session to send Messagable using header fields BeginString, TargetCompID, SenderCompID.
func Send(m Messagable) (err error) {
	msg := m.ToMessage()
	var beginString FIXString
	if err := msg.Header.GetField(tagBeginString, &beginString); err != nil {
		return err
	}

	var targetCompID FIXString
	if err := msg.Header.GetField(tagTargetCompID, &targetCompID); err != nil {
		return err
	}

	var senderCompID FIXString
	if err := msg.Header.GetField(tagSenderCompID, &senderCompID); err != nil {
		return err
	}

	sessionID := SessionID{BeginString: string(beginString), TargetCompID: string(targetCompID), SenderCompID: string(senderCompID)}

	return SendToTarget(msg, sessionID)
}

// SendToTarget sends a message based on the sessionID. Convenient for use in FromApp since it provides a session ID for incoming messages.
// Preparation, application callbacks and persistence finish synchronously; the
// writer receives independent encoded bytes. The caller retains ownership of m
// and may mutate it after return. Engine-managed header/trailer byte views may
// change on a later send of the same Message; CopyInto and owned string getters
// detach values that must survive that reuse. Successful return means queued,
// not that the socket write has completed.
func SendToTarget(m Messagable, sessionID SessionID) error {
	msg := m.ToMessage()
	session, ok := lookupSession(sessionID)
	if !ok {
		return errUnknownSession
	}

	return session.queueForSend(msg)
}

// ResetSession resets session's sequence numbers.
func ResetSession(sessionID SessionID) error {
	session, ok := lookupSession(sessionID)
	if !ok {
		return errUnknownSession
	}
	session.log.OnEvent("Session reset")
	if err := session.State.ShutdownNow(session); isOutboundCapacityError(err) {
		return err
	}
	if err := session.dropAndReset(); err != nil {
		session.logError(err)
		return err
	}

	return nil
}

// UnregisterSession removes a session from the set of known sessions.
func UnregisterSession(sessionID SessionID) error {
	sessionsLock.Lock()
	defer sessionsLock.Unlock()

	if _, ok := sessions.Load(sessionID); ok {
		sessions.Delete(sessionID)
		return nil
	}

	return errUnknownSession
}

// SetNextTargetMsgSeqNum set the next expected target message sequence number for the session matching the session id.
func SetNextTargetMsgSeqNum(sessionID SessionID, seqNum int) error {
	sessionsLock.RLock()
	defer sessionsLock.RUnlock()
	session, ok := lookupSession(sessionID)
	if !ok {
		return errUnknownSession
	}
	return session.store.SetNextTargetMsgSeqNum(seqNum)
}

// SetNextSenderMsgSeqNum sets the next outgoing message sequence number for the session matching the session id.
// It waits for pending send preparation and resend iteration. Call it outside
// application send callbacks, which already execute under sender ownership.
func SetNextSenderMsgSeqNum(sessionID SessionID, seqNum int) error {
	session, ok := lookupSession(sessionID)
	if !ok {
		return errUnknownSession
	}
	session.resendMutex.Lock()
	defer session.resendMutex.Unlock()
	session.sendMutex.Lock()
	defer session.unlockSend()
	if session.retired {
		return errUnknownSession
	}
	return session.store.SetNextSenderMsgSeqNum(seqNum)
}

// GetExpectedSenderNum retrieves the expected sender sequence number for the session matching the session id.
func GetExpectedSenderNum(sessionID SessionID) (int, error) {
	sessionsLock.RLock()
	defer sessionsLock.RUnlock()
	session, ok := lookupSession(sessionID)
	if !ok {
		return 0, errUnknownSession
	}
	return session.store.NextSenderMsgSeqNum(), nil
}

// GetExpectedTargetNum retrieves the next target sequence number for the session matching the session id.
func GetExpectedTargetNum(sessionID SessionID) (int, error) {
	sessionsLock.RLock()
	defer sessionsLock.RUnlock()
	session, ok := lookupSession(sessionID)
	if !ok {
		return 0, errUnknownSession
	}
	return session.store.NextTargetMsgSeqNum(), nil
}

// GetMessageStore returns the MessageStore interface for session matching the session id.
func GetMessageStore(sessionID SessionID) (MessageStore, error) {
	session, ok := lookupSession(sessionID)
	if !ok {
		return nil, errUnknownSession
	}
	return session.store, nil
}

// GetLog returns the Log interface for session matching the session id.
func GetLog(sessionID SessionID) (Log, error) {
	session, ok := lookupSession(sessionID)
	if !ok {
		return nil, errUnknownSession
	}
	return session.log, nil
}

func registerSession(s *session) error {
	sessionsLock.Lock()
	defer sessionsLock.Unlock()

	if _, ok := sessions.Load(s.sessionID); ok {
		return errDuplicateSessionID
	}
	if _, ok := sessionReservations[s.sessionID]; ok {
		return errDuplicateSessionID
	}

	sessions.Store(s.sessionID, registeredSession{session: s, name: s.sessionID.format()})
	return nil
}

func reserveSessionID(id SessionID, dynamic bool) (*sessionReservation, error) {
	sessionsLock.Lock()
	defer sessionsLock.Unlock()
	if _, exists := sessions.Load(id); exists {
		return nil, errDuplicateSessionID
	}
	if _, exists := sessionReservations[id]; exists {
		return nil, errDuplicateSessionID
	}
	reservation := &sessionReservation{dynamic: dynamic}
	sessionReservations[id] = reservation
	return reservation, nil
}

func registerReservedSession(s *session) {
	sessionsLock.Lock()
	defer sessionsLock.Unlock()
	sessions.Store(s.sessionID, registeredSession{session: s, name: s.sessionID.format()})
	if !s.reservation.dynamic {
		delete(sessionReservations, s.sessionID)
	}
}

func releaseSessionReservation(id SessionID, reservation *sessionReservation) {
	sessionsLock.Lock()
	defer sessionsLock.Unlock()
	if reservation != nil && sessionReservations[id] == reservation {
		delete(sessionReservations, id)
	}
}

// Removal waits for registry-owned sequence reads/writes, but never waits for
// sender locks or store Close while holding the registry lock.
func unregisterSessionInstance(s *session) {
	sessionsLock.Lock()
	defer sessionsLock.Unlock()
	if entry, ok := sessions.Load(s.sessionID); ok && entry.session == s {
		sessions.Delete(s.sessionID)
	}
}

func lookupSession(sessionID SessionID) (s *session, ok bool) {
	entry, ok := sessions.Load(sessionID)
	return entry.session, ok
}
