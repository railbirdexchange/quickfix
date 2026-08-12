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

// SendStageObserver receives send-stage timing events.
type SendStageObserver func(SendStageEvent)

var sendStageObserverState struct {
	sync.RWMutex
	observer SendStageObserver
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
