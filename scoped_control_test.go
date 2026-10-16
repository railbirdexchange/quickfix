package quickfix

import (
	"bytes"
	"runtime"
	"testing"

	"github.com/quickfixgo/quickfix/config"
	"github.com/quickfixgo/quickfix/internal"
)

type scopedControlApplication struct {
	scopedOutboundBenchmarkApplication
	hook  func(*Message)
	calls int
}

func (application *scopedControlApplication) ToAdmin(message *Message, _ SessionID) {
	application.calls++
	if application.hook != nil {
		application.hook(message)
	}
}

func (application *scopedControlApplication) ToApp(message *Message, id SessionID) error {
	application.ToAdmin(message, id)
	return nil
}

func newScopedControlFixture(t testing.TB) (*scopedOutboundBenchmark, *scopedControlApplication) {
	t.Helper()
	fixture := newScopedOutboundBenchmark(t, false, "")
	application := &scopedControlApplication{}
	fixture.session.application = application
	return fixture, application
}

func completeScopedControl(fixture *scopedOutboundBenchmark) error {
	<-fixture.session.messageEvent
	if len(fixture.session.outbound.vacant) != len(fixture.session.outbound.slots) {
		return ErrOutboundCapacity
	}
	fixture.peer.Reset()
	return fixture.peer.Parse(fixture.connection.wire[:fixture.connection.used])
}

func TestScopedControlAcquisitionAndCopies(t *testing.T) {
	current := &session{}
	message, err := current.acquireControl("0")
	if err != nil {
		t.Fatal(err)
	}
	message.Body.SetString(tagTestReqID, "retained")
	copy := NewMessage()
	message.CopyInto(copy)
	if next, err := current.acquireControl("1"); err != ErrOutboundCapacity || next != nil {
		t.Fatalf("nested acquisition: message=%p error=%v", next, err)
	}
	if value, _ := message.Body.GetString(tagTestReqID); value != "retained" {
		t.Fatal("nested acquisition changed the active callback view")
	}
	current.releaseControl()
	if value, _ := message.Body.GetString(tagTestReqID); value != "retained" {
		t.Fatal("release cleared the completed callback view")
	}
	next, err := current.acquireControl("1")
	if err != nil || next != message || next.Body.Has(tagTestReqID) {
		t.Fatalf("reuse did not clear the previous control: same=%v error=%v", next == message, err)
	}
	next.Body.SetString(tagTestReqID, "replacement")
	if value, _ := copy.Body.GetString(tagTestReqID); value != "retained" {
		t.Fatal("owned copy borrowed reusable control storage")
	}
	current.releaseControl()
}

func TestScopedControlRestoresOwnerAfterCallbackReplacement(t *testing.T) {
	for _, wholeMessage := range []bool{false, true} {
		name := "body"
		if wholeMessage {
			name = "parsed_message"
		}
		t.Run(name, func(t *testing.T) {
			fixture, application := newScopedControlFixture(t)
			defer fixture.close()
			template := NewMessage()
			template.Body.SetString(tagTestReqID, "caller-owned")
			var frame, originalFrame []byte
			if wholeMessage {
				fixture.message.Body.SetString(tagTestReqID, "caller-owned")
				var err error
				frame, err = fixture.message.AppendTo(make([]byte, 0, DefaultParseLimits().MaxFrameBytes))
				if err != nil {
					t.Fatal(err)
				}
				originalFrame = bytes.Clone(frame)
				slot, err := NewMessageSlot(nil, nil, DefaultParseLimits())
				if err != nil {
					t.Fatal(err)
				}
				slot.enableScopedCallbacks()
				if err := slot.Parse(frame); err != nil {
					t.Fatal(err)
				}
				template = slot.Message()
			}
			retained := NewMessage()
			application.hook = func(message *Message) {
				if wholeMessage {
					*message = *template
				} else {
					message.Body = template.Body
				}
				message.CopyInto(retained)
			}
			if err := fixture.session.sendHeartbeat(nil, nil); err != nil {
				t.Fatal(err)
			}
			message, err := fixture.session.acquireControl("1")
			if err != nil {
				t.Fatal(err)
			}
			if message.parseSlot != nil || message.inbound != nil || message.rawMessage != nil || len(message.fields) != 0 || len(message.bodyBytes) != 0 {
				t.Fatal("control retained the replacement message's parse/frame state")
			}
			if message.arena != fixture.session.controls.original.arena || !message.arena.intact() || message.outboundFields != fixture.session.controls.original.outboundFields || message.outboundFields.owner != message {
				t.Fatal("control did not restore its original arena and engine storage")
			}
			message.Body.SetString(tagTestReqID, "next-control")
			fixture.session.releaseControl()
			if value, _ := template.Body.GetString(tagTestReqID); value != "caller-owned" {
				t.Fatalf("control reuse changed the caller template: %q", value)
			}
			if !bytes.Equal(frame, originalFrame) {
				t.Fatal("control reuse changed the caller's parsed frame")
			}
			if value, _ := retained.Body.GetString(tagTestReqID); value != "caller-owned" {
				t.Fatal("control reuse changed an owned callback copy")
			}
			if err := completeScopedControl(fixture); err != nil {
				t.Fatal(err)
			}
			if value, _ := fixture.peer.Message().Body.GetString(tagTestReqID); value != "caller-owned" {
				t.Fatal("control reuse changed the completed output")
			}
		})
	}
}

func TestScopedControlHeartbeatOwnsRequestID(t *testing.T) {
	fixture, application := newScopedControlFixture(t)
	defer fixture.close()
	fixture.message.Body.SetString(tagTestReqID, "request-id")
	frame, err := fixture.message.AppendTo(make([]byte, 0, DefaultParseLimits().MaxFrameBytes))
	if err != nil {
		t.Fatal(err)
	}
	original := bytes.Clone(frame)
	request, err := NewMessageSlot(nil, nil, DefaultParseLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := request.Parse(frame); err != nil {
		t.Fatal(err)
	}
	application.hook = func(message *Message) {
		value, _ := message.Body.GetBytes(tagTestReqID)
		value[0] = 'X'
		if source, _ := request.Message().Body.GetString(tagTestReqID); source != "request-id" {
			t.Fatalf("callback getter mutation changed the request: %q", source)
		}
	}
	id, _ := request.Message().Body.GetBytes(tagTestReqID)
	if err := fixture.session.sendHeartbeat(request.Message(), id); err != nil {
		t.Fatal(err)
	}
	if err := completeScopedControl(fixture); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frame, original) {
		t.Fatal("callback getter mutation changed the inbound frame")
	}
	if value, _ := fixture.peer.Message().Body.GetString(tagTestReqID); value != "request-id" {
		t.Fatalf("getter mutation changed separately encoded wire: %q", value)
	}
}

func TestScopedControlRejectOwnsRouteAndReference(t *testing.T) {
	// Location routing and RefMsgType belong to FIX 4.2 and later. The grouped
	// fixture uses FIX 4.4; the scalar fixture deliberately uses FIX 4.0.
	fixture := newScopedOutboundBenchmark(t, true, "")
	application := &scopedControlApplication{}
	fixture.session.application = application
	defer fixture.close()
	routes := [...][2]Tag{
		{tagSenderCompID, tagTargetCompID}, {tagSenderSubID, tagTargetSubID},
		{tagSenderLocationID, tagTargetLocationID}, {tagTargetCompID, tagSenderCompID},
		{tagTargetSubID, tagSenderSubID}, {tagTargetLocationID, tagSenderLocationID},
		{tagOnBehalfOfCompID, tagDeliverToCompID}, {tagOnBehalfOfSubID, tagDeliverToSubID},
		{tagDeliverToCompID, tagOnBehalfOfCompID}, {tagDeliverToSubID, tagOnBehalfOfSubID},
		{tagOnBehalfOfLocationID, tagDeliverToLocationID}, {tagDeliverToLocationID, tagOnBehalfOfLocationID},
	}
	for _, route := range routes {
		fixture.message.Header.SetInt(route[0], int(route[0]))
	}
	frame, err := fixture.message.AppendTo(make([]byte, 0, DefaultParseLimits().MaxFrameBytes))
	if err != nil {
		t.Fatal(err)
	}
	original := bytes.Clone(frame)
	request, err := NewMessageSlot(nil, nil, DefaultParseLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := request.Parse(frame); err != nil {
		t.Fatal(err)
	}
	var expected [len(routes)][]byte
	application.hook = func(message *Message) {
		for index, route := range routes {
			value, err := message.Header.GetBytes(route[1])
			if err != nil {
				t.Fatal(err)
			}
			expected[index] = bytes.Clone(value)
			value[0] = 'X'
		}
		value, _ := message.Body.GetBytes(tagRefMsgType)
		value[0] = 'X'
		if !bytes.Equal(frame, original) {
			t.Fatal("reject callback getter mutation changed the source frame")
		}
	}
	reject := &messageRejectError{text: "Incorrect value", rejectReason: 5}
	if err := fixture.session.doReject(request.Message(), reject); err != nil {
		t.Fatal(err)
	}
	if err := completeScopedControl(fixture); err != nil {
		t.Fatal(err)
	}
	for index, route := range routes {
		value, _ := fixture.peer.Message().Header.GetBytes(route[1])
		if !bytes.Equal(value, expected[index]) {
			t.Fatalf("route %d getter mutation changed encoded wire: %q", route[1], value)
		}
	}
	if value, _ := fixture.peer.Message().Body.GetString(tagRefMsgType); value != "D" {
		t.Fatalf("reference getter mutation changed encoded wire: %q", value)
	}
}

func TestScopedControlOwnedBytesStageOverlappingSource(t *testing.T) {
	current := &session{}
	message, err := current.acquireControl("0")
	if err != nil {
		t.Fatal(err)
	}
	defer current.releaseControl()
	message.Body.SetString(tagTestReqID, "x")
	message.Body.SetString(tagText, "later-value")
	source, _ := message.Body.GetBytes(tagText)
	setControlBytes(&message.Body.FieldMap, tagTestReqID, source)
	for _, tag := range [...]Tag{tagTestReqID, tagText} {
		if value, _ := message.Body.GetString(tag); value != "later-value" {
			t.Fatalf("overlapping resize damaged field %d: %q", tag, value)
		}
	}
	value, _ := message.Body.GetBytes(tagTestReqID)
	value[0] = 'X'
	if value, _ := message.Body.GetString(tagText); value != "later-value" {
		t.Fatal("owned target value aliases its source field")
	}
}

func TestScopedControlInvalidLimitsFailSessionSetup(t *testing.T) {
	settings := NewSessionSettings()
	settings.Set(config.BoundedMaxFields, "3")
	_, err := (sessionFactory{}).newSession(
		SessionID{BeginString: BeginStringFIX42, SenderCompID: "sender", TargetCompID: "target"},
		NewMemoryStoreFactory(), settings, NewNullLogFactory(), &scopedControlApplication{},
	)
	if err != ErrInvalidParseLimits {
		t.Fatalf("small control limits: %v", err)
	}
}

func TestScopedControlCallbackOwnershipAndCompletedWrite(t *testing.T) {
	fixture, application := newScopedControlFixture(t)
	defer fixture.close()
	retained := NewMessage()
	sequence := fixture.store.NextSenderMsgSeqNum()
	application.hook = func(message *Message) {
		if message != fixture.session.controls.message || !fixture.session.controls.busy {
			t.Fatal("callback does not own the acquired control")
		}
		if err := fixture.session.sendTestRequest(); err != ErrOutboundCapacity {
			t.Fatalf("nested control did not fail before reentering send ownership: %v", err)
		}
		message.Header.SetInt(tagMsgSeqNum, 777)
		message.Body.SetString(tagTestReqID, "callback-value")
		message.CopyInto(retained)
	}
	if err := fixture.session.sendHeartbeat(nil, nil); err != nil {
		t.Fatal(err)
	}
	// The synchronous send scope has ended. Reuse it before waiting for the
	// writer; the socket still owns only its independent fixed output slot.
	message, err := fixture.session.acquireControl("1")
	if err != nil {
		t.Fatal(err)
	}
	message.Body.SetString(tagTestReqID, "next-control")
	fixture.session.releaseControl()
	if err := completeScopedControl(fixture); err != nil {
		t.Fatal(err)
	}
	if value, _ := fixture.peer.Message().Body.GetString(tagTestReqID); value != "callback-value" {
		t.Fatalf("writer observed recycled control bytes: %q", value)
	}
	if value, _ := fixture.peer.Message().Header.GetInt(tagMsgSeqNum); value != 777 {
		t.Fatalf("callback wire sequence was overwritten: %d", value)
	}
	if value, _ := retained.Body.GetString(tagTestReqID); value != "callback-value" {
		t.Fatal("callback CopyInto did not detach")
	}
	if fixture.store.NextSenderMsgSeqNum() != sequence+1 {
		t.Fatal("nested acquisition or callback sequence edit changed the persistence sequence")
	}
	if retainedWire, present := fixture.store.message(sequence); !present || !bytes.Equal(retainedWire, fixture.connection.wire[:fixture.connection.used]) {
		t.Fatal("persisted wire differs from the completed write")
	}
}

func TestScopedControlCapacityFailureReleasesScope(t *testing.T) {
	fixture, application := newScopedControlFixture(t)
	defer fixture.close()
	sequence, target := fixture.store.NextSenderMsgSeqNum(), fixture.store.NextTargetMsgSeqNum()
	oversized := string(bytes.Repeat([]byte{'x'}, fixture.session.boundedIn.limits.MaxFrameBytes))
	application.hook = func(message *Message) { message.Body.SetString(tagText, oversized) }
	if err := fixture.session.sendHeartbeat(nil, nil); err != ErrFrameCapacity {
		t.Fatalf("oversized callback result: %v", err)
	}
	if fixture.session.controls.busy || fixture.store.NextSenderMsgSeqNum() != sequence || fixture.store.NextTargetMsgSeqNum() != target {
		t.Fatal("failed control retained ownership or changed sequence state")
	}
	if len(fixture.session.outbound.vacant) != len(fixture.session.outbound.slots) || fixture.connection.used != 0 {
		t.Fatal("failed control retained or wrote an output slot")
	}
	application.hook = nil
	if err := fixture.session.sendHeartbeat(nil, nil); err != nil {
		t.Fatalf("next control could not recover after clearing its failed scope: %v", err)
	}
	if err := completeScopedControl(fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.peer.Message().Body.Has(tagText) {
		t.Fatal("failed control field leaked into the next heartbeat")
	}
}

func TestScopedControlGapFillPreservesSequenceAndOriginalTime(t *testing.T) {
	fixture, application := newScopedControlFixture(t)
	defer fixture.close()
	application.hook = func(message *Message) {
		sent, _ := message.Header.GetBytes(tagSendingTime)
		original, _ := message.Header.GetBytes(tagOrigSendingTime)
		first := sent[0]
		original[0] = 'X'
		if sent[0] != first {
			t.Fatal("original time getter aliases SendingTime")
		}
	}
	sender, target := fixture.store.NextSenderMsgSeqNum(), fixture.store.NextTargetMsgSeqNum()
	if err := fixture.session.generateSequenceReset(14, 29, fixture.message); err != nil {
		t.Fatal(err)
	}
	if err := completeScopedControl(fixture); err != nil {
		t.Fatal(err)
	}
	message := fixture.peer.Message()
	if sequence, _ := message.Header.GetInt(tagMsgSeqNum); sequence != 14 {
		t.Fatalf("gapfill sequence: %d", sequence)
	}
	if next, _ := message.Body.GetInt(tagNewSeqNo); next != 29 {
		t.Fatalf("gapfill next sequence: %d", next)
	}
	if dup, _ := message.Header.GetBool(tagPossDupFlag); !dup {
		t.Fatal("gapfill missing PossDupFlag")
	}
	if gap, _ := message.Body.GetBool(tagGapFillFlag); !gap {
		t.Fatal("gapfill missing GapFillFlag")
	}
	sent, _ := message.Header.GetBytes(tagSendingTime)
	original, _ := message.Header.GetBytes(tagOrigSendingTime)
	if !bytes.Equal(sent, original) {
		t.Fatalf("gapfill original time: %q != %q", original, sent)
	}
	if fixture.store.NextSenderMsgSeqNum() != sender || fixture.store.NextTargetMsgSeqNum() != target {
		t.Fatal("gapfill changed committed sender or target sequence")
	}
}

func TestScopedControlEmptyTestRequestIDAndTimeoutState(t *testing.T) {
	fixture, _ := newScopedControlFixture(t)
	defer fixture.close()
	if err := fixture.session.sendHeartbeat(fixture.message, nil); err != nil {
		t.Fatal(err)
	}
	if err := completeScopedControl(fixture); err != nil {
		t.Fatal(err)
	}
	if value, err := fixture.peer.Message().Body.GetBytes(tagTestReqID); err != nil || len(value) != 0 {
		t.Fatalf("empty but present TestReqID: %q %v", value, err)
	}
	fixture.connection.used = 0
	fixture.floor++
	if err := fixture.store.RetireBefore(fixture.floor); err != nil {
		t.Fatal(err)
	}
	state := (inSession{}).Timeout(fixture.session, internal.PeerTimeout)
	if state != &fixture.session.boundedIn.pending || fixture.session.controls.busy {
		t.Fatal("peer timeout did not release control ownership into the fixed timeout state")
	}
	if err := completeScopedControl(fixture); err != nil {
		t.Fatal(err)
	}
}

func TestScopedControlWarmedAllocations(t *testing.T) {
	refTag := Tag(11)
	reject := &messageRejectError{text: "Incorrect value", rejectReason: 5, refTagID: &refTag}
	for _, scenario := range []struct {
		name string
		send func(*session, *Message) error
	}{
		{"heartbeat", func(s *session, _ *Message) error { return s.sendHeartbeat(nil, nil) }},
		{"test_request", func(s *session, _ *Message) error { return s.sendTestRequest() }},
		{"resend_request", func(s *session, _ *Message) error { _, err := s.sendResendRequest(10, 20); return err }},
		{"gap_fill", func(s *session, message *Message) error { return s.generateSequenceReset(10, 20, message) }},
		{"logout", func(s *session, _ *Message) error { return s.sendLogout("Completed") }},
		{"logon", func(s *session, _ *Message) error { return s.sendLogonInReplyTo(false, nil) }},
		{"reject", func(s *session, message *Message) error { return s.doReject(message, reject) }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture, _ := newScopedControlFixture(t)
			defer fixture.close()
			cycle := func() error {
				sequence := fixture.store.NextSenderMsgSeqNum()
				if err := scenario.send(fixture.session, fixture.message); err != nil {
					return err
				}
				if err := completeScopedControl(fixture); err != nil {
					return err
				}
				fixture.connection.used = 0
				if fixture.store.NextSenderMsgSeqNum() != sequence {
					fixture.floor++
					return fixture.store.RetireBefore(fixture.floor)
				}
				return nil
			}
			for warmup := 0; warmup < 66; warmup++ {
				if err := cycle(); err != nil {
					t.Fatal(err)
				}
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			var operationError error
			for iteration := 0; iteration < 100; iteration++ {
				if operationError = cycle(); operationError != nil {
					break
				}
			}
			runtime.ReadMemStats(&after)
			if operationError != nil {
				t.Fatal(operationError)
			}
			if allocations, allocatedBytes := after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc; allocations != 0 || allocatedBytes != 0 {
				t.Fatalf("100 completed control writes allocated %d objects / %d bytes", allocations, allocatedBytes)
			}
		})
	}
}
