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

import "github.com/quickfixgo/quickfix/internal"

type resendState struct {
	loggedOn
	messageStash          map[int]*Message
	currentResendRangeEnd int
	resendRangeEnd        int
}

func (s resendState) String() string { return "Resend" }

func (s resendState) Timeout(session *session, event internal.Event) (nextState sessionState) {
	nextState = inSession{}.Timeout(session, event)
	switch nextState.(type) {
	case inSession:
		nextState = s
	case pendingTimeout:
		// Wrap pendingTimeout in resend. prevents us falling back to inSession if recovering
		// from pendingTimeout.
		nextState = pendingTimeout{s}
	}

	return
}

func (s resendState) FixMsgIn(session *session, msg *Message) (nextState sessionState) {
	nextState = inSession{}.FixMsgIn(session, msg)

	if !nextState.IsLoggedOn() {
		return
	}
	if session.boundedIn != nil {
		session.boundedIn.ring.discardBefore(session.store.NextTargetMsgSeqNum())
	}

	if s.currentResendRangeEnd != 0 && s.currentResendRangeEnd < session.store.NextTargetMsgSeqNum() {
		nextResendState, err := session.sendResendRequest(session.store.NextTargetMsgSeqNum(), s.resendRangeEnd)
		if err != nil {
			return handleStateError(session, err)
		}
		nextResendState.messageStash = s.messageStash
		return nextResendState
	}

	var gapFillFlag FIXBoolean
	if msg.Body.Has(tagGapFillFlag) {
		if err := msg.Body.GetField(tagGapFillFlag, &gapFillFlag); err != nil {
			return handleStateError(session, err)
		}
	}

	if bool(gapFillFlag) && s.currentResendRangeEnd != 0 && s.currentResendRangeEnd == session.store.NextTargetMsgSeqNum() {
		nextResendState, err := session.sendResendRequest(session.store.NextTargetMsgSeqNum(), s.resendRangeEnd)
		if err != nil {
			return handleStateError(session, err)
		}
		nextResendState.messageStash = s.messageStash
		return nextResendState
	}

	if s.resendRangeEnd >= session.store.NextTargetMsgSeqNum() {
		return s
	}
	if session.boundedIn != nil {
		ring := session.boundedIn.ring
		for {
			slot := ring.take(session.store.NextTargetMsgSeqNum())
			if slot == nil {
				break
			}
			nextState = inSession{}.FixMsgIn(session, slot.parsed.Message())
			slot.release()
			if !nextState.IsLoggedOn() {
				return
			}
			ring.discardBefore(session.store.NextTargetMsgSeqNum())
		}
		// Later out-of-order frames can extend beyond the original recovery
		// range. Keep their slots and request the next gap instead of discarding
		// messages merely because the original range has completed.
		lowest, found := 0, false
		for _, entry := range ring.stash {
			if entry.slot != nil && (!found || entry.seq < lowest) {
				lowest, found = entry.seq, true
			}
		}
		if found {
			state, err := session.sendResendRequest(session.store.NextTargetMsgSeqNum(), lowest-1)
			if err != nil {
				return handleStateError(session, err)
			}
			return state
		}
		return
	}

	for len(s.messageStash) > 0 {
		targetSeqNum := session.store.NextTargetMsgSeqNum()
		msg, ok := s.messageStash[targetSeqNum]
		if !ok {
			break
		}

		delete(s.messageStash, targetSeqNum)

		nextState = inSession{}.FixMsgIn(session, msg)
		if !nextState.IsLoggedOn() {
			return
		}
	}

	return
}
