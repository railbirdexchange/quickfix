package quickfix

import (
	"bytes"
	"testing"
)

type scopedOutboundHookApplication struct {
	*scopedOutboundBenchmarkApplication
	toApp func(*Message) error
}

func (application *scopedOutboundHookApplication) ToApp(message *Message, sessionID SessionID) error {
	if err := application.scopedOutboundBenchmarkApplication.ToApp(message, sessionID); err != nil {
		return err
	}
	if application.toApp != nil {
		return application.toApp(message)
	}
	return nil
}

func TestOrdinaryOutboundCallerOwnershipAndCallbackSequence(t *testing.T) {
	fixture := newScopedOutboundBenchmark(t, false, "")
	defer fixture.close()
	application := &scopedOutboundHookApplication{scopedOutboundBenchmarkApplication: fixture.application}
	fixture.session.application = application
	application.toApp = func(message *Message) error {
		if message != fixture.message {
			t.Fatal("ToApp did not receive the caller's original Message")
		}
		message.Header.SetInt(tagMsgSeqNum, 777)
		message.Body.SetString(11, "callback-order")
		return nil
	}
	sequence := fixture.store.NextSenderMsgSeqNum()
	if err := SendToTarget(fixture.message, fixture.session.sessionID); err != nil {
		t.Fatal(err)
	}
	// Send has finished encoding and persistence. The caller may mutate its
	// original Message while the independently owned output slot is queued.
	fixture.message.Body.SetString(11, "caller-after-send")
	<-fixture.session.messageEvent
	fixture.session.sendMutex.Lock()
	fixture.session.sendQueued(true)
	fixture.session.sendMutex.Unlock()
	<-fixture.session.messageEvent
	completed := fixture.connection.wire[:fixture.connection.used]
	if err := fixture.peer.Parse(completed); err != nil {
		t.Fatal(err)
	}
	peer := fixture.peer.Message()
	if got, _ := peer.Header.GetInt(tagMsgSeqNum); got != 777 {
		t.Fatalf("callback sequence overwritten: %d", got)
	}
	if got, _ := peer.Body.GetString(11); got != "callback-order" {
		t.Fatalf("writer observed post-Send mutation: %q", got)
	}
	if got, _ := fixture.message.Body.GetString(11); got != "caller-after-send" {
		t.Fatalf("caller lost Message ownership: %q", got)
	}
	for _, item := range []struct {
		tag          Tag
		fields, peer *FieldMap
	}{
		{tagBodyLength, &fixture.message.Header.FieldMap, &peer.Header.FieldMap},
		{tagCheckSum, &fixture.message.Trailer.FieldMap, &peer.Trailer.FieldMap},
	} {
		got, reject := item.fields.GetBytes(item.tag)
		want, peerReject := item.peer.GetBytes(item.tag)
		if reject != nil || peerReject != nil || !bytes.Equal(got, want) {
			t.Fatalf("observable framing tag %d: got %q (%v), want %q (%v)", item.tag, got, reject, want, peerReject)
		}
	}
	retained, present := fixture.store.message(sequence)
	if !present || !bytes.Equal(retained, completed) || fixture.store.NextSenderMsgSeqNum() != sequence+1 {
		t.Fatal("callback-edited wire sequence changed the legacy persistence key or sender increment")
	}
	if len(fixture.session.outbound.vacant) != len(fixture.session.outbound.slots) {
		t.Fatal("completed write retained an output slot")
	}
}

func TestOrdinaryOutboundCanonicalIdentity(t *testing.T) {
	fixture := newScopedOutboundBenchmark(t, false, "")
	defer fixture.close()
	identity := fixture.session.sessionID.SenderCompID
	stale := bytes.Repeat([]byte{'?'}, len(identity))
	fixture.message.Header.SetBytes(tagSenderCompID, stale)
	value, _ := fixture.message.Header.GetBytes(tagSenderCompID)
	copy(value, identity)
	// GetBytes now matches the session identity, but the setter's separately
	// encoded TagValue still contains question marks. Header preparation must
	// canonicalize both representations before it can skip this setter.
	if err := fixture.process(); err != nil {
		t.Fatal(err)
	}
	values, _ := fixture.message.Header.lookup(tagSenderCompID)
	if !bytes.Equal(values[0].bytes, append(append([]byte("49="), identity...), '\x01')) {
		t.Fatalf("noncanonical sender identity survived preparation: %q", values[0].bytes)
	}
}

func TestOrdinaryOutboundGetterMutationDoesNotRewriteWire(t *testing.T) {
	fixture := newScopedOutboundBenchmark(t, false, "")
	defer fixture.close()
	fixture.session.EnableLastMsgSeqNumProcessed = true
	application := &scopedOutboundHookApplication{scopedOutboundBenchmarkApplication: fixture.application}
	fixture.session.application = application
	want := make(map[Tag]string)
	application.toApp = func(message *Message) error {
		for _, tag := range []Tag{tagMsgSeqNum, tagSendingTime, tagLastMsgSeqNumProcessed} {
			value, reject := message.Header.GetBytes(tag)
			if reject != nil {
				return reject
			}
			want[tag] = string(value)
			for index := range value {
				value[index] = '9'
			}
		}
		return nil
	}
	if err := SendToTarget(fixture.message, fixture.session.sessionID); err != nil {
		t.Fatal(err)
	}
	<-fixture.session.messageEvent
	fixture.session.sendMutex.Lock()
	fixture.session.sendQueued(true)
	fixture.session.sendMutex.Unlock()
	<-fixture.session.messageEvent
	if err := fixture.peer.Parse(fixture.connection.wire[:fixture.connection.used]); err != nil {
		t.Fatal(err)
	}
	for tag, expected := range want {
		if actual, _ := fixture.peer.Message().Header.GetString(tag); actual != expected {
			t.Fatalf("GetBytes(%d) mutation rewrote wire: %q != %q", tag, actual, expected)
		}
		getter, _ := fixture.message.Header.GetBytes(tag)
		if !bytes.Equal(getter, bytes.Repeat([]byte{'9'}, len(getter))) {
			t.Fatalf("GetBytes(%d) lost its mutable getter semantics: %q", tag, getter)
		}
	}
}

func TestOrdinaryOutboundCopiesDetachEngineFields(t *testing.T) {
	fixture := newScopedOutboundBenchmark(t, true, "")
	defer fixture.close()
	if err := fixture.process(); err != nil {
		t.Fatal(err)
	}
	owned := NewMessage()
	fixture.message.CopyInto(owned)
	valueCopy := *fixture.message
	fixture.message.CopyInto(&valueCopy)
	var fields FieldMap
	fields.init()
	fixture.message.Header.CopyInto(&fields)
	previous, _ := fixture.message.Header.GetString(tagMsgSeqNum)
	previousChecksum, _ := fixture.message.Trailer.GetString(tagCheckSum)
	if err := fixture.process(); err != nil {
		t.Fatal(err)
	}
	for _, copied := range []*Message{owned, &valueCopy} {
		if got, _ := copied.Header.GetString(tagMsgSeqNum); got != previous {
			t.Fatalf("copied sequence aliases the engine workspace: %q != %q", got, previous)
		}
		if got, _ := copied.Trailer.GetString(tagCheckSum); got != previousChecksum {
			t.Fatalf("copied checksum aliases the engine workspace: %q != %q", got, previousChecksum)
		}
	}
	if got, _ := fields.GetString(tagMsgSeqNum); got != previous {
		t.Fatalf("copied section aliases the engine workspace: %q != %q", got, previous)
	}
}

type scopedOutboundRetainingLog struct {
	Log
	wires [][]byte
}

func (log *scopedOutboundRetainingLog) OnOutgoing(wire []byte) {
	log.wires = append(log.wires, wire)
}

func TestOrdinaryOutboundGenericStoreAndLoggerRetainOwnedBytes(t *testing.T) {
	fixture := newScopedOutboundBenchmark(t, false, "")
	defer fixture.close()
	store, err := NewMemoryStoreFactory().Create(fixture.session.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.session.store = store
	fixture.session.outbound.journal = nil
	log := &scopedOutboundRetainingLog{Log: fixture.session.log}
	fixture.session.log, fixture.session.outbound.log = log, nil
	var first []byte
	for iteration := 0; iteration < 70; iteration++ {
		if err := SendToTarget(fixture.message, fixture.session.sessionID); err != nil {
			t.Fatal(err)
		}
		<-fixture.session.messageEvent
		fixture.session.sendMutex.Lock()
		fixture.session.sendQueued(true)
		fixture.session.sendMutex.Unlock()
		<-fixture.session.messageEvent
		if iteration == 0 {
			first = bytes.Clone(fixture.connection.wire[:fixture.connection.used])
		}
		fixture.connection.used = 0
	}
	retained, err := store.GetMessages(1, 1)
	if err != nil || len(retained) != 1 || !bytes.Equal(retained[0], first) {
		t.Fatalf("generic store retained recycled output: %v", err)
	}
	if len(log.wires) != 70 || !bytes.Equal(log.wires[0], first) {
		t.Fatal("generic logger retained recycled output")
	}
}

func TestOrdinaryOutboundWarmedAllocations(t *testing.T) {
	for _, scenario := range []struct {
		name, messageType string
		groups            bool
	}{
		{name: "new_order_single"},
		{name: "repeating_group", groups: true},
		{name: "custom_message_type", messageType: "CUSTOM.APPLICATION.MESSAGE.TYPE"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newScopedOutboundBenchmarkWithDelivery(t, scenario.groups, scenario.messageType, true)
			defer fixture.close()
			for iteration := 0; iteration < 66; iteration++ {
				if err := fixture.process(); err != nil {
					t.Fatal(err)
				}
			}
			var operationError error
			allocations := testing.AllocsPerRun(100, func() { operationError = fixture.process() })
			if operationError != nil || allocations != 0 {
				t.Fatalf("completed ordinary send: error=%v allocations=%v", operationError, allocations)
			}
		})
	}
}

func TestOrdinaryOutboundSequenceSetterWaitsForReservation(t *testing.T) {
	fixture := newScopedOutboundBenchmark(t, false, "")
	defer fixture.close()
	entered, release := make(chan struct{}), make(chan struct{})
	application := &scopedOutboundHookApplication{scopedOutboundBenchmarkApplication: fixture.application}
	application.toApp = func(*Message) error {
		close(entered)
		<-release
		return nil
	}
	fixture.session.application = application
	sequence := fixture.store.NextSenderMsgSeqNum()
	sent := make(chan error, 1)
	go func() { sent <- SendToTarget(fixture.message, fixture.session.sessionID) }()
	<-entered
	// Getter calls remain usable inside a callback's sender reservation.
	if got, err := GetExpectedSenderNum(fixture.session.sessionID); err != nil || got != sequence {
		t.Fatalf("sender getter during preparation: %d, %v", got, err)
	}
	setterStarted, setterDone := make(chan struct{}), make(chan error, 1)
	go func() {
		close(setterStarted)
		setterDone <- SetNextSenderMsgSeqNum(fixture.session.sessionID, sequence+2)
	}()
	<-setterStarted
	close(release)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if err := <-setterDone; err != nil {
		t.Fatal(err)
	}
	if got := fixture.store.NextSenderMsgSeqNum(); got != sequence+2 {
		t.Fatalf("concurrent setter was overwritten by commit: %d", got)
	}
	fixture.session.sendMutex.Lock()
	fixture.session.outbound.dropPending()
	fixture.session.sendMutex.Unlock()
}

func TestOrdinaryOutboundObserverPreservesStagesAndOwnedType(t *testing.T) {
	const messageType = "CUSTOM.APPLICATION.MESSAGE.TYPE"
	fixture := newScopedOutboundBenchmark(t, false, messageType)
	defer fixture.close()
	var events []SendStageEvent
	SetSendStageObserver(func(event SendStageEvent) { events = append(events, event) })
	defer SetSendStageObserver(nil)
	if err := fixture.process(); err != nil {
		t.Fatal(err)
	}
	value, _ := fixture.message.Header.GetBytes(tagMsgType)
	for index := range value {
		value[index] = '?'
	}
	want := []string{"to_app", "build_message", "persist", "outbound_channel_wait"}
	if len(events) != len(want) {
		t.Fatalf("observer stages: %+v", events)
	}
	for index, event := range events {
		if event.Stage != want[index] || event.MsgType != messageType || !event.Success || event.SessionID != fixture.session.sessionID {
			t.Fatalf("observer event %d lost its stage/value semantics: %+v", index, event)
		}
	}
}
