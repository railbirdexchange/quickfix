package quickfix

import (
	"errors"
	"io"
	"net"
	"time"
)

var (
	ErrBoundedOutboundConfiguration = errors.New("quickfix: bounded outbound requires bounded inbound, BorrowedOutboundApplication, and a BoundedStore with persistence enabled")
	ErrBoundedOutboundScope         = errors.New("quickfix: bounded outbound capability is valid only in its borrowed inbound callback")
	ErrBoundedOutboundBusy          = errors.New("quickfix: an outbound builder is already active")
	ErrBoundedOutboundDisconnected  = errors.New("quickfix: bounded outbound writer is disconnected")
	ErrBoundedSendRequiresCallback  = errors.New("quickfix: bounded sessions require MessageSlot.BeginOutbound inside a borrowed callback")
	ErrInvalidOutboundLimits        = errors.New("quickfix: invalid outbound ring limits")
)

// BorrowedOutboundApplication explicitly opts into callback-scoped outbound
// builders. Both methods run on the session owner. They may read or mutate the
// builder synchronously, but must not retain it, send it, reset it, or use it
// concurrently. A ToAppBorrowed error vetoes sending, including during resend.
type BorrowedOutboundApplication interface {
	BorrowedApplication
	ToAdminBorrowed(*OutboundMessage, SessionID) error
	ToAppBorrowed(*OutboundMessage, SessionID) error
}

type outboundSlot struct {
	wire []byte
	err  error
}

// Only the session accesses free. ready and completed transfer ownership;
// neither sender accesses a slot after transferring it. A queue permit is
// reserved before committing journal/sequence state and returned on dequeue.
type boundedOutboundSession struct {
	session            *session
	builder            *OutboundMessage
	replayBuilder      *OutboundMessage
	store              *BoundedStore
	replay             *MessageSlot
	encoded            *MessageSlot
	application        BorrowedOutboundApplication
	free               []*outboundSlot
	ready              chan *outboundSlot
	completed          chan *outboundSlot
	permits            chan struct{}
	activeSlot         *MessageSlot
	callbackErr        boundedOutboundReject
	callbackDone       bool
	inOutboundCallback bool
	resetPending       bool
	writeTimeout       time.Duration
	writer             *boundedWriteConnection
}

type boundedOutboundReject struct{ err error }

func (e *boundedOutboundReject) Error() string             { return e.err.Error() }
func (*boundedOutboundReject) RejectReason() int           { return 0 }
func (*boundedOutboundReject) BusinessRejectRefID() string { return "" }
func (*boundedOutboundReject) RefTagID() *Tag              { return nil }
func (*boundedOutboundReject) IsBusinessReject() bool      { return false }

func newBoundedOutbound(s *session, app BorrowedOutboundApplication, store *BoundedStore, queue, builderBytes int, timeout time.Duration) (*boundedOutboundSession, error) {
	max := int(^uint(0) >> 1)
	if queue < 1 || queue >= max/1024 || timeout <= 0 || s.boundedIn == nil || queue+1 > max/s.boundedIn.limits.MaxFrameBytes {
		return nil, ErrInvalidOutboundLimits
	}
	limits := s.boundedIn.limits
	// Separate mutation headroom from the maximum number of wire fields.
	// Replacing headers during send/replay consumes mutation records too.
	builder, err := NewOutboundMessage(2*limits.MaxFields, builderBytes)
	if err != nil {
		return nil, err
	}
	replayBuilder, err := NewOutboundMessage(2*limits.MaxFields, builderBytes)
	if err != nil {
		return nil, err
	}
	replay, err := NewMessageSlot(s.transportDataDictionary, s.appDataDictionary, limits)
	if err != nil {
		return nil, err
	}
	encoded, err := NewMessageSlot(s.transportDataDictionary, s.appDataDictionary, limits)
	if err != nil {
		return nil, err
	}
	b := &boundedOutboundSession{session: s, application: app, store: store, builder: builder, replayBuilder: replayBuilder, replay: replay,
		free: make([]*outboundSlot, queue+1), ready: make(chan *outboundSlot, queue), completed: make(chan *outboundSlot, queue+1), permits: make(chan struct{}, queue), writeTimeout: timeout}
	b.encoded = encoded
	slots := make([]outboundSlot, queue+1)
	frames := make([]byte, (queue+1)*limits.MaxFrameBytes)
	for i := range slots {
		start, end := i*limits.MaxFrameBytes, (i+1)*limits.MaxFrameBytes
		slots[i].wire = frames[start:start:end]
		b.free[i] = &slots[i]
	}
	for i := 0; i < queue; i++ {
		b.permits <- struct{}{}
	}
	builder.owner, builder.active = b, false
	return b, nil
}

// BeginOutbound borrows the session's sole builder until Send, Abort, or the
// inbound callback returns. This avoids global session lookup and shared queues.
func (slot *MessageSlot) BeginOutbound(msgType []byte) (*OutboundMessage, error) {
	b := slot.outbound
	if b == nil || b.activeSlot != slot {
		return nil, ErrBoundedOutboundScope
	}
	m, err := b.begin(msgType)
	if err != nil {
		b.callbackErr.err = err
	}
	return m, err
}

// RetireOutboundBefore advances the resend recovery floor only after the
// application has independently established peer acknowledgement. Socket write
// completion is not acknowledgement. Call only from this slot's callback.
func (slot *MessageSlot) RetireOutboundBefore(nextSequence int) error {
	b := slot.outbound
	if b == nil || b.activeSlot != slot {
		return ErrBoundedOutboundScope
	}
	err := b.store.RetireBefore(nextSequence)
	if err != nil {
		b.callbackErr.err = err
	}
	return err
}

func (b *boundedOutboundSession) begin(msgType []byte) (*OutboundMessage, error) {
	if b.builder.active {
		return nil, ErrBoundedOutboundBusy
	}
	b.builder.Reset()
	b.builder.active = true
	b.callbackDone = false
	if err := b.builder.Header().SetBytes(tagMsgType, msgType); err != nil {
		b.abort()
		return nil, err
	}
	return b.builder, nil
}

func (b *boundedOutboundSession) abort() {
	b.builder.active = false
	b.builder.Reset()
	b.callbackDone = false
}

// Abort releases an unfinished session builder without sequence/store changes.
func (m *OutboundMessage) Abort() {
	if m.owner != nil && m.active {
		m.owner.abort()
	}
}

// Send reserves output and journal capacity, fully encodes, commits persistence
// and sequence, then transfers the output to the writer. Completion remains
// asynchronous; only the completed-slot channel returns output ownership.
func (m *OutboundMessage) Send() error {
	if m.owner == nil || !m.active || m.owner.activeSlot == nil || m.owner.inOutboundCallback {
		return ErrBoundedOutboundScope
	}
	b := m.owner
	err := b.sendBuilder(b.activeSlot.Message(), nil, 0)
	if err != nil {
		b.callbackErr.err = err
	}
	return err
}

func (b *boundedOutboundSession) reclaim(slot *outboundSlot) error {
	err := slot.err
	slot.err = nil
	slot.wire = slot.wire[:0]
	index := len(b.free)
	b.free = b.free[:index+1]
	b.free[index] = slot
	return err
}

func (b *boundedOutboundSession) reserveOutput() (*outboundSlot, error) {
	if b.writer == nil {
		return nil, ErrBoundedOutboundDisconnected
	}
	for {
		// Observe completed errors before committing further messages.
		select {
		case slot := <-b.completed:
			if err := b.reclaim(slot); err != nil {
				return nil, err
			}
			continue
		default:
		}
		select {
		case <-b.writer.done:
			return nil, ErrBoundedOutboundDisconnected
		default:
		}
		select {
		case <-b.permits:
			// Q permits and Q+1 slots cover Q queued plus one writer-owned slot.
			for len(b.free) == 0 {
				slot := <-b.completed
				if err := b.reclaim(slot); err != nil {
					b.permits <- struct{}{}
					return nil, err
				}
			}
			last := len(b.free) - 1
			slot := b.free[last]
			b.free[last] = nil
			b.free = b.free[:last]
			return slot, nil
		case slot := <-b.completed:
			if err := b.reclaim(slot); err != nil {
				return nil, err
			}
		case <-b.writer.done:
			return nil, ErrBoundedOutboundDisconnected
		}
	}
}

func (b *boundedOutboundSession) cancelOutput(slot *outboundSlot) {
	b.reclaim(slot)
	b.permits <- struct{}{}
}

func (b *boundedOutboundSession) fillHeader(m *OutboundMessage, reply *Message) {
	s := b.session
	h := m.Header()
	h.SetString(tagBeginString, s.sessionID.BeginString)
	h.SetString(tagSenderCompID, s.sessionID.SenderCompID)
	h.SetString(tagTargetCompID, s.sessionID.TargetCompID)
	if s.sessionID.SenderSubID != "" {
		h.SetString(tagSenderSubID, s.sessionID.SenderSubID)
	}
	if s.sessionID.SenderLocationID != "" {
		h.SetString(tagSenderLocationID, s.sessionID.SenderLocationID)
	}
	if s.sessionID.TargetSubID != "" {
		h.SetString(tagTargetSubID, s.sessionID.TargetSubID)
	}
	if s.sessionID.TargetLocationID != "" {
		h.SetString(tagTargetLocationID, s.sessionID.TargetLocationID)
	}
	precision := s.timestampPrecision
	if s.sessionID.BeginString < BeginStringFIX42 {
		precision = Seconds
	}
	h.SetTime(tagSendingTime, FIXUTCTimestamp{Time: time.Now().UTC(), Precision: precision})
	if s.EnableLastMsgSeqNumProcessed {
		seq := s.store.NextTargetMsgSeqNum() - 1
		if reply != nil {
			if value, err := reply.Header.GetInt(tagMsgSeqNum); err == nil {
				seq = value
			}
		}
		h.SetInt(tagLastMsgSeqNumProcessed, seq)
	}
}

func (b *boundedOutboundSession) sendBuilder(reply *Message, rawBody []byte, replaySeq int) error {
	err := b.sendMessage(b.builder, reply, rawBody, replaySeq, b.callbackDone)
	b.abort()
	return err
}

func (b *boundedOutboundSession) sendMessage(m *OutboundMessage, reply *Message, rawBody []byte, replaySeq int, callbackDone bool) (err error) {
	if err = m.Err(); err != nil {
		return err
	}
	reserveStarted := time.Now()
	output, err := b.reserveOutput()
	b.observe("outbound_capacity_wait", nil, reserveStarted, err == nil)
	if err != nil {
		return err
	}
	transferred := false
	reservation := -1
	defer func() {
		if !transferred {
			b.cancelOutput(output)
		}
		if reservation >= 0 {
			b.store.abort(reservation)
		}
	}()
	seq := replaySeq
	reset := false
	if seq == 0 {
		seq = b.store.NextSenderMsgSeqNum()
		// An explicit reset already present in a Logon can reserve reset capacity
		// even when the old journal is full, without discarding it prematurely.
		kind, _ := m.Message().Header.GetBytes(tagMsgType)
		reset = string(kind) == "A" && b.resetPending
		if string(kind) == "A" && m.Message().Body.Has(tagResetSeqNumFlag) {
			flag, _ := m.Message().Body.GetBytes(tagResetSeqNumFlag)
			reset = reset || string(flag) == "Y"
		}
		if reset {
			reservation, err = b.store.reserveReset()
			seq = 1
		} else {
			reservation, err = b.store.reserve(seq)
		}
		if err != nil {
			return err
		}
	}
	if replaySeq == 0 {
		b.fillHeader(m, reply)
	}
	m.Header().SetInt(tagMsgSeqNum, seq)
	if err = m.Err(); err != nil {
		return err
	}
	kind, rej := m.Message().Header.GetBytes(tagMsgType)
	if rej != nil {
		return rej
	}
	if !callbackDone {
		callbackStarted := time.Now()
		b.inOutboundCallback = true
		if isAdminMessageType(kind) {
			err = b.application.ToAdminBorrowed(m, b.session.sessionID)
			b.observe("to_admin", kind, callbackStarted, err == nil)
		} else {
			err = b.application.ToAppBorrowed(m, b.session.sessionID)
			b.observe("to_app", kind, callbackStarted, err == nil)
		}
		b.inOutboundCallback = false
		if err != nil {
			return err
		}
	}
	if replaySeq == 0 && string(kind) == "A" {
		finalReset := b.resetPending
		if m.Message().Body.Has(tagResetSeqNumFlag) {
			flag, _ := m.Message().Body.GetBytes(tagResetSeqNumFlag)
			if string(flag) != "Y" && string(flag) != "N" {
				return IncorrectDataFormatForValue(tagResetSeqNumFlag)
			}
			finalReset = finalReset || string(flag) == "Y"
		}
		if finalReset != reset {
			b.store.abort(reservation)
			reservation = -1
			if finalReset {
				reservation, err = b.store.reserveReset()
				seq = 1
			} else {
				seq = b.store.NextSenderMsgSeqNum()
				reservation, err = b.store.reserve(seq)
			}
			if err != nil {
				return err
			}
			reset = finalReset
			m.Header().SetInt(tagMsgSeqNum, seq)
		}
	}
	if err = m.Err(); err != nil {
		return err
	}
	// Sequence ownership remains with the session even if the callback changes it.
	actual, rej := m.Message().Header.GetInt(tagMsgSeqNum)
	if rej != nil {
		return rej
	}
	if actual != seq {
		return ErrInvalidJournalSequence
	}
	buildStarted := time.Now()
	if rawBody != nil {
		output.wire, err = m.Message().AppendWithBodyBytesTo(output.wire[:0], rawBody)
	} else {
		output.wire, err = m.Message().AppendTo(output.wire[:0])
	}
	b.observe("build_message", kind, buildStarted, err == nil)
	if err != nil {
		return err
	}
	// Check every emitted frame, including callback-mutated resend headers,
	// against the fixed limits before persistence or ownership transfer.
	err = b.encoded.Parse(output.wire)
	b.encoded.Reset()
	if err != nil {
		return err
	}
	if replaySeq == 0 {
		persistStarted := time.Now()
		if reset {
			err = b.store.commitReset(reservation, output.wire)
		} else {
			err = b.store.commit(reservation, seq, output.wire, true)
		}
		b.observe("persist", kind, persistStarted, err == nil)
		if err != nil {
			return err
		}
		reservation = -1
		if reset {
			b.resetPending = false
			b.session.sentReset = true
		}
	}
	b.session.boundedIn.log.OnOutgoingBorrowed(output.wire)
	channelStarted := time.Now()
	b.ready <- output
	transferred = true
	b.observe("outbound_channel_wait", kind, channelStarted, true)
	if b.session.stateTimer != nil {
		b.session.stateTimer.Reset(b.session.HeartBtInt)
	}
	return nil
}

func (b *boundedOutboundSession) observe(stage string, kind []byte, started time.Time, success bool) {
	if observer := sendStageObserverState.Load(); observer != nil {
		(*observer)(SendStageEvent{SessionID: b.session.sessionID, MsgType: string(kind), Stage: stage, Duration: time.Since(started), Success: success})
	}
}

func (b *boundedOutboundSession) sendLegacy(m, reply *Message) error {
	if b.builder.active {
		return ErrBoundedOutboundBusy
	}
	b.builder.Reset()
	b.builder.active = true
	if err := b.builder.CopyFrom(m); err != nil {
		b.abort()
		return err
	}
	return b.sendBuilder(reply, nil, 0)
}

type boundedWriteConnection struct {
	connection net.Conn
	owner      *boundedOutboundSession
	stop       chan struct{}
	done       chan struct{}
	result     error // writer-owned; readable only after done is closed.
}

func newBoundedWriteConnection(conn net.Conn, b *boundedOutboundSession) *boundedWriteConnection {
	return &boundedWriteConnection{connection: conn, owner: b, stop: make(chan struct{}), done: make(chan struct{})}
}

func (w *boundedWriteConnection) run() {
	defer close(w.done)
	for {
		select {
		case <-w.stop:
			return
		case slot := <-w.owner.ready:
			w.owner.permits <- struct{}{}
			slot.err = w.connection.SetWriteDeadline(time.Now().Add(w.owner.writeTimeout))
			remaining := slot.wire
			for slot.err == nil && len(remaining) > 0 {
				var n int
				n, slot.err = w.connection.Write(remaining)
				if n < 0 || n > len(remaining) {
					slot.err = io.ErrShortWrite
					break
				}
				remaining = remaining[n:]
				if n == 0 && slot.err == nil {
					slot.err = io.ErrNoProgress
				}
			}
			failed := slot.err != nil
			if failed {
				w.result = slot.err
			}
			w.owner.completed <- slot
			if failed {
				return
			}
		}
	}
}

// finish joins the writer before reclaiming all its slots. Normal disconnects
// flush pending messages; a finite per-message deadline bounds slow consumers.
func (w *boundedWriteConnection) finish() error {
	b := w.owner
	for len(b.free) < cap(b.free) {
		select {
		case slot := <-b.completed:
			b.reclaim(slot)
		case <-w.done:
			goto stopped
		}
	}
	close(w.stop)
	<-w.done
stopped:
	for {
		select {
		case slot := <-b.ready:
			b.permits <- struct{}{}
			b.reclaim(slot)
		default:
			goto completed
		}
	}
completed:
	for {
		select {
		case slot := <-b.completed:
			b.reclaim(slot)
		default:
			return w.result
		}
	}
}
