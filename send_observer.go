package quickfix

import (
	"sync/atomic"
	"time"
)

// SendTimingObserver receives coarse timing breakdowns for outbound message
// preparation. Observers must be non-blocking; they run on the send hot path.
type SendTimingObserver func(sessionID SessionID, stage string, duration time.Duration, err error)

type sendTimingObserverHolder struct {
	observer SendTimingObserver
}

var sendTimingObserver atomic.Value

// RegisterSendTimingObserver installs an outbound send timing observer and
// returns a cleanup function that restores the previous observer.
func RegisterSendTimingObserver(observer SendTimingObserver) func() {
	previous, _ := sendTimingObserver.Load().(sendTimingObserverHolder)
	sendTimingObserver.Store(sendTimingObserverHolder{observer: observer})
	return func() {
		sendTimingObserver.Store(previous)
	}
}

func observeSendTiming(sessionID SessionID, stage string, duration time.Duration, err error) {
	holder, _ := sendTimingObserver.Load().(sendTimingObserverHolder)
	if holder.observer == nil {
		return
	}
	holder.observer(sessionID, stage, duration, err)
}
