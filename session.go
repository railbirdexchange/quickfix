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
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/quickfixgo/quickfix/datadictionary"
	"github.com/quickfixgo/quickfix/internal"
)

// The Session is the primary FIX abstraction for message communication.
type session struct {
	store MessageStore

	log       Log
	sessionID SessionID

	connection net.Conn
	messageIn  <-chan fixIn
	// Closed after the session has detached the active connection.
	connectionDone chan struct{}

	// Mutex for sender sequence numbers, persistence, and socket writes.
	sendMutex sync.Mutex
	// Protected by sendMutex. Application messages may only be written after Logon.
	applicationSendingEnabled bool
	// Mutex to prevent messages being sent when resendRequest is active
	// Must be locked before sendMutex to prevent a potential deadlock
	resendMutex sync.RWMutex

	sessionEvent chan internal.Event
	application  Application
	Validator
	stateMachine
	stateTimer *internal.EventTimer
	peerTimer  *internal.EventTimer
	sentReset  bool
	stopOnce   sync.Once

	targetDefaultApplVerID string

	admin chan interface{}
	internal.SessionSettings
	transportDataDictionary *datadictionary.DataDictionary
	appDataDictionary       *datadictionary.DataDictionary

	timestampPrecision      TimestampPrecision
	lastCheckedResetSeqTime time.Time
}

func (s *session) logError(err error) {
	s.log.OnEvent(err.Error())
}

// TargetDefaultApplicationVersionID returns the default application version ID for messages received by this version.
// Applicable for For FIX.T.1 sessions.
func (s *session) TargetDefaultApplicationVersionID() string {
	return s.targetDefaultApplVerID
}

type connect struct {
	connection     net.Conn
	messageIn      <-chan fixIn
	connectionDone chan struct{}
	err            chan<- error
}

func (s *session) connect(msgIn <-chan fixIn, connection net.Conn, connectionDone chan struct{}) error {
	rep := make(chan error)
	s.admin <- connect{
		connection:     connection,
		messageIn:      msgIn,
		connectionDone: connectionDone,
		err:            rep,
	}

	return <-rep
}

type stopReq struct{}

var errSessionDisconnected = errors.New("session disconnected")

func (s *session) stop() {
	// Stop once.
	s.stopOnce.Do(func() {
		s.admin <- stopReq{}
	})
}

type waitChan <-chan interface{}

type waitForInSessionReq struct{ rep chan<- waitChan }

func (s *session) waitForInSessionTime() {
	rep := make(chan waitChan)
	s.admin <- waitForInSessionReq{rep}
	if wait, ok := <-rep; ok {
		<-wait
	}
}

func (s *session) insertSendingTime(msg *Message) {
	sendingTime := time.Now().UTC()

	if s.sessionID.BeginString >= BeginStringFIX42 {
		msg.Header.SetField(tagSendingTime, FIXUTCTimestamp{Time: sendingTime, Precision: s.timestampPrecision})
	} else {
		msg.Header.SetField(tagSendingTime, FIXUTCTimestamp{Time: sendingTime, Precision: Seconds})
	}
}

func optionallySetID(msg *Message, field Tag, value string) {
	if len(value) != 0 {
		msg.Header.SetString(field, value)
	}
}

func (s *session) fillDefaultHeader(msg *Message, inReplyTo *Message) {
	msg.Header.SetString(tagBeginString, s.sessionID.BeginString)
	msg.Header.SetString(tagSenderCompID, s.sessionID.SenderCompID)
	optionallySetID(msg, tagSenderSubID, s.sessionID.SenderSubID)
	optionallySetID(msg, tagSenderLocationID, s.sessionID.SenderLocationID)

	msg.Header.SetString(tagTargetCompID, s.sessionID.TargetCompID)
	optionallySetID(msg, tagTargetSubID, s.sessionID.TargetSubID)
	optionallySetID(msg, tagTargetLocationID, s.sessionID.TargetLocationID)

	s.insertSendingTime(msg)

	if s.EnableLastMsgSeqNumProcessed {
		if inReplyTo != nil {
			if lastSeqNum, err := inReplyTo.Header.GetInt(tagMsgSeqNum); err != nil {
				s.logError(err)
			} else {
				msg.Header.SetInt(tagLastMsgSeqNumProcessed, lastSeqNum)
			}
		} else {
			msg.Header.SetInt(tagLastMsgSeqNumProcessed, s.store.NextTargetMsgSeqNum()-1)
		}
	}
}

func (s *session) shouldSendReset() bool {
	if s.sessionID.BeginString < BeginStringFIX41 {
		return false
	}

	return (s.ResetOnLogon || s.ResetOnDisconnect || s.ResetOnLogout) &&
		s.store.NextTargetMsgSeqNum() == 1 && s.store.NextSenderMsgSeqNum() == 1
}

func (s *session) sendLogon() error {
	return s.sendLogonInReplyTo(s.shouldSendReset(), nil)
}

func (s *session) sendLogonInReplyTo(setResetSeqNum bool, inReplyTo *Message) error {
	logon := NewMessage()
	logon.Header.SetField(tagMsgType, FIXString("A"))
	logon.Header.SetField(tagBeginString, FIXString(s.sessionID.BeginString))
	logon.Header.SetField(tagTargetCompID, FIXString(s.sessionID.TargetCompID))
	logon.Header.SetField(tagSenderCompID, FIXString(s.sessionID.SenderCompID))
	logon.Body.SetField(tagEncryptMethod, FIXString("0"))
	logon.Body.SetField(tagHeartBtInt, FIXInt(s.HeartBtInt.Seconds()))

	if setResetSeqNum {
		logon.Body.SetField(tagResetSeqNumFlag, FIXBoolean(true))
	}

	if len(s.DefaultApplVerID) > 0 {
		logon.Body.SetField(tagDefaultApplVerID, FIXString(s.DefaultApplVerID))
	}

	// Evaluate tag 789.
	if s.EnableNextExpectedMsgSeqNum {
		if inReplyTo != nil {
			targetWantsNextSeqNumToBe, getErr := inReplyTo.Body.GetInt(tagNextExpectedMsgSeqNum)
			if getErr == nil {
				actualNextNum := s.store.NextSenderMsgSeqNum()
				// // Is the 789 we received too high ??
				if targetWantsNextSeqNumToBe > actualNextNum {
					// we can't resend what we never sent! something unrecoverable has happened.
					return RejectLogon{fmt.Sprintf("Tag 789 (NextExpectedMsgSeqNum) is higher than expected. Expected %d, Received %d", actualNextNum, targetWantsNextSeqNumToBe)}
				}
				nextSeqNum := s.store.NextTargetMsgSeqNum()
				logon.Body.SetField(tagNextExpectedMsgSeqNum, FIXInt(nextSeqNum+1))
			}
		} else {
			// We are sending a logon.
			nextseqnum := s.store.NextTargetMsgSeqNum()
			logon.Body.SetField(tagNextExpectedMsgSeqNum, FIXInt(nextseqnum+1))
		}
	}

	if err := s.dropAndSendInReplyTo(logon, inReplyTo); err != nil {
		return err
	}

	return nil
}

func (s *session) generateSequenceReset(beginSeqNo int, endSeqNo int, inReplyTo Message) (err error) {
	sequenceReset := NewMessage()
	s.fillDefaultHeader(sequenceReset, &inReplyTo)

	sequenceReset.Header.SetField(tagMsgType, FIXString("4"))
	sequenceReset.Header.SetField(tagMsgSeqNum, FIXInt(beginSeqNo))
	sequenceReset.Header.SetField(tagPossDupFlag, FIXBoolean(true))
	sequenceReset.Body.SetField(tagNewSeqNo, FIXInt(endSeqNo))
	sequenceReset.Body.SetField(tagGapFillFlag, FIXBoolean(true))

	var origSendingTime FIXString
	if err := sequenceReset.Header.GetField(tagSendingTime, &origSendingTime); err == nil {
		sequenceReset.Header.SetField(tagOrigSendingTime, origSendingTime)
	}

	s.application.ToAdmin(sequenceReset, s.sessionID)

	msgBytes := sequenceReset.build()

	s.resendMutex.RLock()
	if err = s.sendRaw(msgBytes); err != nil {
		s.resendMutex.RUnlock()
		return
	}
	s.resendMutex.RUnlock()
	s.log.OnEventf("Sent SequenceReset TO: %v", endSeqNo)

	return
}

func (s *session) buildLogout(reason string) *Message {
	logout := NewMessage()
	logout.Header.SetField(tagMsgType, FIXString("5"))
	logout.Header.SetField(tagBeginString, FIXString(s.sessionID.BeginString))
	logout.Header.SetField(tagTargetCompID, FIXString(s.sessionID.TargetCompID))
	logout.Header.SetField(tagSenderCompID, FIXString(s.sessionID.SenderCompID))
	if reason != "" {
		logout.Body.SetField(tagText, FIXString(reason))
	}

	return logout
}

func (s *session) sendLogout(reason string) error {
	return s.sendLogoutInReplyTo(reason, nil)
}

func (s *session) sendLogoutInReplyTo(reason string, inReplyTo *Message) error {
	logout := s.buildLogout(reason)
	return s.dropAndSendInReplyTo(logout, inReplyTo)
}

func (s *session) resend(msg *Message) bool {
	msg.Header.SetField(tagPossDupFlag, FIXBoolean(true))

	var origSendingTime FIXString
	if err := msg.Header.GetField(tagSendingTime, &origSendingTime); err == nil {
		msg.Header.SetField(tagOrigSendingTime, origSendingTime)
	}

	s.insertSendingTime(msg)

	return s.application.ToApp(msg, s.sessionID) == nil
}

// send validates and persists the message, then writes it if the session is logged on.
func (s *session) send(msg *Message) error {
	return s.sendInReplyTo(msg, nil)
}

func (s *session) sendInReplyTo(msg *Message, inReplyTo *Message) error {
	// resendMutex must always be locked before sendMutex to prevent a potential deadlock
	s.resendMutex.RLock()
	writeResult, err := s.prepareAndWrite(msg, inReplyTo, false)
	s.resendMutex.RUnlock()
	return s.finishSocketWrite(writeResult, err)
}

// dropAndReset resets the message store.
func (s *session) dropAndReset() error {
	s.sendMutex.Lock()
	defer s.sendMutex.Unlock()

	return s.store.Reset()
}

// dropAndSend validates, persists, and writes an administrative message before Logon completes.
func (s *session) dropAndSend(msg *Message) error {
	return s.dropAndSendInReplyTo(msg, nil)
}

func (s *session) dropAndSendInReplyTo(msg *Message, inReplyTo *Message) error {
	s.resendMutex.RLock()
	writeResult, err := s.prepareAndWrite(msg, inReplyTo, true)
	s.resendMutex.RUnlock()
	return s.finishSocketWrite(writeResult, err)
}

type socketWriteResult struct {
	connection net.Conn
	event      SendStageEvent
	attempted  bool
}

func (s *session) prepareAndWrite(msg *Message, inReplyTo *Message, force bool) (socketWriteResult, error) {
	s.sendMutex.Lock()
	defer s.sendMutex.Unlock()

	if !force {
		now := time.Now()
		if !s.applicationSendingEnabled ||
			!s.SessionTime.IsInRange(now) ||
			!s.SessionTime.IsInSameRange(s.store.CreationTime(), now) {
			return socketWriteResult{}, nil
		}
	}

	msgBytes, err := s.prepMessageForSend(msg, inReplyTo)
	if err != nil {
		return socketWriteResult{}, err
	}

	return s.writeLocked(msgBytes)
}

func (s *session) prepMessageForSend(msg *Message, inReplyTo *Message) (msgBytes []byte, err error) {
	s.fillDefaultHeader(msg, inReplyTo)
	seqNum := s.store.NextSenderMsgSeqNum()
	msg.Header.SetField(tagMsgSeqNum, FIXInt(seqNum))

	msgType, err := msg.Header.GetBytes(tagMsgType)
	if err != nil {
		return
	}

	if isAdminMessageType(msgType) {
		s.application.ToAdmin(msg, s.sessionID)
		if bytes.Equal(msgType, msgTypeLogon) {
			var resetSeqNumFlag FIXBoolean
			if msg.Body.Has(tagResetSeqNumFlag) {
				if err = msg.Body.GetField(tagResetSeqNumFlag, &resetSeqNumFlag); err != nil {
					return
				}
			}

			if resetSeqNumFlag.Bool() {
				if err = s.store.Reset(); err != nil {
					return
				}

				s.sentReset = true
				seqNum = s.store.NextSenderMsgSeqNum()
				msg.Header.SetField(tagMsgSeqNum, FIXInt(seqNum))
			}
		}
	} else {
		if err = s.application.ToApp(msg, s.sessionID); err != nil {
			return
		}
	}

	// Message converted to bytes here.
	msgBytes = msg.build()
	err = s.persist(seqNum, msgBytes)

	return
}

func (s *session) persist(seqNum int, msgBytes []byte) error {
	if !s.DisableMessagePersist {
		return s.store.SaveMessageAndIncrNextSenderMsgSeqNum(seqNum, msgBytes)
	}

	return s.store.IncrNextSenderMsgSeqNum()
}

// sendRaw writes stored FIX bytes without changing sequence or persistence.
// The caller must hold resendMutex before calling it.
func (s *session) sendRaw(msg []byte) error {
	s.sendMutex.Lock()
	writeResult, err := s.writeLocked(msg)
	s.sendMutex.Unlock()
	return s.finishSocketWrite(writeResult, err)
}

// writeLocked writes one complete FIX message while sendMutex is held.
func (s *session) writeLocked(msg []byte) (socketWriteResult, error) {
	if s.connection == nil {
		return socketWriteResult{}, errSessionDisconnected
	}

	result := socketWriteResult{connection: s.connection}
	if s.SocketWriteTimeout > 0 {
		if err := s.connection.SetWriteDeadline(time.Now().Add(s.SocketWriteTimeout)); err != nil {
			s.connection = nil
			s.applicationSendingEnabled = false
			return result, err
		}
	}

	writeStartedAt := time.Now()
	written, err := s.connection.Write(msg)
	result.attempted = true
	result.event = SendStageEvent{
		SessionID: s.sessionID,
		MsgType:   fixMsgTypeFromRaw(msg),
		Stage:     "socket_write",
		Duration:  time.Since(writeStartedAt),
		Success:   err == nil && written == len(msg),
	}
	if err == nil && written != len(msg) {
		err = io.ErrShortWrite
	}
	if err != nil {
		s.connection = nil
		s.applicationSendingEnabled = false
		return result, err
	}

	s.log.OnOutgoing(msg)
	s.stateTimer.Reset(s.HeartBtInt)
	return result, nil
}

func (s *session) finishSocketWrite(result socketWriteResult, err error) error {
	if err != nil {
		s.logError(err)
		if result.connection != nil {
			_ = result.connection.Close()
		}
	}
	if result.attempted {
		observeSendStage(result.event)
	}
	return err
}

func (s *session) setApplicationSendingEnabled(enabled bool) {
	s.sendMutex.Lock()
	s.applicationSendingEnabled = enabled && s.connection != nil
	s.sendMutex.Unlock()
}

func (s *session) doTargetTooHigh(reject targetTooHigh) (nextState resendState, err error) {
	s.log.OnEventf("MsgSeqNum too high, expecting %v but received %v", reject.ExpectedTarget, reject.ReceivedTarget)
	return s.sendResendRequest(reject.ExpectedTarget, reject.ReceivedTarget-1)
}

func (s *session) sendResendRequest(beginSeq, endSeq int) (nextState resendState, err error) {
	nextState.resendRangeEnd = endSeq

	resend := NewMessage()
	resend.Header.SetBytes(tagMsgType, msgTypeResendRequest)
	resend.Body.SetField(tagBeginSeqNo, FIXInt(beginSeq))

	var endSeqNo int
	if s.ResendRequestChunkSize != 0 {
		endSeqNo = beginSeq + s.ResendRequestChunkSize - 1
	} else {
		endSeqNo = endSeq
	}

	if endSeqNo < endSeq {
		nextState.currentResendRangeEnd = endSeqNo
	} else {
		if s.sessionID.BeginString < BeginStringFIX42 {
			endSeqNo = 999999
		} else {
			endSeqNo = 0
		}
	}
	resend.Body.SetField(tagEndSeqNo, FIXInt(endSeqNo))

	if err = s.dropAndSend(resend); err != nil {
		return
	}
	s.log.OnEventf("Sent ResendRequest FROM: %v TO: %v", beginSeq, endSeqNo)

	return
}

func (s *session) handleLogon(msg *Message) error {
	// Grab default app ver id from fixt.1.1 logon.
	if s.sessionID.BeginString == BeginStringFIXT11 {
		var targetApplVerID FIXString

		if err := msg.Body.GetField(tagDefaultApplVerID, &targetApplVerID); err != nil {
			return err
		}

		s.targetDefaultApplVerID = string(targetApplVerID)
	}

	resetStore := false
	if s.InitiateLogon {
		s.log.OnEvent("Received logon response")
	} else {
		s.log.OnEvent("Received logon request")
		resetStore = s.ResetOnLogon

		if s.RefreshOnLogon {
			if err := s.store.Refresh(); err != nil {
				return err
			}
		}
	}

	nextSenderMsgNumAtLogonReceived := s.store.NextSenderMsgSeqNum()

	// Make sure this is a valid session before resetting the store.
	if err := s.verifyMsgAgainstAppImpl(msg); err != nil {
		return err
	}

	var resetSeqNumFlag FIXBoolean
	if err := msg.Body.GetField(tagResetSeqNumFlag, &resetSeqNumFlag); err == nil {
		if resetSeqNumFlag {
			if !s.sentReset {
				s.log.OnEvent("Logon contains ResetSeqNumFlag=Y, resetting sequence numbers to 1")
				resetStore = true
			}
		}
	}

	if resetStore {
		if err := s.store.Reset(); err != nil {
			return err
		}
	}

	// Verify seq num too high but dont check against app implementation since we just did that.
	// Don't need to double check.
	if err := s.verifyIgnoreSeqNumTooHigh(msg); err != nil {
		return err
	}

	if !s.InitiateLogon {
		if !s.HeartBtIntOverride {
			var heartBtInt FIXInt
			if err := msg.Body.GetField(tagHeartBtInt, &heartBtInt); err == nil {
				s.HeartBtInt = time.Duration(heartBtInt) * time.Second
			}
		}

		s.log.OnEvent("Responding to logon request")
		if err := s.sendLogonInReplyTo(resetSeqNumFlag.Bool(), msg); err != nil {
			return err
		}
	}
	s.sentReset = false

	s.peerTimer.Reset(time.Duration(float64(1.2) * float64(s.HeartBtInt)))

	// Evaluate tag 789 to see if we end up with an implied gapfill/resend.
	if s.EnableNextExpectedMsgSeqNum && !msg.Body.Has(tagResetSeqNumFlag) {
		targetWantsNextSeqNumToBe, getErr := msg.Body.GetInt(tagNextExpectedMsgSeqNum)
		if getErr == nil {
			if targetWantsNextSeqNumToBe != nextSenderMsgNumAtLogonReceived {
				if !s.DisableMessagePersist {
					seqResetErr := s.generateSequenceReset(targetWantsNextSeqNumToBe, nextSenderMsgNumAtLogonReceived+1, *msg)
					if seqResetErr != nil {
						return seqResetErr
					}
				} else {
					return targetTooHigh{ReceivedTarget: targetWantsNextSeqNumToBe, ExpectedTarget: nextSenderMsgNumAtLogonReceived}
				}
			}
		}
	}

	if err := s.checkTargetTooHigh(msg); err != nil {
		return err
	}

	if err := s.store.IncrNextTargetMsgSeqNum(); err != nil {
		return err
	}

	s.setApplicationSendingEnabled(true)
	s.application.OnLogon(s.sessionID)
	return nil
}

func (s *session) initiateLogout(reason string) (err error) {
	return s.initiateLogoutInReplyTo(reason, nil)
}

func (s *session) initiateLogoutInReplyTo(reason string, inReplyTo *Message) (err error) {
	if err = s.sendLogoutInReplyTo(reason, inReplyTo); err != nil {
		s.logError(err)
		return
	}
	s.log.OnEvent("Inititated logout request")
	time.AfterFunc(s.LogoutTimeout, func() { s.sessionEvent <- internal.LogoutTimeout })
	return
}

func (s *session) verify(msg *Message) MessageRejectError {
	return s.verifySelect(msg, true, true, true)
}

func (s *session) verifyIgnoreSeqNumTooHigh(msg *Message) MessageRejectError {
	return s.verifySelect(msg, false, true, false)
}

func (s *session) verifyIgnoreSeqNumTooHighOrLow(msg *Message) MessageRejectError {
	return s.verifySelect(msg, false, false, true)
}

func (s *session) verifySelect(msg *Message, checkTooHigh bool, checkTooLow bool, checkAppImpl bool) MessageRejectError {
	if reject := s.checkBeginString(msg); reject != nil {
		return reject
	}

	if reject := s.checkCompID(msg); reject != nil {
		return reject
	}

	switch s.stateMachine.State.(type) {
	case resendState:
		//Don't check staleness of a replay
	default:
		if reject := s.checkSendingTime(msg); reject != nil {
			return reject
		}
	}
	if checkTooLow {
		if reject := s.checkTargetTooLow(msg); reject != nil {
			return reject
		}
	}

	if checkTooHigh {
		if reject := s.checkTargetTooHigh(msg); reject != nil {
			return reject
		}
	}

	if checkAppImpl {
		return s.verifyMsgAgainstAppImpl(msg)
	}

	return nil
}

func (s *session) verifyMsgAgainstAppImpl(msg *Message) MessageRejectError {
	if s.Validator != nil {
		if reject := s.Validator.Validate(msg); reject != nil {
			return reject
		}
	}

	return s.fromCallback(msg)
}

func (s *session) fromCallback(msg *Message) MessageRejectError {
	msgType, err := msg.Header.GetBytes(tagMsgType)
	if err != nil {
		return err
	}

	if isAdminMessageType(msgType) {
		return s.application.FromAdmin(msg, s.sessionID)
	}

	return s.application.FromApp(msg, s.sessionID)
}

func (s *session) checkTargetTooLow(msg *Message) MessageRejectError {
	if !msg.Header.Has(tagMsgSeqNum) {
		return RequiredTagMissing(tagMsgSeqNum)
	}

	seqNum, err := msg.Header.GetInt(tagMsgSeqNum)
	if err != nil {
		return err
	}

	if seqNum < s.store.NextTargetMsgSeqNum() {
		return targetTooLow{ReceivedTarget: seqNum, ExpectedTarget: s.store.NextTargetMsgSeqNum()}
	}

	return nil
}

func (s *session) checkTargetTooHigh(msg *Message) MessageRejectError {
	if !msg.Header.Has(tagMsgSeqNum) {
		return RequiredTagMissing(tagMsgSeqNum)
	}

	seqNum, err := msg.Header.GetInt(tagMsgSeqNum)
	if err != nil {
		return err
	}

	if seqNum > s.store.NextTargetMsgSeqNum() {
		return targetTooHigh{ReceivedTarget: seqNum, ExpectedTarget: s.store.NextTargetMsgSeqNum()}
	}

	return nil
}

func (s *session) checkCompID(msg *Message) MessageRejectError {
	senderCompID, haveSender := msg.Header.GetBytes(tagSenderCompID)
	targetCompID, haveTarget := msg.Header.GetBytes(tagTargetCompID)

	switch {
	case haveSender != nil:
		return RequiredTagMissing(tagSenderCompID)
	case haveTarget != nil:
		return RequiredTagMissing(tagTargetCompID)
	case len(targetCompID) == 0:
		return TagSpecifiedWithoutAValue(tagTargetCompID)
	case len(senderCompID) == 0:
		return TagSpecifiedWithoutAValue(tagSenderCompID)
	case s.sessionID.SenderCompID != string(targetCompID) || s.sessionID.TargetCompID != string(senderCompID):
		return compIDProblem()
	}

	return nil
}

func (s *session) checkSendingTime(msg *Message) MessageRejectError {
	if s.SkipCheckLatency {
		return nil
	}

	if ok := msg.Header.Has(tagSendingTime); !ok {
		return RequiredTagMissing(tagSendingTime)
	}

	sendingTime, err := msg.Header.GetTime(tagSendingTime)
	if err != nil {
		return err
	}

	if delta := time.Since(sendingTime); delta <= -1*s.MaxLatency || delta >= s.MaxLatency {
		return sendingTimeAccuracyProblem()
	}

	return nil
}

func (s *session) checkBeginString(msg *Message) MessageRejectError {
	switch beginString, err := msg.Header.GetBytes(tagBeginString); {
	case err != nil:
		return RequiredTagMissing(tagBeginString)
	case s.sessionID.BeginString != string(beginString):
		return incorrectBeginString{}
	}

	return nil
}

func (s *session) drainMessageIn() {
	s.log.OnEventf("Draining %d messages from inbound channel...", len(s.messageIn))
	for {
		select {
		case fixInc, ok := <-s.messageIn:
			if !ok {
				return
			}
			s.Incoming(s, fixInc)
		default:
			return
		}
	}
}

func (s *session) doReject(msg *Message, rej MessageRejectError) error {
	reply := msg.reverseRoute()

	if s.sessionID.BeginString >= BeginStringFIX42 {

		if rej.IsBusinessReject() {
			reply.Header.SetField(tagMsgType, FIXString("j"))
			reply.Body.SetField(tagBusinessRejectReason, FIXInt(rej.RejectReason()))
			if refID := rej.BusinessRejectRefID(); refID != "" {
				reply.Body.SetField(tagBusinessRejectRefID, FIXString(refID))
			}
		} else {
			reply.Header.SetField(tagMsgType, FIXString("3"))
			switch {
			default:
				reply.Body.SetField(tagSessionRejectReason, FIXInt(rej.RejectReason()))
			case rej.RejectReason() > rejectReasonInvalidMsgType && s.sessionID.BeginString == BeginStringFIX42:
				// Fix42 knows up to invalid msg type.
			}

			if refTagID := rej.RefTagID(); refTagID != nil {
				reply.Body.SetField(tagRefTagID, FIXInt(*refTagID))
			}
		}
		reply.Body.SetField(tagText, FIXString(rej.Error()))

		var msgType FIXString
		if err := msg.Header.GetField(tagMsgType, &msgType); err == nil {
			reply.Body.SetField(tagRefMsgType, msgType)
		}
	} else {
		reply.Header.SetField(tagMsgType, FIXString("3"))

		if refTagID := rej.RefTagID(); refTagID != nil {
			reply.Body.SetField(tagText, FIXString(fmt.Sprintf("%s (%d)", rej.Error(), *refTagID)))
		} else {
			reply.Body.SetField(tagText, FIXString(rej.Error()))
		}
	}

	seqNum := new(FIXInt)
	if err := msg.Header.GetField(tagMsgSeqNum, seqNum); err == nil {
		reply.Body.SetField(tagRefSeqNum, seqNum)
	}

	s.log.OnEventf("Message Rejected: %v", rej.Error())
	return s.sendInReplyTo(reply, msg)
}

type fixIn struct {
	bytes       *bytes.Buffer
	receiveTime time.Time
}

func (s *session) onDisconnect() chan struct{} {
	s.log.OnEvent("Disconnected")
	s.setApplicationSendingEnabled(false)
	if s.ResetOnDisconnect {
		if err := s.dropAndReset(); err != nil {
			s.logError(err)
		}
	}

	connectionDone := s.detachConnection()

	// s.messageIn is buffered so we need to drain it before disconnection
	s.drainMessageIn()

	s.messageIn = nil
	return connectionDone
}

func (s *session) detachConnection() chan struct{} {
	s.sendMutex.Lock()
	connection := s.connection
	connectionDone := s.connectionDone
	s.connection = nil
	s.connectionDone = nil
	s.applicationSendingEnabled = false
	s.sendMutex.Unlock()
	if connection != nil {
		_ = connection.Close()
	}
	return connectionDone
}

func (s *session) onAdmin(msg interface{}) {
	switch msg := msg.(type) {

	case connect:

		if s.IsConnected() {
			if msg.err != nil {
				msg.err <- errors.New("Already connected")
				close(msg.err)
			}
			return
		}

		if !s.IsSessionTime() {
			if connectionDone := s.handleDisconnectState(s); connectionDone != nil {
				close(connectionDone)
			}
			if msg.err != nil {
				msg.err <- errors.New("Connection outside of session time")
				close(msg.err)
			}
			return
		}

		s.messageIn = msg.messageIn
		s.sendMutex.Lock()
		s.connection = msg.connection
		s.connectionDone = msg.connectionDone
		s.applicationSendingEnabled = false
		s.sendMutex.Unlock()
		s.sentReset = false

		if err := s.Connect(s); err != nil {
			connectionDone := s.detachConnection()
			s.messageIn = nil
			if connectionDone != nil {
				close(connectionDone)
			}
			if msg.err != nil {
				msg.err <- err
				close(msg.err)
			}
			return
		}
		if msg.err != nil {
			close(msg.err)
		}

	case stopReq:
		s.Stop(s)

	case waitForInSessionReq:
		if !s.IsSessionTime() {
			msg.rep <- s.stateMachine.notifyOnInSessionTime
		}
		close(msg.rep)
	}
}

func (s *session) run() {
	s.Start(s)
	var stopChan = make(chan struct{})
	s.stateTimer = internal.NewEventTimer(func() {
		select {
		// Deadlock in write to chan s.sessionEvent after s.Stopped()==true and end of loop session.go:766 because no reader of chan s.sessionEvent.
		case s.sessionEvent <- internal.NeedHeartbeat:
		case <-stopChan:
		}
	})
	s.peerTimer = internal.NewEventTimer(func() {
		select {
		// Deadlock in write to chan s.sessionEvent after s.Stopped()==true and end of loop session.go:766 because no reader of chan s.sessionEvent.
		case s.sessionEvent <- internal.PeerTimeout:
		case <-stopChan:
		}

	})

	// Without this sleep the ticker will be aligned at the millisecond which
	// corresponds to the creation of the session. If the session creation
	// happened at 07:00:00.678 and the session StartTime is 07:30:00, any new
	// connection received between 07:30:00.000 and 07:30:00.677 will be
	// rejected. Aligning the ticker with a round second fixes that.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))

	ticker := time.NewTicker(time.Second)

	defer func() {
		close(stopChan)
		s.stateTimer.Stop()
		s.peerTimer.Stop()
		ticker.Stop()
	}()

	for !s.Stopped() {
		select {

		case msg := <-s.admin:
			s.onAdmin(msg)

		case fixIn, ok := <-s.messageIn:
			if !ok {
				s.Disconnected(s)
			} else {
				s.Incoming(s, fixIn)
			}

		case evt := <-s.sessionEvent:
			s.Timeout(s, evt)

		case now := <-ticker.C:
			s.CheckSessionTime(s, now)
			s.CheckResetTime(s, now)
		}
	}
}
