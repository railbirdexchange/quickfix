package quickfix

import (
	"bytes"
	"sync/atomic"
	"time"
)

// SendStageEvent reports timing for one stage of sending a FIX message.
type SendStageEvent struct {
	SessionID SessionID
	MsgType   string
	Stage     string
	Duration  time.Duration
	Success   bool
}

// SendStageObserver receives synchronous timing events on the goroutine doing
// the work. Callbacks may run concurrently on submitting, session and socket
// writer goroutines; they must return promptly. Event values may be retained.
//
// For bounded output, outbound_queue_wait measures enqueue through write start,
// socket_write measures the full write including partial-write retries, and
// outbound_complete measures enqueue through write completion and slot return.
// These events are delivered after slot return and before the writer's completion
// notification. Queue-based intervals require observation at both enqueue and
// the start of flushing to the writer. socket_write requires observation at
// the start of flushing to the writer.
type SendStageObserver func(SendStageEvent)

// An installed function is immutable. Only installation allocates its holder;
// emitting events never takes a process-wide lock.
var sendStageObserverState atomic.Pointer[SendStageObserver]

// SetSendStageObserver installs a process-wide observer for FIX send-stage
// timings. Passing nil disables observation.
func SetSendStageObserver(observer SendStageObserver) {
	if observer == nil {
		sendStageObserverState.Store(nil)
		return
	}
	sendStageObserverState.Store(&observer)
}

func startSendStage() time.Time {
	if sendStageObserverState.Load() != nil {
		return time.Now()
	}
	return time.Time{}
}

func (s *session) finishSendStage(stage string, messageType []byte, started time.Time, success bool) {
	if !started.IsZero() {
		s.observeSendStage(stage, s.sendStageMessageType(messageType), time.Since(started), success)
	}
}

func observeSendStage(event SendStageEvent) {
	if observer := sendStageObserverState.Load(); observer != nil {
		(*observer)(event)
	}
}

func (s *session) sendStageMessageType(value []byte) string {
	if s.boundedIn != nil {
		return s.boundedIn.metadata.ownedString(tagMsgType, value)
	}
	return string(value)
}

// BuiltMessageObserver receives the final encoded bytes synchronously after a
// normal message's store update succeeds, or after a replay is rebuilt, before
// enqueueing. Persistence follows the configured DisableMessagePersist policy.
// It does not report completed socket writes. The callback must return promptly
// and must not send on the same session or mutate msg/raw. The raw argument is
// authoritative for replay, whose original serialized body is preserved.
//
// Ordinary applications may retain msg and raw under the existing outgoing
// message ownership rules. BorrowApplication instead makes engine-owned messages
// and raw callback-scoped; copy anything retained before the callback returns.
type BuiltMessageObserver interface {
	OnMessageBuilt(sessionID SessionID, msg *Message, raw []byte)
}

// QueueDepthEvent reports pending outbound messages and queued inbound messages.
// ToSend excludes frames already transferred to the socket writer.
type QueueDepthEvent struct {
	SessionID SessionID
	ToSend    int
	MessageIn int
}

// QueueDepthObserver runs synchronously during session flushing. It must return
// promptly and must not send on the same session. Event values may be retained.
type QueueDepthObserver func(QueueDepthEvent)

var queueDepthObserverState atomic.Pointer[QueueDepthObserver]

// SetQueueDepthObserver installs a process-wide observer; nil disables it.
func SetQueueDepthObserver(observer QueueDepthObserver) {
	if observer == nil {
		queueDepthObserverState.Store(nil)
		return
	}
	queueDepthObserverState.Store(&observer)
}

func observeQueueDepth(event QueueDepthEvent) {
	if observer := queueDepthObserverState.Load(); observer != nil {
		(*observer)(event)
	}
}

func fixMsgTypeFromRaw(msg []byte) string {
	return string(fixMsgTypeBytesFromRaw(msg))
}

func fixMsgTypeBytesFromRaw(msg []byte) []byte {
	if bytes.HasPrefix(msg, []byte("35=")) {
		return rawTagBytes(msg[3:])
	}
	if index := bytes.Index(msg, []byte{'\x01', '3', '5', '='}); index >= 0 {
		return rawTagBytes(msg[index+4:])
	}
	return nil
}

func rawTagBytes(valueAndRest []byte) []byte {
	if end := bytes.IndexByte(valueAndRest, '\x01'); end >= 0 {
		return valueAndRest[:end]
	}
	return valueAndRest
}
