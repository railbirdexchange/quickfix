package quickfix

import (
	"bytes"
	"io"
	"time"

	"github.com/quickfixgo/quickfix/config"
)

// preparedMessageStore explicitly accepts borrowed bytes and reserves journal
// capacity without changing committed state. Other MessageStore implementations
// receive an owned copy through their unchanged public interface.
type preparedMessageStore interface {
	preparedStoreOwner() MessageStore
	reserve(int) (int, error)
	commit(int, int, []byte, bool) error
	abort(int)
	reserveReset() (int, error)
	commitReset(int, []byte) error
}

func (s *session) configureOutbound(settings *SessionSettings) error {
	depth := 32
	if settings.HasSetting(config.OutboundFrameQueueCapacity) {
		value, err := settings.IntSetting(config.OutboundFrameQueueCapacity)
		if err != nil {
			return err
		}
		depth = value
	}
	ring, err := newOutboundRing(depth, s.boundedIn.limits.MaxFrameBytes)
	if err != nil {
		return err
	}
	ring.writeTimeout = 30 * time.Second
	if settings.HasSetting(config.OutboundWriteTimeout) {
		ring.writeTimeout, err = settings.DurationSetting(config.OutboundWriteTimeout)
		if err != nil {
			return err
		}
		if ring.writeTimeout <= 0 {
			return ErrInvalidOutboundLimits
		}
	}
	ring.replay, err = NewMessageSlot(s.transportDataDictionary, s.appDataDictionary, s.boundedIn.limits)
	if err != nil {
		return err
	}
	ring.replay.enableScopedCallbacks()
	if journal, ok := s.store.(preparedMessageStore); ok && journal.preparedStoreOwner() == s.store {
		// Embedding a store promotes private methods too. A decorator must
		// continue through its public persistence and reset overrides; only
		// the actual capability owner may accept direct prepared commits.
		ring.journal = journal
	}
	ring.log, _ = s.log.(BorrowedLog)
	s.outbound = ring
	return nil
}

func (s *session) fillOutboundHeader(message *Message, inReplyTo *Message) {
	message.outboundBorrowed = applicationBorrowsMessages(s.application)
	setCanonicalIdentity(&message.Header.FieldMap, tagBeginString, s.sessionID.BeginString)
	setCanonicalIdentity(&message.Header.FieldMap, tagSenderCompID, s.sessionID.SenderCompID)
	setCanonicalIdentity(&message.Header.FieldMap, tagTargetCompID, s.sessionID.TargetCompID)
	for _, identity := range []struct {
		tag   Tag
		value string
	}{
		{tagSenderSubID, s.sessionID.SenderSubID}, {tagSenderLocationID, s.sessionID.SenderLocationID},
		{tagTargetSubID, s.sessionID.TargetSubID}, {tagTargetLocationID, s.sessionID.TargetLocationID},
	} {
		if identity.value != "" {
			setCanonicalIdentity(&message.Header.FieldMap, identity.tag, identity.value)
		}
	}
	precision := s.timestampPrecision
	if s.sessionID.BeginString < BeginStringFIX42 {
		precision = Seconds
	}
	message.setOutboundTime(time.Now().UTC(), precision)
	if s.EnableLastMsgSeqNumProcessed {
		sequence := s.store.NextTargetMsgSeqNum() - 1
		if inReplyTo != nil {
			value, reject := inReplyTo.Header.GetInt(tagMsgSeqNum)
			if reject != nil {
				s.logError(reject)
				return
			}
			sequence = value
		}
		message.setOutboundInt(&message.Header.FieldMap, outboundLastProcessed, tagLastMsgSeqNumProcessed, sequence)
	}
}

// prepareOutbound runs synchronously on the original SendToTarget caller while
// existing sendMutex/resendMutex ownership is held. It never hands the caller's
// Message to the writer. The returned slot already owns independent wire bytes.
func (s *session) prepareOutbound(message, inReplyTo *Message, replacement bool) (result *outboundSlot, err error) {
	ring := s.outbound
	slot, err := ring.reserve(replacement)
	if err != nil {
		return nil, err
	}
	defer func() {
		if result == nil {
			slot.release()
		}
	}()
	messageType, reject := message.Header.GetBytes(tagMsgType)
	if reject != nil {
		return nil, reject
	}
	logon := bytes.Equal(messageType, msgTypeLogon)
	pendingReset := ring.resetPending.Load()
	if pendingReset && !logon {
		return nil, ErrOutboundResetPending
	}
	sequence := s.store.NextSenderMsgSeqNum()
	if pendingReset {
		sequence = 1
	}
	resetStore := pendingReset
	sentResetRequested := false
	if logon {
		if reset, reject := message.Body.GetBool(tagResetSeqNumFlag); reject == nil && reset {
			resetStore = true
		}
	}
	journal := ring.journal
	reservation := -1
	reservedReset := resetStore
	if journal != nil && !s.DisableMessagePersist {
		if resetStore {
			reservation, err = journal.reserveReset()
		} else {
			reservation, err = journal.reserve(sequence)
		}
		if err != nil {
			return nil, err
		}
		defer func() { journal.abort(reservation) }()
	}
	s.fillOutboundHeader(message, inReplyTo)
	message.setOutboundInt(&message.Header.FieldMap, outboundSequence, tagMsgSeqNum, sequence)
	if isAdminMessageType(messageType) {
		started := startSendStage()
		s.application.ToAdmin(message, s.sessionID)
		s.finishSendStage("to_admin", messageType, started, true)
		if logon {
			reset := false
			if message.Body.Has(tagResetSeqNumFlag) {
				reset, reject = message.Body.GetBool(tagResetSeqNumFlag)
				if reject != nil {
					return nil, reject
				}
			}
			resetStore = pendingReset || reset
			sentResetRequested = reset
			if resetStore {
				sequence = 1
			}
			if reset {
				message.setOutboundInt(&message.Header.FieldMap, outboundSequence, tagMsgSeqNum, sequence)
			}
		}
	} else {
		started := startSendStage()
		err = s.application.ToApp(message, s.sessionID)
		s.finishSendStage("to_app", messageType, started, err == nil)
		if err != nil {
			return nil, err
		}
	}
	if reservation >= 0 && resetStore != reservedReset {
		journal.abort(reservation)
		reservation = -1
		if resetStore {
			reservation, err = journal.reserveReset()
		} else {
			reservation, err = journal.reserve(sequence)
		}
		if err != nil {
			return nil, err
		}
	}
	started := startSendStage()
	slot.wire, err = message.AppendTo(slot.wire[:0])
	if err != nil {
		s.finishSendStage("build_message", messageType, started, false)
		return nil, err
	}
	message.retainOutboundFraming(slot.wire)
	if reservation >= 0 {
		// A valid retained frame must fit the configured replay metadata as
		// well as its byte slot. This is capacity admission, not new outgoing
		// dictionary validation: legacy semantic parse errors are unchanged.
		ring.replay.Reset()
		parseError := ring.replay.Parse(slot.wire)
		ring.replay.Reset()
		if isInboundCapacityError(parseError) {
			s.finishSendStage("build_message", messageType, started, false)
			return nil, parseError
		}
	}
	s.finishSendStage("build_message", messageType, started, true)
	started = startSendStage()
	if reservation >= 0 {
		if resetStore {
			err = journal.commitReset(reservation, slot.wire)
		} else {
			err = journal.commit(reservation, sequence, slot.wire, true)
		}
	} else {
		if resetStore {
			err = s.store.Reset()
		}
		if err == nil {
			if s.DisableMessagePersist {
				err = s.store.IncrNextSenderMsgSeqNum()
			} else {
				err = s.store.SaveMessageAndIncrNextSenderMsgSeqNum(sequence, bytes.Clone(slot.wire))
			}
		}
	}
	s.finishSendStage("persist", messageType, started, err == nil)
	if err != nil {
		return nil, err
	}
	if resetStore {
		ring.resetPending.Store(false)
		ring.scheduledResetPending.Store(false)
		s.sentReset = sentResetRequested
	}
	return slot, nil
}

func (s *session) logOutgoingFrame(wire []byte) {
	if s.outbound.log != nil {
		s.outbound.log.OnOutgoingBorrowed(wire)
		return
	}
	s.log.OnOutgoing(bytes.Clone(wire))
}

func (s *session) flushOutbound(blockUntilSent bool) error {
	writer := s.writeConnection
	if writer == nil {
		if s.outbound.count != 0 {
			s.log.OnEventf("Failed to send: disconnected")
			return io.ErrClosedPipe
		}
		return nil
	}
	for s.outbound.count != 0 {
		select {
		case <-writer.done:
			return writer.failure()
		default:
		}
		started := startSendStage()
		if blockUntilSent {
			select {
			case <-writer.permits:
			case <-writer.done:
				return writer.failure()
			}
		} else {
			select {
			case <-writer.permits:
			default:
				if !started.IsZero() {
					s.observeSendStage("outbound_channel_wait", fixMsgTypeFromRaw(s.outbound.front().wire), time.Since(started), false)
				}
				return nil
			}
		}
		slot := s.outbound.pop()
		var messageType string
		if !started.IsZero() {
			messageType = fixMsgTypeFromRaw(slot.wire)
		}
		// Logging finishes while the sender still owns the slot. Generic
		// retaining loggers receive a detached copy before writer handoff.
		s.logOutgoingFrame(slot.wire)
		writer.ready <- slot
		if !started.IsZero() {
			s.observeSendStage("outbound_channel_wait", messageType, time.Since(started), true)
		}
		s.stateTimer.Reset(s.HeartBtInt)
	}
	return nil
}

// A resend can exceed the output queue depth. Its session owner waits directly
// for the independent writer, which never needs sendMutex or session dispatch
// to return a slot. A failed/closed writer always wakes this wait.
func (s *session) reserveResendOutput() (*outboundSlot, error) {
	for {
		writer := s.writeConnection
		if writer != nil {
			select {
			case <-writer.done:
				if writer.err != nil {
					return nil, writer.err
				}
				return nil, io.ErrClosedPipe
			default:
			}
		}
		if err := s.flushOutbound(false); err != nil {
			return nil, err
		}
		slot, err := s.outbound.reserve(false)
		if err == nil {
			return slot, nil
		}
		if writer == nil {
			return nil, err
		}
		select {
		case <-writer.done:
			if writer.err != nil {
				return nil, writer.err
			}
			return nil, io.ErrClosedPipe
		case <-s.messageEvent:
		}
	}
}

func (s *session) enqueueReplay(message *Message, body []byte) error {
	if s.outbound == nil {
		return s.EnqueueBytesAndSend(message.buildWithBodyBytes(body))
	}
	s.sendMutex.Lock()
	defer s.sendMutex.Unlock()
	slot, err := s.reserveResendOutput()
	if err != nil {
		return err
	}
	slot.wire, err = message.AppendWithBodyBytesTo(slot.wire[:0], body)
	if err != nil {
		slot.release()
		return err
	}
	message.retainOutboundFraming(slot.wire)
	s.outbound.enqueue(slot)
	return s.flushOutbound(true)
}

func isOutboundCapacityError(err error) bool {
	return err == ErrOutboundCapacity || err == ErrOutboundResetPending || err == ErrBufferTooSmall || err == ErrJournalFull || err == ErrJournalMessageCapacity || err == ErrJournalRangeUnavailable || isInboundCapacityError(err)
}

func (s *session) refreshStore() error {
	s.resendMutex.Lock()
	defer s.resendMutex.Unlock()
	s.sendMutex.Lock()
	defer s.sendMutex.Unlock()
	return s.store.Refresh()
}

func (s *session) stageLogonReset() {
	s.outbound.resetPending.Store(true)
}

func (s *session) finishLogonReset() error {
	s.resendMutex.Lock()
	defer s.resendMutex.Unlock()
	s.sendMutex.Lock()
	defer s.sendMutex.Unlock()
	if err := s.store.Reset(); err != nil {
		return err
	}
	s.outbound.resetPending.Store(false)
	s.outbound.scheduledResetPending.Store(false)
	return nil
}

func (s *session) logonSenderSequence() int {
	if s.outbound != nil && s.outbound.resetPending.Load() {
		return 1
	}
	return s.store.NextSenderMsgSeqNum()
}

func (s *session) logonTargetSequence() int {
	if s.outbound != nil && s.outbound.resetPending.Load() {
		return 1
	}
	return s.store.NextTargetMsgSeqNum()
}
