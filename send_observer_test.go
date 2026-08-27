package quickfix

import (
	"errors"
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

func TestFixMsgTypeFromRaw(t *testing.T) {
	raw := []byte("8=FIXT.1.1\x019=12\x0135=8\x0149=SENDER\x01")
	if got := fixMsgTypeFromRaw(raw); got != "8" {
		t.Fatalf("msg type = %q, want %q", got, "8")
	}
}

func TestWriteCompletionObserver(t *testing.T) {
	defer SetWriteCompletionObserver(nil)

	var got WriteCompletionEvent
	SetWriteCompletionObserver(func(event WriteCompletionEvent) {
		got = event
	})

	expectedErr := errors.New("write failed")
	event := WriteCompletionEvent{
		SessionID:   SessionID{BeginString: BeginStringFIXT11, SenderCompID: "SENDER", TargetCompID: "TARGET"},
		MsgType:     "X",
		Token:       42,
		AdmittedAt:  time.Now().Add(-time.Millisecond),
		WriteStart:  time.Now(),
		CompletedAt: time.Now().Add(time.Millisecond),
		Bytes:       100,
		Err:         expectedErr,
	}
	observeWriteCompletion(event)

	if got != event {
		t.Fatalf("observer event = %#v, want %#v", got, event)
	}
}
