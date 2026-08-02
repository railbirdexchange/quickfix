package quickfix

import (
	"bytes"
	"sync/atomic"
	"time"
)

// SendStageEvent reports timing for a FIX socket write.
type SendStageEvent struct {
	SessionID SessionID
	MsgType   string
	Stage     string
	Duration  time.Duration
	Success   bool
}

// SendStageObserver receives synchronous socket-write timing events after the
// session send lock has been released. Observers must not call session send APIs;
// replay observations still occur while the resend lock preserves wire order.
type SendStageObserver func(SendStageEvent)

var sendStageObserver atomic.Pointer[SendStageObserver]

// SetSendStageObserver installs a process-wide observer for FIX socket-write
// timings. Passing nil disables observation.
func SetSendStageObserver(observer SendStageObserver) {
	if observer == nil {
		sendStageObserver.Store(nil)
		return
	}
	sendStageObserver.Store(&observer)
}

func observeSendStage(event SendStageEvent) {
	observer := sendStageObserver.Load()
	if observer != nil {
		(*observer)(event)
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
