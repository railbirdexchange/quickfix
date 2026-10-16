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
	if err := s.controls.init(s, s.boundedIn.limits); err != nil {
		return err
	}
	s.outbound = ring
	return nil
}

func (s *session) fillOutboundHeader(message *Message, inReplyTo *Message) {
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
	if err := message.constructionError(); err != nil {
		return nil, err
	}
	// A second ordinary send establishes a reusable caller scope. Avoid imposing
	// a full fixed arena on single-use legacy builders. Incoming parsed slots
	// already have their own session-owned storage and reset lifecycle.
	if message.arena != nil || message.parseSlot == nil && !message.Header.scoped && !message.Body.scoped && !message.Trailer.scoped &&
		message.outboundOwner == message {
		if err := message.attachMessageArena(s.boundedIn.limits.MaxFrameBytes, s.boundedIn.limits.MaxFields); err != nil {
			return nil, err
		}
	}
	ring := s.outbound
	var slot *outboundSlot
	if replacement {
		// Replacement must not flush the pending bytes it intends to discard.
		slot, err = ring.reserve(true)
	} else {
		slot, err = s.reserveOrdinaryOutput(message)
	}
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
	admin := isAdminMessageType(messageType)
	// Header construction can move arena bytes. An enabled observer retains an
	// owned snapshot; disabled observation needs only the booleans above.
	if sendStageObserverState.enabled.Load() {
		messageType = bytes.Clone(messageType)
	} else {
		messageType = nil
	}
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
	if logon && message.Body.Has(tagResetSeqNumFlag) {
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
	if admin {
		started := startSendStage()
		s.application.ToAdmin(message, s.sessionID)
		s.finishPreparedSendStage("to_admin", messageType, started, true)
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
		s.finishPreparedSendStage("to_app", messageType, started, err == nil)
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
		s.finishPreparedSendStage("build_message", messageType, started, false)
		return nil, err
	}
	message.retainOutboundFraming(slot.wire)
	if err := message.constructionError(); err != nil {
		return nil, err
	}
	if reservation >= 0 {
		// A valid retained frame must fit the configured replay metadata as
		// well as its byte slot. This is capacity admission, not new outgoing
		// dictionary validation: legacy semantic parse errors are unchanged.
		ring.replay.Reset()
		parseError := ring.replay.Parse(slot.wire)
		ring.replay.Reset()
		if isInboundCapacityError(parseError) {
			s.finishPreparedSendStage("build_message", messageType, started, false)
			return nil, parseError
		}
	}
	s.finishPreparedSendStage("build_message", messageType, started, true)
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
	s.finishPreparedSendStage("persist", messageType, started, err == nil)
	if err != nil {
		return nil, err
	}
	if resetStore {
		ring.resetPending.Store(false)
		ring.scheduledResetPending.Store(false)
		s.sentReset = sentResetRequested
	}
	message.outboundOwner = message
	return slot, nil
}

// reserveOrdinaryOutput preserves bounded admission while applying socket
// backpressure to logged-on ordinary sends. The existing sendMutex protects the
// pending queue and connection, and resendMutex preserves replay ordering. The
// independent writer returns slots without either lock or session dispatch.
func (s *session) reserveOrdinaryOutput(message *Message) (*outboundSlot, error) {
	ring, writer := s.outbound, s.writeConnection
	if writer == nil || !s.outboundLoggedOn.Load() {
		// Waiting for Logon/session dispatch from its own callback would
		// deadlock. Offline and pre-logon queues keep explicit finite limits.
		return ring.reserve(false)
	}
	select {
	case <-writer.failed:
		return nil, writer.failure()
	case <-writer.done:
		return nil, writer.failure()
	default:
	}
	slot, err := ring.reserve(false)
	if err != nil {
		// Do not flush old traffic while a reset is pending, or wait for
		// output when retention already cannot admit this send. Preflight and
		// abort leave reservation bookkeeping unchanged during the wait.
		if ring.resetPending.Load() || ring.scheduledResetPending.Load() {
			return nil, ErrOutboundResetPending
		}
		messageType, reject := message.Header.GetBytes(tagMsgType)
		if reject != nil {
			return nil, reject
		}
		if bytes.Equal(messageType, msgTypeLogon) && message.Body.Has(tagResetSeqNumFlag) {
			if reset, _ := message.Body.GetBool(tagResetSeqNumFlag); reset {
				// Reset/replacement admission must not flush bytes it intends to
				// discard. Keep its original immediate capacity boundary.
				return nil, ErrOutboundCapacity
			}
		}
		if journal := ring.journal; journal != nil && !s.DisableMessagePersist {
			reservation, err := journal.reserve(s.store.NextSenderMsgSeqNum())
			if err != nil {
				return nil, err
			}
			journal.abort(reservation)
		}
		// A full pending queue can still have a vacant progress slot. Drain
		// pending entries first so acquiring that slot can safely enqueue.
		if err := s.flushOutbound(true); err != nil {
			return nil, err
		}
		select {
		case slot = <-ring.vacant:
		case <-writer.failed:
			return nil, writer.failure()
		case <-writer.done:
			return nil, writer.failure()
		}
	}
	// Failure is published before the writer returns its slot, independently
	// of done, which remains the full connection-owner join barrier.
	select {
	case <-writer.failed:
		slot.release()
		return nil, writer.failure()
	case <-writer.done:
		slot.release()
		return nil, writer.failure()
	default:
		return slot, nil
	}
}

func (s *session) finishPreparedSendStage(stage string, messageType []byte, started time.Time, success bool) {
	if messageType != nil {
		s.finishSendStage(stage, messageType, started, success)
	}
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
		case <-writer.failed:
			return writer.failure()
		case <-writer.done:
			return writer.failure()
		default:
		}
		started := startSendStage()
		if blockUntilSent {
			select {
			case <-writer.permits:
			case <-writer.failed:
				return writer.failure()
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
		select {
		case <-writer.failed:
			return writer.failure()
		default:
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
			case <-writer.failed:
				return nil, writer.failure()
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
		case <-writer.failed:
			return nil, writer.failure()
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
	return err == ErrOutboundCapacity || err == ErrOutboundResetPending || err == ErrBufferTooSmall || err == ErrJournalFull || err == ErrJournalMessageCapacity || err == ErrJournalRangeUnavailable || err == errMessageArenaLimits || isInboundCapacityError(err)
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
