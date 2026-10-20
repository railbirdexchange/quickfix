package quickfix

import (
	"bytes"
	"strconv"
)

// scopedControl is owned by the session's processing scope. A control remains
// acquired through its synchronous application callback and encoding; the
// writer receives an independent output slot. Nested acquisition fails instead
// of overwriting the active callback's Message or allocating another builder.
type scopedControl struct {
	message  *Message
	original Message
	text     []byte
	events   []byte
	busy     bool
	resend   scopedResend
}

func (control *scopedControl) init(session *session, limits ParseLimits) error {
	message := NewMessage()
	if err := message.attachMessageArena(limits.MaxFrameBytes, limits.MaxFields); err != nil {
		return err
	}
	if session.boundedIn != nil {
		message.useFieldMetadata(session.boundedIn.metadata)
	}
	message.outboundFields = &outboundFields{owner: message}
	control.message = message
	control.original = *message
	control.text = make([]byte, limits.MaxFrameBytes)
	control.events = make([]byte, limits.MaxFrameBytes+len("Message Rejected: "))
	control.resend.session = session
	control.resend.visit = control.resend.process
	return nil
}

func (s *session) acquireControl(messageType string) (*Message, error) {
	control := &s.controls
	if control.busy {
		return nil, ErrOutboundCapacity
	}
	if control.message == nil {
		// Internal legacy session fixtures may not pass through the factory.
		// Real sessions initialize this storage before processing begins.
		limits := DefaultParseLimits()
		if s.boundedIn != nil {
			limits = s.boundedIn.limits
		}
		if err := control.init(s, limits); err != nil {
			return nil, err
		}
	}
	control.busy = true
	message := control.message
	// Public callbacks may replace a section or the whole Message. Restore the
	// physical control owner before clearing anything, including arena records
	// whose section pointers address this Message. Never recycle a replacement's
	// caller-owned maps, parse slot, frame or engine buffers.
	*message = control.original
	message.Header.Clear()
	message.Body.Clear()
	message.Trailer.Clear()
	message.Header.SetString(tagMsgType, messageType)
	if err := message.constructionError(); err != nil {
		control.busy = false
		return nil, err
	}
	return message, nil
}

func (s *session) releaseControl() {
	// Keep the last callback view intact until the next acquisition. Retained
	// copies made with CopyInto own their bytes independently of this cell.
	s.controls.busy = false
}

// toAdmin keeps the control cell private unless the application explicitly
// borrows callbacks. Continue sending the delivered representation so callback
// field/section replacements and its retained pointer describe the same wire.
func (s *session) toAdmin(message *Message) (*Message, error) {
	if err := message.constructionError(); err != nil {
		return nil, err
	}
	if message == s.controls.message && !applicationBorrowsMessages(s.application) {
		owned := NewMessage()
		message.CopyInto(owned)
		message = owned
	}
	s.application.ToAdmin(message, s.sessionID)
	return message, nil
}

// enqueueControl preserves a gap fill's explicit sequence and bypasses normal
// sender-sequence assignment/persistence just as EnqueueBytesAndSend did. Its
// body is encoded directly into the reserved output slot rather than build().
func (s *session) enqueueControl(message *Message) error {
	if err := message.constructionError(); err != nil {
		return err
	}
	if s.outbound == nil {
		return s.EnqueueBytesAndSend(message.build())
	}
	s.sendMutex.Lock()
	defer s.sendMutex.Unlock()
	slot, err := s.reserveResendOutput()
	if err != nil {
		return err
	}
	slot.wire, err = message.AppendTo(slot.wire[:0])
	if err != nil {
		slot.release()
		return err
	}
	message.retainOutboundFraming(slot.wire)
	if err := message.constructionError(); err != nil {
		slot.release()
		return err
	}
	s.outbound.enqueue(slot)
	return s.flushOutbound(true)
}

func (s *session) sendHeartbeat(inReplyTo *Message, testRequestID []byte) error {
	heartbeat, err := s.acquireControl("0")
	if err != nil {
		return err
	}
	defer s.releaseControl()
	if inReplyTo != nil || testRequestID != nil {
		setControlBytes(&heartbeat.Body.FieldMap, tagTestReqID, testRequestID)
	}
	return s.sendInReplyTo(heartbeat, inReplyTo)
}

func (s *session) logControlReject(reason string) {
	const prefix = "Message Rejected: "
	if s.boundedIn != nil && s.boundedIn.eventLog != nil && len(reason) <= len(s.controls.events)-len(prefix) {
		text := append(s.controls.events[:0], prefix...)
		text = append(text, reason...)
		s.boundedIn.eventLog.OnEventBorrowed(text)
		return
	}
	// Unknown loggers retain the legacy format and owned string arguments.
	s.log.OnEventf("Message Rejected: %v", reason)
}

func (s *session) sendTestRequest() error {
	request, err := s.acquireControl("1")
	if err != nil {
		return err
	}
	defer s.releaseControl()
	request.Body.SetString(tagTestReqID, "TEST")
	return s.send(request)
}

func (s *session) setControlRejectText(message *Message, reason string, refTag Tag) error {
	// FIX 4.0/4.1 include the reference tag in Text instead of RefTagID.
	text := s.controls.text[:0]
	if len(reason) > cap(text)-3-intTextLength(int(refTag)) {
		return ErrFrameCapacity
	}
	text = append(text, reason...)
	text = append(text, ' ', '(')
	text = strconv.AppendInt(text, int64(refTag), 10)
	text = append(text, ')')
	setControlBytes(&message.Body.FieldMap, tagText, text)
	return message.constructionError()
}

// Control values previously passed through FIXString.Read/Write, which copied
// the source and kept getter bytes separate from encoded wire. Preserve that
// ownership without changing public SetBytes' caller-owned alias semantics.
func setControlBytes(fields *FieldMap, tag Tag, value []byte) {
	arena := fields.mutableArena()
	if fields.constructionErr != nil {
		return
	}
	if arena == nil {
		fields.SetBytes(tag, bytes.Clone(value))
		return
	}
	if !arena.scalarFits(fields, tag, len(value)) {
		return
	}
	if arenaOverlaps(value, arena.bytes) || arenaOverlaps(value, arena.values) {
		copy(arena.staging, value)
		value = arena.staging[:len(value)]
	}
	arena.setScalar(fields, tag, value, "", nil, true)
}

func reverseControlRoute(source, target *Message) {
	for _, tags := range [...][2]Tag{
		{tagSenderCompID, tagTargetCompID}, {tagSenderSubID, tagTargetSubID},
		{tagSenderLocationID, tagTargetLocationID}, {tagTargetCompID, tagSenderCompID},
		{tagTargetSubID, tagSenderSubID}, {tagTargetLocationID, tagSenderLocationID},
		{tagOnBehalfOfCompID, tagDeliverToCompID}, {tagOnBehalfOfSubID, tagDeliverToSubID},
		{tagDeliverToCompID, tagOnBehalfOfCompID}, {tagDeliverToSubID, tagOnBehalfOfSubID},
	} {
		if value, present := source.Header.getBytesIfPresent(tags[0]); present && len(value) != 0 {
			setControlBytes(&target.Header.FieldMap, tags[1], value)
		}
	}
	if beginString, present := source.Header.getBytesIfPresent(tagBeginString); present && string(beginString) != BeginStringFIX40 {
		for _, tags := range [...][2]Tag{
			{tagOnBehalfOfLocationID, tagDeliverToLocationID},
			{tagDeliverToLocationID, tagOnBehalfOfLocationID},
		} {
			if value, present := source.Header.getBytesIfPresent(tags[0]); present && len(value) != 0 {
				setControlBytes(&target.Header.FieldMap, tags[1], value)
			}
		}
	}
}

// The callback passed through MessageStore must remain stable: a fresh closure
// capturing the resend cursor escapes through that interface on every request.
// This fixed cursor belongs to the session until IterateMessages returns.
type scopedResend struct {
	session                *session
	inReplyTo              *Message
	sequence, nextSequence int
	busy                   bool
	visit                  func([]byte) error
}

func (s *session) resendScopedMessages(beginSeqNo, endSeqNo int, inReplyTo *Message) error {
	cursor := &s.controls.resend
	if cursor.busy || s.controls.busy {
		return ErrOutboundCapacity
	}
	s.resendMutex.Lock()
	defer s.resendMutex.Unlock()
	cursor.busy = true
	cursor.sequence, cursor.nextSequence = beginSeqNo, beginSeqNo
	cursor.inReplyTo = inReplyTo
	defer cursor.finish()
	if err := s.store.IterateMessages(beginSeqNo, endSeqNo, cursor.visit); err != nil {
		s.log.OnEventf("error retrieving messages from store: %s", err.Error())
		return err
	}
	if cursor.sequence != cursor.nextSequence {
		return s.generateSequenceReset(cursor.sequence, cursor.nextSequence, inReplyTo)
	}
	return nil
}

func (cursor *scopedResend) finish() {
	cursor.session.outbound.replay.Reset()
	cursor.inReplyTo = nil
	cursor.busy = false
}

func (cursor *scopedResend) process(wire []byte) error {
	s := cursor.session
	s.outbound.replay.Reset()
	if err := s.outbound.replay.Parse(wire); err != nil {
		s.log.OnEventf("Resend Msg Parse Error: %v, %v", err.Error(), bytes.NewBuffer(wire).String())
		return err
	}
	message := s.outbound.replay.Message()
	if !applicationBorrowsMessages(s.application) {
		// Detach before resend rewrites parsed fields into engine scalar storage.
		message = materializeInboundMessage(message)
	}
	messageType, _ := message.Header.GetBytes(tagMsgType)
	sequence, _ := message.Header.GetInt(tagMsgSeqNum)
	if isAdminMessageType(messageType) || !s.resend(message) {
		cursor.nextSequence = sequence + 1
		return nil
	}
	if cursor.sequence != sequence {
		if err := s.generateSequenceReset(cursor.sequence, sequence, cursor.inReplyTo); err != nil {
			return err
		}
	}
	s.logSequenceEvent(eventResendingMessage, sequence, 0)
	if err := s.enqueueReplay(message, message.bodyBytes); err != nil {
		return err
	}
	cursor.sequence = sequence + 1
	cursor.nextSequence = cursor.sequence
	return nil
}
