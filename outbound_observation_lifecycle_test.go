package quickfix

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type outboundLifecycleEvent struct {
	event  SendStageEvent
	vacant int
}

// Only the test owner drains this bounded channel. Caller and writer callbacks
// may overlap; they never append to a shared slice or inspect mutable wire bytes.
type outboundLifecycleRecorder struct {
	events   chan outboundLifecycleEvent
	ring     *outboundRing
	overflow atomic.Bool
}

func (recorder *outboundLifecycleRecorder) install() {
	SetSendStageObserver(func(event SendStageEvent) {
		select {
		case recorder.events <- outboundLifecycleEvent{event: event, vacant: len(recorder.ring.vacant)}:
		default:
			recorder.overflow.Store(true)
		}
	})
}

func (recorder *outboundLifecycleRecorder) take(t *testing.T) []outboundLifecycleEvent {
	t.Helper()
	if recorder.overflow.Load() {
		t.Fatal("bounded observation channel overflowed")
	}
	events := make([]outboundLifecycleEvent, 0, cap(recorder.events))
	for {
		select {
		case event := <-recorder.events:
			events = append(events, event)
		default:
			return events
		}
	}
}

func newOutboundLifecycleFixture(t *testing.T, messageType string, gated bool) (*scopedOutboundBenchmark, *outboundLifecycleRecorder, *scopedOutboundTestConnection) {
	t.Helper()
	SetSendStageObserver(nil)
	t.Cleanup(func() { SetSendStageObserver(nil) })
	fixture := newScopedOutboundBenchmarkWithDelivery(t, true, messageType, true)
	t.Cleanup(fixture.close)
	recorder := &outboundLifecycleRecorder{events: make(chan outboundLifecycleEvent, 64), ring: fixture.session.outbound}
	if !gated {
		return fixture, recorder, nil
	}
	if err := fixture.session.writeConnection.finish(); err != nil {
		t.Fatal(err)
	}
	connection := &scopedOutboundTestConnection{
		scopedOutboundBenchmarkConnection: scopedOutboundBenchmarkConnection{wire: make([]byte, 16384)},
		started:                           make(chan struct{}, 2),
		allow:                             make(chan struct{}),
	}
	fixture.connection = &connection.scopedOutboundBenchmarkConnection
	fixture.session.writeConnection = newOutboundWriter(connection, len(fixture.session.outbound.pending), time.Second, fixture.session.messageEvent)
	// Unblock even a failing assertion before joining the writer.
	t.Cleanup(func() { close(connection.allow) })
	go fixture.session.writeConnection.run()
	return fixture, recorder, connection
}

func outboundLifecycleWake(t *testing.T, current *session) {
	t.Helper()
	select {
	case <-current.messageEvent:
	case <-time.After(2 * time.Second):
		t.Fatal("missing outbound notification")
	}
}

func outboundLifecycleEnqueue(t *testing.T, fixture *scopedOutboundBenchmark) {
	t.Helper()
	if err := SendToTarget(fixture.message, fixture.session.sessionID); err != nil {
		t.Fatal(err)
	}
	outboundLifecycleWake(t, fixture.session)
}

func outboundLifecycleFlush(t *testing.T, current *session) {
	t.Helper()
	current.sendMutex.Lock()
	err := current.flushOutbound(true)
	current.sendMutex.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

func outboundLifecycleStage(stage string) bool {
	return stage == "outbound_queue_wait" || stage == "socket_write" || stage == "outbound_complete"
}

func requireNoOutboundLifecycleEvents(t *testing.T, events []outboundLifecycleEvent) {
	t.Helper()
	for _, captured := range events {
		if outboundLifecycleStage(captured.event.Stage) {
			t.Fatalf("premature lifecycle event: %+v", captured)
		}
	}
}

func requireOutboundLifecycleEvent(t *testing.T, events []outboundLifecycleEvent, stage, messageType string, id SessionID, success bool, vacant int) SendStageEvent {
	t.Helper()
	var result SendStageEvent
	count := 0
	for _, captured := range events {
		if captured.event.Stage != stage {
			continue
		}
		count++
		result = captured.event
		if result.MsgType != messageType || result.SessionID != id || result.Success != success ||
			result.Duration < 0 || captured.vacant != vacant {
			t.Fatalf("invalid completed lifecycle event: %+v, want type=%q session=%+v success=%v vacant=%d",
				captured, messageType, id, success, vacant)
		}
	}
	if count != 1 {
		t.Fatalf("stage %q count=%d, want one: %+v", stage, count, events)
	}
	return result
}

func TestOutboundObservationPartialWriteCompletionAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "completed"
		if fail {
			name = "partial_then_failure"
		}
		t.Run(name, func(t *testing.T) {
			fixture, recorder, connection := newOutboundLifecycleFixture(t, "AJ", true)
			recorder.install()
			if fail {
				connection.failAt = 2
				connection.failure = errors.New("last partial write failed")
			}
			outboundLifecycleEnqueue(t, fixture)
			connection.maxWrite = len(fixture.session.outbound.front().wire) - 1
			queued := time.Now()
			time.Sleep(3 * time.Millisecond)
			pendingMinimum := time.Since(queued)
			requireNoOutboundLifecycleEvents(t, recorder.take(t))
			outboundLifecycleFlush(t, fixture.session)
			scopedBackpressureWait(t, connection.started, "first partial write did not start")
			requireNoOutboundLifecycleEvents(t, recorder.take(t))
			connection.allow <- struct{}{}
			scopedBackpressureWait(t, connection.started, "last partial write did not start")
			requireNoOutboundLifecycleEvents(t, recorder.take(t))
			if len(recorder.ring.vacant) != len(recorder.ring.slots)-1 {
				t.Fatal("slot returned before last partial write completed")
			}
			blocked := time.Now()
			time.Sleep(3 * time.Millisecond)
			writeMinimum := time.Since(blocked)
			connection.allow <- struct{}{}
			outboundLifecycleWake(t, fixture.session)
			events := recorder.take(t)
			// Reaching the writer completes the queue stage even if the socket
			// subsequently fails; write and whole-send success carry that error.
			queue := requireOutboundLifecycleEvent(t, events, "outbound_queue_wait", "AJ", fixture.session.sessionID, true, len(recorder.ring.slots))
			write := requireOutboundLifecycleEvent(t, events, "socket_write", "AJ", fixture.session.sessionID, !fail, len(recorder.ring.slots))
			complete := requireOutboundLifecycleEvent(t, events, "outbound_complete", "AJ", fixture.session.sessionID, !fail, len(recorder.ring.slots))
			if queue.Duration < pendingMinimum || write.Duration < writeMinimum ||
				complete.Duration < queue.Duration || complete.Duration < write.Duration {
				t.Fatalf("lost queue/write interval: queue=%v minimum=%v write=%v minimum=%v complete=%v",
					queue.Duration, pendingMinimum, write.Duration, writeMinimum, complete.Duration)
			}
			if connection.calls != 2 {
				t.Fatalf("write calls=%d, want two", connection.calls)
			}
			if fail && fixture.session.writeConnection.err != connection.failure {
				t.Fatalf("writer error=%v, want %v", fixture.session.writeConnection.err, connection.failure)
			}
		})
	}
}

func TestOutboundObservationRetainedMetadataAcrossSlotReuse(t *testing.T) {
	const original = "UNKNOWN.ORIGINAL.MESSAGE.TYPE"
	fixture, recorder, _ := newOutboundLifecycleFixture(t, original, false)
	recorder.install()
	if err := fixture.process(); err != nil {
		t.Fatal(err)
	}
	retained := recorder.take(t)
	id := fixture.session.sessionID
	for iteration := 0; iteration <= len(recorder.ring.slots); iteration++ {
		fixture.message.Header.SetString(tagMsgType, "UNKNOWN.REPLACEMENT.MESSAGE.TYPE")
		if err := fixture.process(); err != nil {
			t.Fatal(err)
		}
		recorder.take(t)
	}
	value, reject := fixture.message.Header.GetBytes(tagMsgType)
	if reject != nil {
		t.Fatal(reject)
	}
	for index := range value {
		value[index] = '?'
	}
	for _, stage := range []string{"outbound_queue_wait", "socket_write", "outbound_complete"} {
		requireOutboundLifecycleEvent(t, retained, stage, original, id, true, len(recorder.ring.slots))
	}
}

func TestOutboundObservationConfiguredTypeChangedByCallback(t *testing.T) {
	fixture, recorder, _ := newOutboundLifecycleFixture(t, "AJ", false)
	for _, messageType := range []string{"AJ", "AE"} {
		if _, ok := fixture.session.boundedIn.metadata.messageTypes[messageType]; !ok {
			t.Fatalf("fixture does not configure %q", messageType)
		}
	}
	// Establish the reusable caller arena before the callback replaces MsgType.
	for range 2 {
		if err := fixture.process(); err != nil {
			t.Fatal(err)
		}
	}
	application := &scopedOutboundHookApplication{scopedOutboundBenchmarkApplication: fixture.application}
	application.toApp = func(message *Message) error {
		if value, reject := message.Header.GetString(tagMsgType); reject != nil || value != "AJ" {
			t.Fatalf("callback input type=%q reject=%v", value, reject)
		}
		message.Header.SetString(tagMsgType, "AE")
		return nil
	}
	fixture.session.application = BorrowApplication(application)
	recorder.install()
	if err := fixture.process(); err != nil {
		t.Fatal(err)
	}
	if actual := fixMsgTypeFromRaw(fixture.connection.wire); actual != "AE" {
		t.Fatalf("completed wire type=%q, want callback type AE", actual)
	}
	retained := recorder.take(t)
	id := fixture.session.sessionID
	application.toApp = nil
	for iteration := 0; iteration <= len(recorder.ring.slots); iteration++ {
		fixture.message.Header.SetString(tagMsgType, "8")
		if err := fixture.process(); err != nil {
			t.Fatal(err)
		}
		recorder.take(t)
	}
	value, reject := fixture.message.Header.GetBytes(tagMsgType)
	if reject != nil {
		t.Fatal(reject)
	}
	value[0] = '?'
	for _, expected := range []struct{ stage, messageType string }{
		{"to_app", "AJ"}, {"build_message", "AJ"}, {"persist", "AJ"}, {"outbound_channel_wait", "AE"},
	} {
		count := 0
		for _, captured := range retained {
			event := captured.event
			if event.Stage != expected.stage {
				continue
			}
			count++
			if event.MsgType != expected.messageType || event.SessionID != id || !event.Success {
				t.Fatalf("retained callback event=%+v, want type %q", event, expected.messageType)
			}
		}
		if count != 1 {
			t.Fatalf("stage %q count=%d, want one", expected.stage, count)
		}
	}
	for _, stage := range []string{"outbound_queue_wait", "socket_write", "outbound_complete"} {
		requireOutboundLifecycleEvent(t, retained, stage, "AE", id, true, len(recorder.ring.slots))
	}
}

func TestOutboundObservationEnableBoundaries(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		atEnqueue     bool
		atFlush       bool
		enableInWrite bool
		wantSocket    bool
	}{
		{name: "disabled"},
		{name: "enabled_after_enqueue", atFlush: true, wantSocket: true},
		{name: "disabled_after_enqueue", atEnqueue: true},
		{name: "enabled_during_unobserved_write", enableInWrite: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture, recorder, connection := newOutboundLifecycleFixture(t, "AJ", true)
			if scenario.atEnqueue {
				recorder.install()
			}
			outboundLifecycleEnqueue(t, fixture)
			recorder.take(t)
			SetSendStageObserver(nil)
			if scenario.atFlush {
				recorder.install()
			}
			outboundLifecycleFlush(t, fixture.session)
			scopedBackpressureWait(t, connection.started, "write did not start")
			requireNoOutboundLifecycleEvents(t, recorder.take(t))
			if scenario.enableInWrite {
				recorder.install()
			}
			connection.allow <- struct{}{}
			outboundLifecycleWake(t, fixture.session)
			events := recorder.take(t)
			if scenario.wantSocket {
				requireOutboundLifecycleEvent(t, events, "socket_write", "AJ", fixture.session.sessionID, true, len(recorder.ring.slots))
				for _, captured := range events {
					if captured.event.Stage == "outbound_queue_wait" || captured.event.Stage == "outbound_complete" {
						t.Fatalf("invented unobserved enqueue interval: %+v", captured)
					}
				}
			} else {
				requireNoOutboundLifecycleEvents(t, events)
			}
		})
	}
}

func TestOutboundObservationConcurrentObserverReplacement(t *testing.T) {
	defer SetSendStageObserver(nil)
	expected := SendStageEvent{SessionID: SessionID{BeginString: BeginStringFIX44, SenderCompID: "S", TargetCompID: "T"},
		MsgType: "AJ", Stage: "socket_write", Duration: time.Nanosecond, Success: true}
	var first, second atomic.Uint64
	var invalid atomic.Bool
	firstObserver := func(event SendStageEvent) {
		if event != expected {
			invalid.Store(true)
		}
		first.Add(1)
	}
	secondObserver := func(event SendStageEvent) {
		if event != expected {
			invalid.Store(true)
		}
		second.Add(1)
	}
	start := make(chan struct{})
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		<-start
		for iteration := 0; iteration < 200; iteration++ {
			SetSendStageObserver(firstObserver)
			SetSendStageObserver(nil)
			SetSendStageObserver(secondObserver)
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		<-start
		for iteration := 0; iteration < 600; iteration++ {
			observeSendStage(expected)
		}
	}()
	close(start)
	scopedBackpressureWait(t, done, "observer replacement did not finish")
	scopedBackpressureWait(t, done, "concurrent observation did not finish")
	beforeFirst, beforeSecond := first.Load(), second.Load()
	SetSendStageObserver(firstObserver)
	observeSendStage(expected)
	SetSendStageObserver(secondObserver)
	observeSendStage(expected)
	SetSendStageObserver(nil)
	observeSendStage(expected)
	if invalid.Load() || first.Load() != beforeFirst+1 || second.Load() != beforeSecond+1 {
		t.Fatalf("replacement/disable dispatch invalid: first=%d second=%d invalid=%v",
			first.Load(), second.Load(), invalid.Load())
	}
}
