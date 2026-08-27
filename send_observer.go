package quickfix

import (
	"bytes"
	"sync"
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

// SendStageObserver receives synchronous send-stage timing events.
type SendStageObserver func(SendStageEvent)

// WriteCompletionEvent reports the result of writing one tracked FIX message
// to the connection. Token is the opaque value supplied by the caller when the
// message was submitted; QuickFIX does not interpret it.
type WriteCompletionEvent struct {
	SessionID   SessionID
	MsgType     string
	Token       uint64
	AdmittedAt  time.Time
	WriteStart  time.Time
	CompletedAt time.Time
	Bytes       int
	Err         error
}

// WriteCompletionObserver receives asynchronous socket-write completion
// events for messages submitted with a non-zero write token.
type WriteCompletionObserver func(WriteCompletionEvent)

var sendStageObserverState struct {
	sync.RWMutex
	observer SendStageObserver
}

var writeCompletionObserverState struct {
	sync.RWMutex
	observer WriteCompletionObserver
}

// SetSendStageObserver installs a process-wide observer for FIX send-stage
// timings. Passing nil disables observation.
func SetSendStageObserver(observer SendStageObserver) {
	sendStageObserverState.Lock()
	defer sendStageObserverState.Unlock()
	sendStageObserverState.observer = observer
}

func observeSendStage(event SendStageEvent) {
	sendStageObserverState.RLock()
	observer := sendStageObserverState.observer
	sendStageObserverState.RUnlock()
	if observer != nil {
		observer(event)
	}
}

// SetWriteCompletionObserver installs a process-wide observer for tracked FIX
// socket writes. Passing nil disables observation. Untracked messages do not
// call the observer or incur write timing calls.
func SetWriteCompletionObserver(observer WriteCompletionObserver) {
	writeCompletionObserverState.Lock()
	defer writeCompletionObserverState.Unlock()
	writeCompletionObserverState.observer = observer
}

func observeWriteCompletion(event WriteCompletionEvent) {
	writeCompletionObserverState.RLock()
	observer := writeCompletionObserverState.observer
	writeCompletionObserverState.RUnlock()
	if observer != nil {
		observer(event)
	}
}

func fixMsgTypeFromRaw(msg []byte) string {
	if bytes.HasPrefix(msg, []byte("35=")) {
		return rawTagValue(msg[3:])
	}
	if index := bytes.Index(msg, []byte{'\x01', '3', '5', '='}); index >= 0 {
		return rawTagValue(msg[index+4:])
	}
	return ""
}

func rawTagValue(valueAndRest []byte) string {
	if end := bytes.IndexByte(valueAndRest, '\x01'); end >= 0 {
		return string(valueAndRest[:end])
	}
	return string(valueAndRest)
}
