package quickfix

import "testing"

func TestHeartbeatObserverNilAndEmptyTypeSnapshot(t *testing.T) {
	for _, test := range []struct {
		name  string
		value []byte
		stage string
		count int
	}{
		{name: "nil", stage: "to_app"},
		{name: "empty", value: []byte{}, stage: "to_app", count: 3},
		{name: "heartbeat", value: []byte("0"), stage: "to_admin", count: 3},
		{name: "custom", value: []byte("CUSTOM.TYPE"), stage: "to_app", count: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newScopedOutboundBenchmark(t, false, "D")
			defer fixture.close()
			fixture.message.Header.SetBytes(tagMsgType, test.value)
			recorded := make(chan SendStageEvent, 16)
			SetSendStageObserver(func(event SendStageEvent) { recorded <- event })
			defer SetSendStageObserver(nil)
			// Prepare directly: nil/empty MsgType values are intentionally invalid
			// wire input. This tests their existing observation behavior without
			// requiring the peer parser to accept them.
			fixture.session.sendMutex.Lock()
			slot, err := fixture.session.prepareOutbound(fixture.message, nil, false)
			fixture.session.sendMutex.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			slot.release()
			events := heartbeatLegacyStageEvents(recorded)
			if len(events) != test.count {
				t.Fatalf("events=%+v, want %d prepared-stage events", events, test.count)
			}
			want := []string{test.stage, "build_message", "persist"}
			for index, event := range events {
				if event.Stage != want[index] || event.MsgType != string(test.value) || !event.Success || event.SessionID != fixture.session.sessionID {
					t.Fatalf("prepared event %d changed semantics: %+v", index, event)
				}
			}
		})
	}
}

func TestHeartbeatObserverRetainsOriginalTypeAcrossCallbackAndReuse(t *testing.T) {
	const original = "ORIGINAL.CUSTOM.APPLICATION.TYPE"
	fixture := newScopedOutboundBenchmarkWithDelivery(t, false, original, true)
	defer fixture.close()
	// Establish the reusable construction arena before observing mutations.
	for iteration := 0; iteration < 2; iteration++ {
		if err := fixture.process(); err != nil {
			t.Fatal(err)
		}
	}
	application := &scopedOutboundHookApplication{scopedOutboundBenchmarkApplication: fixture.application}
	fixture.session.application = BorrowApplication(application)
	recorded := make(chan SendStageEvent, 32)
	SetSendStageObserver(func(event SendStageEvent) { recorded <- event })
	defer SetSendStageObserver(nil)
	inputs := []string{original, "SECOND.CUSTOM.APPLICATION.TYPE"}
	wires := []string{"CALLBACK.REPLACED.APPLICATION.TYPE", "SECOND.CALLBACK.WIRE.TYPE"}
	for index, input := range inputs {
		fixture.message.Header.SetString(tagMsgType, input)
		application.toApp = func(message *Message) error {
			message.Header.SetString(tagMsgType, wires[index])
			return nil
		}
		if err := fixture.process(); err != nil {
			t.Fatal(err)
		}
	}
	value, reject := fixture.message.Header.GetBytes(tagMsgType)
	if reject != nil {
		t.Fatal(reject)
	}
	for index := range value {
		value[index] = '?'
	}
	events := heartbeatLegacyStageEvents(recorded)
	want := []string{"to_app", "build_message", "persist", "outbound_channel_wait"}
	if len(events) != 2*len(want) {
		t.Fatalf("unexpected observed stages: %+v", events)
	}
	for index, event := range events {
		send, stage := index/len(want), index%len(want)
		messageType := inputs[send]
		if want[stage] == "outbound_channel_wait" {
			messageType = wires[send]
		}
		if event.Stage != want[stage] || event.MsgType != messageType || !event.Success || event.SessionID != fixture.session.sessionID {
			t.Fatalf("retained event %d changed after callback/arena reuse: %+v, want type %q", index, event, messageType)
		}
	}
}

type heartbeatObserverHookStore struct {
	MessageStore
	beforeSave func()
}

func (store *heartbeatObserverHookStore) SaveMessageAndIncrNextSenderMsgSeqNum(sequence int, wire []byte) error {
	store.beforeSave()
	return store.MessageStore.SaveMessageAndIncrNextSenderMsgSeqNum(sequence, wire)
}

func TestHeartbeatObserverEnableTransitions(t *testing.T) {
	for _, mode := range []string{"enable_in_callback", "disable_in_callback", "replace_in_callback", "enable_during_untimed_persist"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newScopedOutboundBenchmark(t, false, "CUSTOM.TYPE")
			defer fixture.close()
			originalRecorded, replacementRecorded := make(chan SendStageEvent, 16), make(chan SendStageEvent, 16)
			originalObserver := func(event SendStageEvent) { originalRecorded <- event }
			replacementObserver := func(event SendStageEvent) { replacementRecorded <- event }
			SetSendStageObserver(originalObserver)
			defer SetSendStageObserver(nil)
			application := &scopedOutboundHookApplication{scopedOutboundBenchmarkApplication: fixture.application}
			fixture.session.application = application
			want := []string{"outbound_channel_wait"}
			switch mode {
			case "enable_in_callback":
				SetSendStageObserver(nil)
				application.toApp = func(*Message) error {
					SetSendStageObserver(replacementObserver)
					return nil
				}
			case "disable_in_callback":
				want = nil
				application.toApp = func(*Message) error {
					SetSendStageObserver(nil)
					return nil
				}
			case "replace_in_callback":
				want = []string{"to_app", "build_message", "persist", "outbound_channel_wait"}
				application.toApp = func(*Message) error {
					SetSendStageObserver(replacementObserver)
					return nil
				}
			case "enable_during_untimed_persist":
				application.toApp = func(*Message) error {
					SetSendStageObserver(nil)
					return nil
				}
				// An external store supplies a deterministic boundary after
				// persist's startSendStage returned zero. Enabling at finish must
				// not invent a duration/event for that untimed stage.
				fixture.session.store = &heartbeatObserverHookStore{MessageStore: fixture.store, beforeSave: func() {
					SetSendStageObserver(replacementObserver)
				}}
				if err := fixture.session.configureOutbound(NewSessionSettings()); err != nil {
					t.Fatal(err)
				}
			}
			if err := fixture.process(); err != nil {
				t.Fatal(err)
			}
			originalEvents := heartbeatLegacyStageEvents(originalRecorded)
			replacementEvents := heartbeatLegacyStageEvents(replacementRecorded)
			if len(originalEvents) != 0 || len(replacementEvents) != len(want) {
				t.Fatalf("observer transition changed event delivery: original=%+v replacement=%+v want=%v", originalEvents, replacementEvents, want)
			}
			for index, event := range replacementEvents {
				if event.Stage != want[index] || event.MsgType != "CUSTOM.TYPE" || !event.Success || event.SessionID != fixture.session.sessionID {
					t.Fatalf("transition event %d changed: %+v", index, event)
				}
			}
		})
	}
}

// These fixtures preserve the original prepared/handoff event semantics.
// Writer lifecycle events are checked separately and may overlap the handoff
// callback, so collection uses a bounded channel rather than a shared slice.
func heartbeatLegacyStageEvents(recorded <-chan SendStageEvent) []SendStageEvent {
	var events []SendStageEvent
	for {
		select {
		case event := <-recorded:
			switch event.Stage {
			case "outbound_queue_wait", "socket_write", "outbound_complete":
			default:
				events = append(events, event)
			}
		default:
			return events
		}
	}
}
