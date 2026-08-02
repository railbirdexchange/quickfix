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
		Stage:     "socket_write",
		Duration:  time.Millisecond,
		Success:   true,
	}
	observeSendStage(event)

	if got != event {
		t.Fatalf("observer event = %#v, want %#v", got, event)
	}
}

func TestFixMsgTypeFromRaw(t *testing.T) {
	raw := []byte("8=FIXT.1.1\x019=12\x0135=8\x0149=SENDER\x01")
	if got := fixMsgTypeFromRaw(raw); got != "8" {
		t.Fatalf("msg type = %q, want %q", got, "8")
	}
}
