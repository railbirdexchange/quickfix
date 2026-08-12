package quickfix

import (
	"testing"
	"time"
)

func TestSendStageObserver(t *testing.T) {
	defer SetSendStageObserver(nil)

	var got SendStageEvent
	SetSendStageObserver(func(event SendStageEvent) {
		got = event
	})

	event := SendStageEvent{
		SessionID: SessionID{BeginString: BeginStringFIXT11, SenderCompID: "SENDER", TargetCompID: "TARGET"},
		MsgType:   "8",
		Stage:     "to_app",
		Duration:  time.Millisecond,
		Success:   true,
	}
	observeSendStage(event)

	if got != event {
		t.Fatalf("observer event = %#v, want %#v", got, event)
	}
}

func TestSendStageObserverRunsAfterSendMutexUnlock(t *testing.T) {
	defer SetSendStageObserver(nil)

	var testSession session
	observerCalled := false
	SetSendStageObserver(func(SendStageEvent) {
		if !testSession.sendMutex.TryLock() {
			t.Error("send stage observer called while send mutex was locked")
			return
		}
		testSession.sendMutex.Unlock()
		observerCalled = true
	})

	testSession.sendMutex.Lock()
	events := []SendStageEvent{{Stage: "persist"}}
	testSession.sendMutex.Unlock()
	observeSendStages(events)

	if !observerCalled {
		t.Fatal("send stage observer was not called")
	}
}

func TestFixMsgTypeFromRaw(t *testing.T) {
	raw := []byte("8=FIXT.1.1\x019=12\x0135=8\x0149=SENDER\x01")
	if got := fixMsgTypeFromRaw(raw); got != "8" {
		t.Fatalf("msg type = %q, want %q", got, "8")
	}
}
