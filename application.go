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

// Application interface should be implemented by FIX Applications.
// This is the primary interface for processing messages from a FIX Session.
type Application interface {
	// OnCreate notification of a session begin created.
	OnCreate(sessionID SessionID)

	// OnLogon notification of a session successfully logging on.
	OnLogon(sessionID SessionID)

	// OnLogout notification of a session logging off or disconnecting.
	OnLogout(sessionID SessionID)

	// ToAdmin notification of admin message being sent to target. A message
	// supplied to Send or SendToTarget remains caller-owned after sending.
	// Engine-generated controls are session-owned callback views: retain their
	// contents with CopyInto or owned string getters before returning. Their
	// Message and GetBytes results must not be retained for asynchronous work.
	ToAdmin(message *Message, sessionID SessionID)

	// ToApp notification of app message being sent to target. Ordinary sends
	// pass the original caller-owned Message synchronously. During resend,
	// the engine supplies a scoped replay view: retain it with CopyInto, or
	// retain individual owned GetString/FIXString values, before returning.
	ToApp(message *Message, sessionID SessionID) error

	// FromAdmin processes an inbound admin message synchronously. The Message,
	// GetBytes results and explicitly borrowed views are valid only during this callback.
	// Do not retain them or pass them to asynchronous work. CopyInto detaches
	// scoped input for retention; GetGroup returns owned group data. GetString,
	// FIXString and String return owned strings. Ordinary setters remain
	// available but may allocate.
	FromAdmin(message *Message, sessionID SessionID) MessageRejectError

	// FromApp processes an inbound application message synchronously, with the
	// same scoped Message/byte lifetime and retention rules as FromAdmin.
	FromApp(message *Message, sessionID SessionID) MessageRejectError
}
