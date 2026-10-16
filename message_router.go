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

// FIX ApplVerID string values.
const (
	ApplVerIDFIX27    = "0"
	ApplVerIDFIX30    = "1"
	ApplVerIDFIX40    = "2"
	ApplVerIDFIX41    = "3"
	ApplVerIDFIX42    = "4"
	ApplVerIDFIX43    = "5"
	ApplVerIDFIX44    = "6"
	ApplVerIDFIX50    = "7"
	ApplVerIDFIX50SP1 = "8"
	ApplVerIDFIX50SP2 = "9"
)

// A MessageRoute is a function that can process a fromApp/fromAdmin callback.
type MessageRoute func(msg *Message, sessionID SessionID) MessageRejectError

// A MessageRouter dispatches messages to MessageRoutes. Configure routes before
// using the router concurrently; concurrent Route calls do not modify the router.
type MessageRouter struct {
	routes map[string]map[string]MessageRoute
}

// NewMessageRouter returns an initialized MessageRouter instance.
func NewMessageRouter() *MessageRouter {
	return &MessageRouter{routes: make(map[string]map[string]MessageRoute)}
}

// AddRoute adds a route to the MessageRouter instance keyed to begin string and msgType.
func (c MessageRouter) AddRoute(beginString string, msgType string, router MessageRoute) {
	routes, ok := c.routes[beginString]
	if !ok {
		routes = make(map[string]MessageRoute)
		c.routes[beginString] = routes
	}
	routes[msgType] = router
}

// Route may be called from the fromApp/fromAdmin callbacks. Messages that cannot be routed will be rejected with UnsupportedMessageType.
func (c MessageRouter) Route(msg *Message, sessionID SessionID) MessageRejectError {
	beginString, err := msg.Header.GetBytes(tagBeginString)
	if err != nil {
		return nil
	}

	msgType, err := msg.Header.GetBytes(tagMsgType)
	if err != nil {
		return err
	}

	return c.tryRoute(beginString, msgType, msg, sessionID)
}

func (c MessageRouter) tryRoute(beginString []byte, msgType []byte, msg *Message, sessionID SessionID) MessageRejectError {
	// Direct string-map lookups borrow the bytes only for the lookup, including
	// custom values longer than the compiler's temporary string buffer.
	routes := c.routes[string(beginString)]
	isAdminMsg := isAdminMessageType(msgType)

	if string(beginString) == BeginStringFIXT11 && !isAdminMsg {
		applVerID, present := msg.Header.getBytesIfPresent(tagApplVerID)
		if !present {
			session, ok := lookupSession(sessionID)
			if !ok {
				return NewMessageRejectError(errUnknownSession.Error(), rejectReasonCompIDProblem, nil)
			}
			applVerID = []byte(session.TargetDefaultApplicationVersionID())
		}

		routes = c.routesForApplVerID(applVerID)
	}

	if route, ok := routes[string(msgType)]; ok {
		return route(msg, sessionID)
	}

	switch {
	case isAdminMsg || string(msgType) == "j":
		return nil
	}

	return UnsupportedMessageType()
}

func (c MessageRouter) routesForApplVerID(applVerID []byte) map[string]MessageRoute {
	switch string(applVerID) {
	case ApplVerIDFIX40:
		return c.routes[BeginStringFIX40]
	case ApplVerIDFIX41:
		return c.routes[BeginStringFIX41]
	case ApplVerIDFIX42:
		return c.routes[BeginStringFIX42]
	case ApplVerIDFIX43:
		return c.routes[BeginStringFIX43]
	case ApplVerIDFIX44:
		return c.routes[BeginStringFIX44]
	default:
		return c.routes[string(applVerID)]
	}
}
