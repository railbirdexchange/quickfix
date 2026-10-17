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
	// Engine-generated controls are owned messages that remain valid afterward.
	// BorrowApplication explicitly replaces owned delivery with callback-scoped
	// views of engine-generated controls.
	ToAdmin(message *Message, sessionID SessionID)

	// ToApp notification of app message being sent to target. Ordinary sends
	// pass the original caller-owned Message synchronously. During resend,
	// the engine supplies an owned message that survives request completion.
	// BorrowApplication explicitly replaces owned replay delivery with a
	// callback-scoped view.
	ToApp(message *Message, sessionID SessionID) error

	// FromAdmin processes an inbound admin message. Messages and their field
	// bytes remain valid after the callback returns. BorrowApplication explicitly
	// replaces this owned delivery with callback-scoped delivery.
	FromAdmin(message *Message, sessionID SessionID) MessageRejectError

	// FromApp processes an inbound application message, with the same ownership
	// rules as FromAdmin.
	FromApp(message *Message, sessionID SessionID) MessageRejectError
}

// BorrowApplication declares that application finishes using engine-owned
// messages and byte views before each callback returns. It uses the same session
// engine as an ordinary Application, omitting the ownership copies at delivery.
// Pass the returned Application to NewAcceptor or NewInitiator.
//
// Incoming messages, engine-generated admin messages and resend messages must
// not be retained or passed to asynchronous work. CopyInto, GetGroup and string
// getters can create owned values when retention is necessary, and may allocate.
// Caller-created outgoing Messages remain caller-owned. Repeated sends can
// attach fixed construction storage; field byte views may move or be overwritten
// on subsequent mutation or send. Do not retain those views across operations.
// Ordinary Application delivery retains the legacy lifetimes.
func BorrowApplication(application Application) Application {
	if applicationBorrowsMessages(application) {
		return application
	}
	return borrowedApplication{Application: application}
}

type borrowedApplication struct{ Application }

func applicationBorrowsMessages(application Application) bool {
	_, borrowed := application.(borrowedApplication)
	return borrowed
}
