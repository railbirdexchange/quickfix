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
