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
	"bytes"
	"time"

	"github.com/quickfixgo/quickfix/internal"
)

type inSession struct{ loggedOn }

func (state inSession) String() string { return "In Session" }

func (state inSession) FixMsgIn(session *session, msg *Message) sessionState {
	msgType, err := msg.Header.GetBytes(tagMsgType)
	if err != nil {
		return handleStateError(session, err)
	}

	switch {
	case bytes.Equal(msgTypeLogon, msgType):
		if err := session.handleLogon(msg); err != nil {
			if isOutboundCapacityError(err) {
				return handleStateError(session, err)
			}
			if err := session.initiateLogoutInReplyTo("", msg); err != nil {
				return handleStateError(session, err)
			}
			return logoutState{}
		}

		return state
	case bytes.Equal(msgTypeLogout, msgType):
		return state.handleLogout(session, msg)
	case bytes.Equal(msgTypeResendRequest, msgType):
		return state.handleResendRequest(session, msg)
	case bytes.Equal(msgTypeSequenceReset, msgType):
		return state.handleSequenceReset(session, msg)
	case bytes.Equal(msgTypeTestRequest, msgType):
		return state.handleTestRequest(session, msg)
	default:
		if err := session.verify(msg); err != nil {
			return state.processReject(session, msg, err)
		}
	}

	if err := session.store.IncrNextTargetMsgSeqNum(); err != nil {
		return handleStateError(session, err)
	}

	return state
}

func (state inSession) Timeout(session *session, event internal.Event) (nextState sessionState) {
	switch event {
	case internal.NeedHeartbeat:
		if err := session.sendHeartbeat(nil, nil); err != nil {
			return handleStateError(session, err)
		}
	case internal.PeerTimeout:
		if err := session.sendTestRequest(); err != nil {
			return handleStateError(session, err)
		}
		session.log.OnEvent("Sent test request TEST")
		session.peerTimer.Reset(time.Duration(float64(1.2) * float64(session.HeartBtInt)))
		return session.timeoutState(state)
	}

	return state
}

func (state inSession) handleLogout(session *session, msg *Message) (nextState sessionState) {
	if err := session.verifySelect(msg, false, false, true); err != nil {
		return state.processReject(session, msg, err)
	}

	if session.IsLoggedOn() {
		session.log.OnEvent("Received logout request")
		session.log.OnEvent("Sending logout response")

		if err := session.sendLogoutInReplyTo("", msg); err != nil {
			if isOutboundCapacityError(err) {
				return handleStateError(session, err)
			}
			session.logError(err)
		}
	} else {
		session.log.OnEvent("Received logout response")
	}

	if session.ResetOnLogout {
		if err := session.dropAndReset(); err != nil {
			session.logError(err)
		}
		return latentState{}
	}

	if err := session.checkTargetTooLow(msg); err != nil {
		return latentState{}
	}

	if err := session.checkTargetTooHigh(msg); err != nil {
		return latentState{}
	}

	if err := session.store.IncrNextTargetMsgSeqNum(); err != nil {
		session.logError(err)
	}

	return latentState{}
}

func (state inSession) handleTestRequest(session *session, msg *Message) (nextState sessionState) {
	if err := session.verify(msg); err != nil {
		return state.processReject(session, msg, err)
	}
	if testReq, present := msg.Body.getBytesIfPresent(tagTestReqID); !present {
		session.log.OnEvent("Test Request with no testRequestID")
	} else {
		if err := session.sendHeartbeat(msg, testReq); err != nil {
			return handleStateError(session, err)
		}
	}

	if err := session.store.IncrNextTargetMsgSeqNum(); err != nil {
		return handleStateError(session, err)
	}
	return state
}

func (state inSession) handleSequenceReset(session *session, msg *Message) (nextState sessionState) {
	var gapFillFlag bool
	if msg.Body.Has(tagGapFillFlag) {
		var err MessageRejectError
		if gapFillFlag, err = msg.Body.GetBool(tagGapFillFlag); err != nil {
			return state.processReject(session, msg, err)
		}
	}

	if err := session.verifySelect(msg, gapFillFlag, gapFillFlag, true); err != nil {
		return state.processReject(session, msg, err)
	}

	if newSeqNo, err := msg.Body.GetInt(tagNewSeqNo); err == nil {
		expectedSeqNum := session.store.NextTargetMsgSeqNum()
		session.logSequenceEvent(eventReceivedSequenceReset, expectedSeqNum, newSeqNo)

		switch {
		case newSeqNo > expectedSeqNum:
			if err := session.store.SetNextTargetMsgSeqNum(int(newSeqNo)); err != nil {
				return handleStateError(session, err)
			}
		case newSeqNo < expectedSeqNum:
			// FIXME: to be compliant with legacy tests, do not include tag in reftagid? (11c_NewSeqNoLess).
			if err := session.doReject(msg, valueIsIncorrectNoTag()); err != nil {
				return handleStateError(session, err)
			}
		}
	}
	return state
}

func (state inSession) handleResendRequest(session *session, msg *Message) (nextState sessionState) {
	if err := session.verifyIgnoreSeqNumTooHighOrLow(msg); err != nil {
		return state.processReject(session, msg, err)
	}

	beginSeqNo, err := msg.Body.GetInt(tagBeginSeqNo)
	if err != nil {
		return state.processReject(session, msg, RequiredTagMissing(tagBeginSeqNo))
	}

	endSeqNo, err := msg.Body.GetInt(tagEndSeqNo)
	if err != nil {
		return state.processReject(session, msg, RequiredTagMissing(tagEndSeqNo))
	}

	session.logSequenceEvent(eventReceivedResendRequest, beginSeqNo, endSeqNo)
	expectedSeqNum := session.store.NextSenderMsgSeqNum()

	if (session.sessionID.BeginString >= BeginStringFIX42 && endSeqNo == 0) ||
		(session.sessionID.BeginString <= BeginStringFIX42 && endSeqNo == 999999) ||
		(endSeqNo >= expectedSeqNum) {
		endSeqNo = expectedSeqNum - 1
	}

	if err := state.resendMessages(session, int(beginSeqNo), endSeqNo, msg); err != nil {
		return handleStateError(session, err)
	}

	if err := session.checkTargetTooLow(msg); err != nil {
		return state
	}

	if err := session.checkTargetTooHigh(msg); err != nil {
		return state
	}

	if err := session.store.IncrNextTargetMsgSeqNum(); err != nil {
		return handleStateError(session, err)
	}
	return state
}

func (state inSession) resendMessages(session *session, beginSeqNo, endSeqNo int, inReplyTo *Message) error {
	if session.DisableMessagePersist {
		return state.generateSequenceReset(session, beginSeqNo, endSeqNo+1, inReplyTo)
	}
	if session.outbound != nil {
		return session.resendScopedMessages(beginSeqNo, endSeqNo, inReplyTo)
	}

	// resendMutex must always be locked before sendMutex to prevent a potential deadlock
	// sendMutex is locked below in session.EnqueueBytesAndSend()
	session.resendMutex.Lock()
	defer session.resendMutex.Unlock()

	seqNum := beginSeqNo
	nextSeqNum := seqNum
	msg := NewMessage()
	err := session.store.IterateMessages(beginSeqNo, endSeqNo, func(msgBytes []byte) error {
		err := ParseMessageWithDataDictionary(msg, bytes.NewBuffer(msgBytes), session.transportDataDictionary, session.appDataDictionary)
		if err != nil {
			session.log.OnEventf("Resend Msg Parse Error: %v, %v", err.Error(), bytes.NewBuffer(msgBytes).String())
			return err // We cant continue with a message that cant be parsed correctly.
		}
		msgType, _ := msg.Header.GetBytes(tagMsgType)
		sentMessageSeqNum, _ := msg.Header.GetInt(tagMsgSeqNum)

		if isAdminMessageType(msgType) {
			nextSeqNum = sentMessageSeqNum + 1
			return nil
		}

		if !session.resend(msg) {
			nextSeqNum = sentMessageSeqNum + 1
			return nil
		}

		if seqNum != sentMessageSeqNum {
			if err = state.generateSequenceReset(session, seqNum, sentMessageSeqNum, inReplyTo); err != nil {
				return err
			}
		}

		session.logSequenceEvent(eventResendingMessage, sentMessageSeqNum, 0)
		// Keep the original serialized body and repeating-group order even if
		// the resend callback changes its convenience field maps.
		if err := session.enqueueReplay(msg, msg.bodyBytes); err != nil {
			return err
		}

		seqNum = sentMessageSeqNum + 1
		nextSeqNum = seqNum
		return nil
	})
	if err != nil {
		session.log.OnEventf("error retrieving messages from store: %s", err.Error())
		return err
	}

	if seqNum != nextSeqNum { // gapfill for catch-up
		if err = state.generateSequenceReset(session, seqNum, nextSeqNum, inReplyTo); err != nil {
			return err
		}
	}

	return nil
}

func (state inSession) processReject(session *session, msg *Message, rej MessageRejectError) sessionState {
	switch TypedError := rej.(type) {
	case targetTooHigh:
		return state.stashTooHigh(session, msg, TypedError)
	case *targetTooHigh:
		return state.stashTooHigh(session, msg, *TypedError)
	case targetTooLow:
		return state.doTargetTooLow(session, msg, TypedError)
	case *targetTooLow:
		return state.doTargetTooLow(session, msg, *TypedError)
	case incorrectBeginString:
		if err := session.initiateLogout(rej.Error()); err != nil {
			return handleStateError(session, err)
		}
		return logoutState{}
	}

	switch rej.RejectReason() {
	case rejectReasonCompIDProblem, rejectReasonSendingTimeAccuracyProblem:
		if err := session.doReject(msg, rej); err != nil {
			return handleStateError(session, err)
		}

		if err := session.initiateLogout(""); err != nil {
			return handleStateError(session, err)
		}
		return logoutState{}
	default:
		if err := session.doReject(msg, rej); err != nil {
			return handleStateError(session, err)
		}

		if err := session.store.IncrNextTargetMsgSeqNum(); err != nil {
			return handleStateError(session, err)
		}
		return state
	}
}

func (state inSession) stashTooHigh(session *session, msg *Message, reject targetTooHigh) sessionState {
	stashIndex := -1
	if msg.inbound != nil {
		var err error
		stashIndex, err = session.boundedIn.ring.stashIndex(reject.ReceivedTarget)
		if err != nil {
			return session.failBoundedInbound(err)
		}
	}
	var nextState resendState
	currentState, recovering := session.State.(resendState)
	if msg.inbound != nil {
		currentState, recovering = boundedResendState(session.State)
	}
	if recovering {
		nextState = currentState
	} else {
		var err error
		if nextState, err = session.doTargetTooHigh(reject); err != nil {
			return handleStateError(session, err)
		}
	}
	if msg.inbound != nil {
		session.boundedIn.ring.retain(stashIndex, reject.ReceivedTarget, msg.inbound)
		return session.recoveryState(nextState)
	}
	if nextState.messageStash == nil {
		nextState.messageStash = make(map[int]*Message)
	}
	nextState.messageStash[reject.ReceivedTarget] = msg
	return nextState
}

func (state inSession) doTargetTooLow(session *session, msg *Message, rej targetTooLow) (nextState sessionState) {
	var posDupFlag bool
	if msg.Header.Has(tagPossDupFlag) {
		var err MessageRejectError
		if posDupFlag, err = msg.Header.GetBool(tagPossDupFlag); err != nil {
			if rejErr := session.doReject(msg, err); rejErr != nil {
				return handleStateError(session, rejErr)
			}
			return state
		}
	}

	if !posDupFlag {
		if err := session.initiateLogout(rej.Error()); err != nil {
			return handleStateError(session, err)
		}
		return logoutState{}
	}

	if !msg.Header.Has(tagOrigSendingTime) {
		if err := session.doReject(msg, RequiredTagMissing(tagOrigSendingTime)); err != nil {
			return handleStateError(session, err)
		}
		return state
	}

	origSendingTime, err := msg.Header.GetTime(tagOrigSendingTime)
	if err != nil {
		if rejErr := session.doReject(msg, err); rejErr != nil {
			return handleStateError(session, rejErr)
		}
		return state
	}

	sendingTime, err := msg.Header.GetTime(tagSendingTime)
	if err != nil {
		return state.processReject(session, msg, err)
	}

	if sendingTime.Before(origSendingTime) {
		if err := session.doReject(msg, sendingTimeAccuracyProblem()); err != nil {
			return handleStateError(session, err)
		}

		if err := session.initiateLogout(""); err != nil {
			return handleStateError(session, err)
		}
		return logoutState{}
	}

	return state
}

func (state *inSession) generateSequenceReset(session *session, beginSeqNo int, endSeqNo int, inReplyTo *Message) error {
	return session.generateSequenceReset(beginSeqNo, endSeqNo, inReplyTo)
}
