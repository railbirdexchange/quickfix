package quickfix

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueueDepthObserverTracksBoundedPending(t *testing.T) {
	current, _, _ := newScopedOutboundLifecycleSession(t)
	inbound := make(chan fixIn, 2)
	inbound <- fixIn{}
	current.messageIn = inbound
	for range 2 {
		if err := current.queueForSend(scopedOutboundLifecycleOrder("pending")); err != nil {
			t.Fatal(err)
		}
	}
	defer current.outbound.dropPending()
	var got QueueDepthEvent
	SetQueueDepthObserver(func(event QueueDepthEvent) { got = event })
	defer SetQueueDepthObserver(nil)
	current.sendMutex.Lock()
	current.observeQueueDepth()
	current.sendMutex.Unlock()
	want := QueueDepthEvent{SessionID: current.sessionID, ToSend: 2, MessageIn: 1}
	if got != want {
		t.Fatalf("queue depth = %#v, want %#v", got, want)
	}
	if allocations := testing.AllocsPerRun(100, current.observeQueueDepth); allocations != 0 {
		t.Fatalf("queue observation allocated %v objects", allocations)
	}
	SetQueueDepthObserver(nil)
	current.outbound.dropPending()
	current.observeQueueDepth()
	if got != want {
		t.Fatal("disabled observer ran or retained event changed")
	}
}

func TestQueueDepthObserverConcurrentReplacement(t *testing.T) {
	defer SetQueueDepthObserver(nil)
	want := QueueDepthEvent{SessionID: SessionID{SenderCompID: "SENDER"}, ToSend: 2, MessageIn: 1}
	var invalid atomic.Bool
	observer := func(got QueueDepthEvent) {
		if got != want {
			invalid.Store(true)
		}
	}
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for range 1000 {
			SetQueueDepthObserver(observer)
			SetQueueDepthObserver(nil)
		}
	}()
	go func() {
		defer workers.Done()
		for range 1000 {
			observeQueueDepth(want)
		}
	}()
	workers.Wait()
	if invalid.Load() {
		t.Fatal("concurrent observation corrupted the event")
	}
}

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
