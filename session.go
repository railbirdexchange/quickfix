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
	"sync"
	"sync/atomic"
	"time"

	"github.com/quickfixgo/quickfix/datadictionary"
	"github.com/quickfixgo/quickfix/internal"
)

// The Session is the primary FIX abstraction for message communication.
type session struct {
	store MessageStore

	log       Log
	sessionID SessionID

	messageOut chan<- []byte
	messageIn  <-chan fixIn

	// Application messages are queued up for send here.
	toSend [][]byte

	// Mutex for access to toSend.
	sendMutex sync.Mutex
	// Mutex to prevent messages being sent when resendRequest is active
	// Must be locked before sendMutex to prevent a potential deadlock
	resendMutex sync.RWMutex

	sessionEvent          chan internal.Event
	messageEvent          chan bool
	application           Application
	builtMessageObserver  BuiltMessageObserver
	builtMessagesBorrowed bool
	Validator
	stateMachine
	stateTimer                            *internal.EventTimer
	peerTimer                             *internal.EventTimer
	sentReset                             bool
	stopOnce                              sync.Once
	retired                               bool // Protected by sendMutex; only terminal dynamic retirement sets it.
	reservation                           *sessionReservation
	storeClosed                           bool // Construction/retirement owner only.
	storeCloseErr                         error
	runStop                               <-chan struct{}
	logonTimeoutTimer, logoutTimeoutTimer *time.Timer

	targetDefaultApplVerID string

	admin chan interface{}
	internal.SessionSettings
	transportDataDictionary *datadictionary.DataDictionary
	appDataDictionary       *datadictionary.DataDictionary
	boundedIn               *boundedInboundSession
	readConnection          *boundedReadConnection
	outbound                *outboundRing
	writeConnection         *outboundWriter
	outboundLoggedOn        atomic.Bool // Published by the session owner; external senders must not read State.
	preserveOnDisconnect    bool
	controls                scopedControl

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
	messageOut chan<- []byte
	messageIn  <-chan fixIn
	err        chan<- error
	reader     *boundedReadConnection
	writer     *outboundWriter
}

func (s *session) connectWithWriter(msgIn <-chan fixIn, reader *boundedReadConnection, writer *outboundWriter) error {
	return s.connectWithIO(msgIn, nil, reader, writer)
}

func (s *session) connectWithIO(msgIn <-chan fixIn, msgOut chan<- []byte, reader *boundedReadConnection, writer *outboundWriter) error {
	rep := make(chan error)
	s.admin <- connect{
		messageOut: msgOut,
		messageIn:  msgIn,
		err:        rep,
		reader:     reader,
		writer:     writer,
	}

	return <-rep
}

type stopReq struct{}

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
	if s.outbound != nil && s.outbound.resetPending.Load() {
		return true
	}

	return (s.ResetOnLogon || s.ResetOnDisconnect || s.ResetOnLogout) &&
		s.store.NextTargetMsgSeqNum() == 1 && s.store.NextSenderMsgSeqNum() == 1
}

func (s *session) sendLogon() error {
	return s.sendLogonInReplyTo(s.shouldSendReset(), nil)
}

func (s *session) sendLogonInReplyTo(setResetSeqNum bool, inReplyTo *Message) error {
	logon, err := s.acquireControl("A")
	if err != nil {
		return err
	}
	defer s.releaseControl()
	logon.Header.SetString(tagBeginString, s.sessionID.BeginString)
	logon.Header.SetString(tagTargetCompID, s.sessionID.TargetCompID)
	logon.Header.SetString(tagSenderCompID, s.sessionID.SenderCompID)
	logon.Body.SetString(tagEncryptMethod, "0")
	logon.Body.SetInt(tagHeartBtInt, int(s.HeartBtInt.Seconds()))

	if setResetSeqNum {
		logon.Body.SetBool(tagResetSeqNumFlag, true)
	}

	if len(s.DefaultApplVerID) > 0 {
		logon.Body.SetString(tagDefaultApplVerID, s.DefaultApplVerID)
	}

	// Evaluate tag 789.
	if s.EnableNextExpectedMsgSeqNum {
		if inReplyTo != nil {
			if inReplyTo.Body.Has(tagNextExpectedMsgSeqNum) {
				targetWantsNextSeqNumToBe, getErr := inReplyTo.Body.GetInt(tagNextExpectedMsgSeqNum)
				if getErr == nil {
					actualNextNum := s.logonSenderSequence()
					// Is the 789 we received too high?
					if targetWantsNextSeqNumToBe > actualNextNum {
						// We cannot resend a sequence that was never sent.
						return RejectLogon{fmt.Sprintf("Tag 789 (NextExpectedMsgSeqNum) is higher than expected. Expected %d, Received %d", actualNextNum, targetWantsNextSeqNumToBe)}
					}
					nextSeqNum := s.logonTargetSequence()
					logon.Body.SetInt(tagNextExpectedMsgSeqNum, nextSeqNum+1)
				}
			}
		} else {
			// We are sending a logon.
			nextseqnum := s.logonTargetSequence()
			logon.Body.SetInt(tagNextExpectedMsgSeqNum, nextseqnum+1)
		}
	}

	if err := s.dropAndSendInReplyTo(logon, inReplyTo); err != nil {
		return err
	}

	return nil
}

func (s *session) generateSequenceReset(beginSeqNo int, endSeqNo int, inReplyTo *Message) (err error) {
	sequenceReset, err := s.acquireControl("4")
	if err != nil {
		return err
	}
	defer s.releaseControl()
	if s.outbound != nil {
		s.fillOutboundHeader(sequenceReset, inReplyTo)
	} else {
		s.fillDefaultHeader(sequenceReset, inReplyTo)
	}
	sequenceReset.Header.SetInt(tagMsgSeqNum, beginSeqNo)
	sequenceReset.Header.SetBool(tagPossDupFlag, true)
	sequenceReset.Body.SetInt(tagNewSeqNo, endSeqNo)
	sequenceReset.Body.SetBool(tagGapFillFlag, true)
	if origSendingTime, present := sequenceReset.Header.getBytesIfPresent(tagSendingTime); present {
		setControlBytes(&sequenceReset.Header.FieldMap, tagOrigSendingTime, origSendingTime)
	}

	sequenceReset, err = s.toAdmin(sequenceReset)
	if err != nil {
		return err
	}

	if err = s.enqueueControl(sequenceReset); err != nil {
		return
	}
	s.logSequenceEvent(eventSentSequenceReset, endSeqNo, 0)

	return
}

func (s *session) buildLogout(reason string) (*Message, error) {
	logout, err := s.acquireControl("5")
	if err != nil {
		return nil, err
	}
	logout.Header.SetString(tagBeginString, s.sessionID.BeginString)
	logout.Header.SetString(tagTargetCompID, s.sessionID.TargetCompID)
	logout.Header.SetString(tagSenderCompID, s.sessionID.SenderCompID)
	if reason != "" {
		logout.Body.SetString(tagText, reason)
	}

	return logout, nil
}

func (s *session) sendLogout(reason string) error {
	return s.sendLogoutInReplyTo(reason, nil)
}

func (s *session) sendLogoutInReplyTo(reason string, inReplyTo *Message) error {
	logout, err := s.buildLogout(reason)
	if err != nil {
		return err
	}
	defer s.releaseControl()
	return s.sendInReplyTo(logout, inReplyTo)
}

func (s *session) dropAndSendLogoutInReplyTo(reason string, inReplyTo *Message) error {
	logout, err := s.buildLogout(reason)
	if err != nil {
		return err
	}
	defer s.releaseControl()
	return s.dropAndSendInReplyTo(logout, inReplyTo)
}

func (s *session) resend(msg *Message) bool {
	if s.outbound != nil {
		msg.outboundBorrowed = applicationBorrowsMessages(s.application)
		msg.setOutboundBool(&msg.Header.FieldMap, outboundPossDup, tagPossDupFlag, true)
		if original, found := msg.Header.getBytesIfPresent(tagSendingTime); found {
			msg.setOutboundBytes(&msg.Header.FieldMap, outboundOriginalTimestamp, tagOrigSendingTime, original)
		}
		precision := s.timestampPrecision
		if s.sessionID.BeginString < BeginStringFIX42 {
			precision = Seconds
		}
		msg.setOutboundTime(time.Now().UTC(), precision)
		return s.application.ToApp(msg, s.sessionID) == nil
	}
	msg.Header.SetField(tagPossDupFlag, FIXBoolean(true))

	var origSendingTime FIXString
	if err := msg.Header.GetField(tagSendingTime, &origSendingTime); err == nil {
		msg.Header.SetField(tagOrigSendingTime, origSendingTime)
	}

	s.insertSendingTime(msg)

	return s.application.ToApp(msg, s.sessionID) == nil
}

// queueForSend will validate, persist, and queue the message for send.
func (s *session) queueForSend(msg *Message) error {
	// resendMutex must always be locked before sendMutex to prevent a potential deadlock.
	// Makes sure that live traffic cannot interleave with replayed messages while processing a ResendRequest.
	s.resendMutex.RLock()
	defer s.resendMutex.RUnlock()

	lockStartedAt := startSendStage()
	s.sendMutex.Lock()
	var lockedAt time.Time
	if !lockStartedAt.IsZero() {
		lockedAt = time.Now()
	}
	defer func() {
		var held time.Duration
		if !lockedAt.IsZero() {
			held = time.Since(lockedAt)
		}
		s.sendMutex.Unlock()
		if !lockedAt.IsZero() {
			s.observeSendStage("queue_send_lock_hold", "", held, true)
		}
	}()
	if !lockedAt.IsZero() {
		s.observeSendStage("queue_send_lock_wait", "", lockedAt.Sub(lockStartedAt), true)
	}
	if s.retired {
		return errUnknownSession
	}
	if s.outbound != nil {
		slot, err := s.prepareOutbound(msg, nil, false)
		if err != nil {
			return err
		}
		s.outbound.enqueue(slot)
		s.notifyMessageOut()
		return nil
	}

	msgBytes, err := s.prepMessageForSend(msg, nil)
	if err != nil {
		return err
	}

	s.toSend = append(s.toSend, msgBytes)

	s.notifyMessageOut()

	return nil
}

func (s *session) notifyMessageOut() {
	select {
	case s.messageEvent <- true:
	default:
	}
}

// send will validate, persist, queue the message. If the session is logged on, send all messages in the queue.
func (s *session) send(msg *Message) error {
	return s.sendInReplyTo(msg, nil)
}
func (s *session) sendInReplyTo(msg *Message, inReplyTo *Message) error {
	if !s.IsLoggedOn() {
		return s.queueForSend(msg)
	}

	// resendMutex must always be locked before sendMutex to prevent a potential deadlock
	s.resendMutex.RLock()
	defer s.resendMutex.RUnlock()

	lockStartedAt := startSendStage()
	s.sendMutex.Lock()
	s.finishSendStage("send_mutex_wait", nil, lockStartedAt, true)
	defer s.sendMutex.Unlock()
	if s.retired {
		return errUnknownSession
	}
	if s.outbound != nil {
		slot, err := s.prepareOutbound(msg, inReplyTo, false)
		if err != nil {
			return err
		}
		s.outbound.enqueue(slot)
		return s.flushOutbound(true)
	}

	msgBytes, err := s.prepMessageForSend(msg, inReplyTo)
	if err != nil {
		return err
	}

	s.toSend = append(s.toSend, msgBytes)
	s.sendQueued(true)

	return nil
}

// dropAndReset will drop the send queue and reset the message store.
func (s *session) dropAndReset() error {
	s.resendMutex.Lock()
	defer s.resendMutex.Unlock()
	s.sendMutex.Lock()
	defer s.sendMutex.Unlock()
	if s.retired {
		return errUnknownSession
	}

	s.dropQueued()
	if err := s.store.Reset(); err != nil {
		return err
	}
	if s.outbound != nil {
		s.outbound.resetPending.Store(false)
		s.outbound.scheduledResetPending.Store(false)
	}
	return nil
}

// dropAndSend will validate and persist the message, then drops the send queue and sends the message.
func (s *session) dropAndSend(msg *Message) error {
	return s.dropAndSendInReplyTo(msg, nil)
}
func (s *session) dropAndSendInReplyTo(msg *Message, inReplyTo *Message) error {
	s.resendMutex.Lock()
	defer s.resendMutex.Unlock()
	s.sendMutex.Lock()
	defer s.sendMutex.Unlock()
	if s.retired {
		return errUnknownSession
	}
	if s.outbound != nil {
		slot, err := s.prepareOutbound(msg, inReplyTo, true)
		if err != nil {
			return err
		}
		s.dropQueued()
		s.outbound.enqueue(slot)
		return s.flushOutbound(true)
	}

	msgBytes, err := s.prepMessageForSend(msg, inReplyTo)
	if err != nil {
		return err
	}

	s.dropQueued()
	s.toSend = append(s.toSend, msgBytes)
	s.sendQueued(true)

	return nil
}

// retireDynamic runs only after the dynamic session loop has returned, or before
// an unstarted session has transferred to it. Socket ownership has already joined.
// Mark terminal before dropping routing so callers holding an earlier lookup
// cannot persist into a closed store. Close runs without engine locks.
func (s *session) retireDynamic() error {
	s.resendMutex.Lock()
	s.sendMutex.Lock()
	s.retired = true
	s.dropQueued()
	s.sendMutex.Unlock()
	s.resendMutex.Unlock()
	unregisterSessionInstance(s)
	return s.closeStore()
}

func (s *session) closeStore() error {
	if !s.storeClosed && s.store != nil {
		s.storeClosed = true
		s.storeCloseErr = s.store.Close()
	}
	return s.storeCloseErr
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
		toAdminStartedAt := time.Now()
		msg, err = s.toAdmin(msg)
		if err != nil {
			return
		}
		s.observeSendStage("to_admin", string(msgType), time.Since(toAdminStartedAt), true)
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
		toAppStartedAt := time.Now()
		if err = s.application.ToApp(msg, s.sessionID); err != nil {
			s.observeSendStage("to_app", string(msgType), time.Since(toAppStartedAt), false)
			return
		}
		s.observeSendStage("to_app", string(msgType), time.Since(toAppStartedAt), true)
	}

	// Message converted to bytes here.
	buildStartedAt := time.Now()
	msgBytes = msg.build()
	s.observeSendStage("build_message", string(msgType), time.Since(buildStartedAt), true)
	persistStartedAt := time.Now()
	err = s.persist(seqNum, msgBytes)
	s.observeSendStage("persist", string(msgType), time.Since(persistStartedAt), err == nil)
	if err == nil {
		s.notifyMessageBuilt(msg, msgBytes)
	}

	return
}

// Resolve the optional hook during setup. Repeating an interface assertion on
// the send path can allocate Go's probabilistically rebuilt type-assert cache,
// including when the application does not implement the observer.
func (s *session) setApplication(application Application) {
	s.application = application
	borrowed, scoped := application.(borrowedApplication)
	if scoped {
		application = borrowed.Application
	}
	s.builtMessageObserver, _ = application.(BuiltMessageObserver)
	s.builtMessagesBorrowed = scoped
}

func (s *session) notifyMessageBuilt(msg *Message, msgBytes []byte) {
	observer := s.builtMessageObserver
	if observer == nil {
		return
	}
	if s.builtMessagesBorrowed {
		scope := beginBorrowedOutboundCopy(msg)
		defer scope.release()
	} else if s.outbound != nil {
		// The writer reuses bounded wire slots after completion. An ordinary
		// observer retains independent bytes, matching the pre-slot API.
		msgBytes = bytes.Clone(msgBytes)
	}
	observer.OnMessageBuilt(s.sessionID, msg, msgBytes[:len(msgBytes):len(msgBytes)])
}

func (s *session) persist(seqNum int, msgBytes []byte) error {
	if !s.DisableMessagePersist {
		return s.store.SaveMessageAndIncrNextSenderMsgSeqNum(seqNum, msgBytes)
	}

	return s.store.IncrNextSenderMsgSeqNum()
}

func (s *session) sendQueued(blockUntilSent bool) {
	if s.outbound != nil {
		s.flushOutbound(blockUntilSent)
		return
	}
	for i, msgBytes := range s.toSend {
		if !s.sendBytes(msgBytes, blockUntilSent) {
			s.toSend = s.toSend[i:]
			s.notifyMessageOut()
			return
		}
	}

	s.dropQueued()
}

func (s *session) dropQueued() {
	if s.outbound != nil {
		s.outbound.dropPending()
		return
	}
	s.toSend = s.toSend[:0]
}

func (s *session) EnqueueBytesAndSend(msg []byte) error {
	s.sendMutex.Lock()
	defer s.sendMutex.Unlock()
	if s.outbound != nil {
		slot, err := s.reserveResendOutput()
		if err != nil {
			return err
		}
		if len(msg) > cap(slot.wire) {
			slot.release()
			return ErrBufferTooSmall
		}
		slot.wire = append(slot.wire[:0], msg...)
		s.outbound.enqueue(slot)
		return s.flushOutbound(true)
	}

	s.toSend = append(s.toSend, msg)
	s.sendQueued(true)
	return nil
}

func (s *session) sendBytes(msg []byte, blockUntilSent bool) bool {
	if s.messageOut == nil {
		s.log.OnEventf("Failed to send: disconnected")
		return false
	}

	if blockUntilSent {
		channelStartedAt := time.Now()
		s.messageOut <- msg
		s.observeSendStage("outbound_channel_wait", fixMsgTypeFromRaw(msg), time.Since(channelStartedAt), true)
		s.log.OnOutgoing(msg)
		s.stateTimer.Reset(s.HeartBtInt)
		return true
	}

	channelStartedAt := time.Now()
	select {
	case s.messageOut <- msg:
		s.observeSendStage("outbound_channel_wait", fixMsgTypeFromRaw(msg), time.Since(channelStartedAt), true)
		s.log.OnOutgoing(msg)
		s.stateTimer.Reset(s.HeartBtInt)
		return true
	default:
		s.observeSendStage("outbound_channel_wait", fixMsgTypeFromRaw(msg), time.Since(channelStartedAt), false)
		return false
	}
}

func (s *session) observeQueueDepth() {
	if queueDepthObserverState.Load() == nil {
		return
	}
	pending := len(s.toSend)
	if s.outbound != nil {
		pending = s.outbound.count
	}
	observeQueueDepth(QueueDepthEvent{
		SessionID: s.sessionID,
		ToSend:    pending,
		MessageIn: len(s.messageIn),
	})
}

func (s *session) observeSendStage(stage, msgType string, duration time.Duration, success bool) {
	observeSendStage(SendStageEvent{
		SessionID: s.sessionID,
		MsgType:   msgType,
		Stage:     stage,
		Duration:  duration,
		Success:   success,
	})
}

func (s *session) doTargetTooHigh(reject targetTooHigh) (nextState resendState, err error) {
	s.logSequenceEvent(eventSequenceTooHigh, reject.ExpectedTarget, reject.ReceivedTarget)
	return s.sendResendRequest(reject.ExpectedTarget, reject.ReceivedTarget-1)
}

func (s *session) sendResendRequest(beginSeq, endSeq int) (nextState resendState, err error) {
	nextState.resendRangeEnd = endSeq

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
	resend, err := s.acquireControl("2")
	if err != nil {
		return nextState, err
	}
	defer s.releaseControl()
	resend.Body.SetInt(tagBeginSeqNo, beginSeq)
	resend.Body.SetInt(tagEndSeqNo, endSeqNo)
	if err = s.send(resend); err != nil {
		return
	}
	s.logSequenceEvent(eventSentResendRequest, beginSeq, endSeqNo)

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

	resetStore := s.outbound != nil && s.outbound.scheduledResetPending.Load()
	if s.InitiateLogon {
		s.log.OnEvent("Received logon response")
	} else {
		s.log.OnEvent("Received logon request")
		resetStore = resetStore || s.ResetOnLogon

		if s.RefreshOnLogon {
			if err := s.refreshStore(); err != nil {
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
		if s.outbound != nil {
			s.stageLogonReset()
		} else {
			if err := s.store.Reset(); err != nil {
				return err
			}
		}
	}

	// Verify seq num too high but dont check against app implementation since we just did that.
	// Don't need to double check.
	if err := s.verifyIgnoreSeqNumTooHigh(msg); err != nil {
		return err
	}
	if resetStore && s.InitiateLogon && s.outbound != nil {
		if err := s.finishLogonReset(); err != nil {
			return err
		}
	}

	if !s.InitiateLogon {
		if !s.HeartBtIntOverride {
			var heartBtInt FIXInt
			if err := msg.Body.GetField(tagHeartBtInt, &heartBtInt); err == nil {
				s.sendMutex.Lock()
				s.HeartBtInt = time.Duration(heartBtInt) * time.Second
				s.sendMutex.Unlock()
			}
		}

		s.log.OnEvent("Responding to logon request")
		scheduledReset := s.outbound != nil && s.outbound.scheduledResetPending.Load()
		if err := s.sendLogonInReplyTo(resetSeqNumFlag.Bool() || scheduledReset, msg); err != nil {
			return err
		}
	}
	s.sentReset = false

	s.peerTimer.Reset(time.Duration(float64(1.2) * float64(s.HeartBtInt)))
	s.application.OnLogon(s.sessionID)

	// Evaluate tag 789 to see if we end up with an implied gapfill/resend.
	if s.EnableNextExpectedMsgSeqNum && !msg.Body.Has(tagResetSeqNumFlag) {
		targetWantsNextSeqNumToBe, getErr := msg.Body.GetInt(tagNextExpectedMsgSeqNum)
		if getErr == nil {
			if targetWantsNextSeqNumToBe != nextSenderMsgNumAtLogonReceived {
				if !s.DisableMessagePersist {
					seqResetErr := s.generateSequenceReset(targetWantsNextSeqNumToBe, nextSenderMsgNumAtLogonReceived+1, msg)
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

	return s.store.IncrNextTargetMsgSeqNum()
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
	if s.logoutTimeoutTimer != nil {
		s.logoutTimeoutTimer.Stop()
	}
	s.logoutTimeoutTimer = s.scheduleTimeout(s.LogoutTimeout, internal.LogoutTimeout)
	return
}

func (s *session) scheduleTimeout(delay time.Duration, event internal.Event) *time.Timer {
	// Capture only this run's channels, never the session or a later run's stop
	// channel. A callback already firing must be able to leave after run exits.
	events, stop := s.sessionEvent, s.runStop
	return time.AfterFunc(delay, func() { deliverSessionTimeout(events, stop, event) })
}

func deliverSessionTimeout(events chan<- internal.Event, stop <-chan struct{}, event internal.Event) {
	select {
	case events <- event:
	case <-stop:
	}
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
	case resendState, *resendState:
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
	if !applicationBorrowsMessages(s.application) {
		// Do not publish an engine slot reference with the owned message. The
		// inbound dispatcher retains ownership until protocol handling finishes.
		msg.inbound = nil
	}
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

	expected := s.store.NextTargetMsgSeqNum()
	if s.outbound != nil && s.outbound.resetPending.Load() {
		if messageType, present := msg.Header.getBytesIfPresent(tagMsgType); present && bytes.Equal(messageType, msgTypeLogon) {
			expected = 1
		}
	}
	if seqNum < expected {
		return s.sequenceTooLow(seqNum, expected)
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
		return s.sequenceTooHigh(seqNum, s.store.NextTargetMsgSeqNum())
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
	s.logSequenceEvent(eventDrainingInbound, len(s.messageIn), 0)
	for {
		select {
		case message, ok := <-s.messageIn:
			if !ok {
				return
			}
			if message.slot != nil {
				message.slot.release()
			}
		default:
			return
		}
	}
}

func (s *session) doReject(msg *Message, rej MessageRejectError) error {
	reply, err := s.acquireControl("3")
	if err != nil {
		return err
	}
	defer s.releaseControl()
	reverseControlRoute(msg, reply)

	if s.sessionID.BeginString >= BeginStringFIX42 {

		if rej.IsBusinessReject() {
			reply.Header.SetString(tagMsgType, "j")
			reply.Body.SetInt(tagBusinessRejectReason, rej.RejectReason())
			if refID := rej.BusinessRejectRefID(); refID != "" {
				reply.Body.SetString(tagBusinessRejectRefID, refID)
			}
		} else {
			switch {
			default:
				reply.Body.SetInt(tagSessionRejectReason, rej.RejectReason())
			case rej.RejectReason() > rejectReasonInvalidMsgType && s.sessionID.BeginString == BeginStringFIX42:
				// Fix42 knows up to invalid msg type.
			}

			if refTagID, present := rej.RefTagID(); present {
				reply.Body.SetInt(tagRefTagID, int(refTagID))
			}
		}
		reply.Body.SetString(tagText, rej.Error())

		if msgType, present := msg.Header.getBytesIfPresent(tagMsgType); present {
			setControlBytes(&reply.Body.FieldMap, tagRefMsgType, msgType)
		}
	} else {
		if refTagID, present := rej.RefTagID(); present {
			if err := s.setControlRejectText(reply, rej.Error(), refTagID); err != nil {
				return err
			}
		} else {
			reply.Body.SetString(tagText, rej.Error())
		}
	}

	if msg.Header.Has(tagMsgSeqNum) {
		if seqNum, reject := msg.Header.GetInt(tagMsgSeqNum); reject == nil {
			reply.Body.SetInt(tagRefSeqNum, seqNum)
		}
	}

	s.logControlReject(rej.Error())
	return s.sendInReplyTo(reply, msg)
}

type fixIn struct {
	bytes       *bytes.Buffer
	receiveTime time.Time
	slot        *inboundSlot
	// With a slot, readError marks a consumed malformed frame to log and drop.
	// Without a slot, it is a terminal reader error.
	readError error
}

func (s *session) onDisconnect() {
	s.outboundLoggedOn.Store(false)
	s.log.OnEvent("Disconnected")
	if s.readConnection != nil {
		// Stop reads and blocked channel sends, then reclaim every transferred
		// slot before another connection can use this session's ring.
		s.readConnection.stopAndDrain(s.messageIn)
		s.readConnection = nil
	}
	if s.boundedIn != nil {
		s.boundedIn.ring.clearStash()
	}
	s.sendMutex.Lock()
	if s.writeConnection != nil {
		if err := s.writeConnection.finish(); err != nil {
			s.preserveOnDisconnect = true
			s.logError(err)
		}
		s.writeConnection = nil
	}
	if s.outbound != nil {
		s.outbound.dropPending()
		s.outbound.resetPending.Store(false)
	}
	if s.messageOut != nil {
		close(s.messageOut)
		s.messageOut = nil
	}
	s.sendMutex.Unlock()
	if s.ResetOnDisconnect && !s.preserveOnDisconnect {
		if err := s.dropAndReset(); err != nil {
			s.logError(err)
		}
	}
	s.preserveOnDisconnect = false

	// s.messageIn is buffered so we need to drain it before disconnection
	s.drainMessageIn()

	s.messageIn = nil
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
			s.handleDisconnectState(s)
			if msg.err != nil {
				msg.err <- errors.New("Connection outside of session time")
				close(msg.err)
			}
			return
		}

		if msg.err != nil {
			close(msg.err)
		}

		s.messageIn = msg.messageIn
		s.sendMutex.Lock()
		s.outboundLoggedOn.Store(false)
		s.messageOut = msg.messageOut
		s.writeConnection = msg.writer
		s.sendMutex.Unlock()
		s.readConnection = msg.reader
		s.sentReset = false

		s.Connect(s)
		if msg.reader != nil && !s.IsConnected() {
			// Initiator refresh/reset/logon preparation can fail before Connect
			// installs a connected state. Reclaim the accepted controller even
			// though a latent-to-latent transition would not run teardown.
			s.onDisconnect()
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
	var stopChan = make(chan struct{})
	s.runStop = stopChan
	s.Start(s)
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
		if s.logonTimeoutTimer != nil {
			s.logonTimeoutTimer.Stop()
		}
		if s.logoutTimeoutTimer != nil {
			s.logoutTimeoutTimer.Stop()
		}
		s.stateTimer.Stop()
		s.peerTimer.Stop()
		ticker.Stop()
	}()

	for !s.Stopped() {
		select {

		case msg := <-s.admin:
			s.onAdmin(msg)

		case <-s.messageEvent:
			s.SendAppMessages(s)

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
