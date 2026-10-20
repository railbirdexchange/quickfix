package quickfix

import (
	"testing"

	"github.com/quickfixgo/quickfix/config"
)

var sessionHeaderStringTags = [...]Tag{
	tagBeginString, tagSenderCompID, tagSenderSubID, tagSenderLocationID,
	tagTargetCompID, tagTargetSubID, tagTargetLocationID,
}

func sessionHeaderStringID(suffix string) SessionID {
	return SessionID{
		BeginString:  BeginStringFIX44,
		SenderCompID: "RAILBIRD-" + suffix, SenderSubID: "local-desk-" + suffix, SenderLocationID: "local-location-" + suffix,
		TargetCompID: "PREPRODFCM-" + suffix, TargetSubID: "peer-desk-" + suffix, TargetLocationID: "peer-location-" + suffix,
		Qualifier: "header-strings-" + suffix,
	}
}

func sessionHeaderStringValues(id SessionID, inbound bool) [7]string {
	if inbound {
		return [7]string{id.BeginString, id.TargetCompID, id.TargetSubID, id.TargetLocationID, id.SenderCompID, id.SenderSubID, id.SenderLocationID}
	}
	return [7]string{id.BeginString, id.SenderCompID, id.SenderSubID, id.SenderLocationID, id.TargetCompID, id.TargetSubID, id.TargetLocationID}
}

func newSessionHeaderStringSession(test testing.TB, id SessionID) *session {
	test.Helper()
	settings := NewSessionSettings()
	settings.Set(config.DataDictionary, "spec/FIX44.xml")
	current, err := (sessionFactory{}).newSession(id, NewBoundedStoreFactory(16, 16384), settings, NewNullLogFactory(), BorrowApplication(&scopedControlApplication{}))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if err := current.closeStore(); err != nil {
			test.Error(err)
		}
	})
	return current
}

func sessionHeaderStringWire(test testing.TB, id SessionID, inbound bool) []byte {
	test.Helper()
	message := NewMessage()
	values := sessionHeaderStringValues(id, inbound)
	for index, tag := range sessionHeaderStringTags {
		message.Header.SetString(tag, values[index])
	}
	message.Header.SetString(tagMsgType, "0")
	message.Header.SetInt(tagMsgSeqNum, 2)
	message.Header.SetString(tagSendingTime, "20261019-12:00:00.000")
	wire, err := message.AppendTo(make([]byte, 0, 16384))
	if err != nil {
		test.Fatal(err)
	}
	return wire
}

func readSessionHeaderStrings(test testing.TB, fields *FieldMap) [7]string {
	test.Helper()
	var values [7]string
	for index, tag := range sessionHeaderStringTags {
		value, reject := fields.GetString(tag)
		if reject != nil {
			test.Fatalf("GetString(%d): %v", tag, reject)
		}
		values[index] = value
		if unlocked, reject := fields.getStringNoLock(tag); reject != nil || unlocked != value {
			test.Fatalf("unlocked GetString(%d) = %q, %v; want %q", tag, unlocked, reject, value)
		}
	}
	return values
}

func TestSessionHeaderStringsReadActualFieldsInBothDirections(t *testing.T) {
	id := sessionHeaderStringID("direction")
	current := newSessionHeaderStringSession(t, id)
	for _, inbound := range []bool{true, false} {
		slot := current.outbound.replay
		if inbound {
			slot = current.boundedIn.parsed
		}
		if err := slot.Parse(sessionHeaderStringWire(t, id, inbound)); err != nil {
			t.Fatal(err)
		}
		fields := &slot.Message().Header.FieldMap
		want := sessionHeaderStringValues(id, inbound)
		if got := readSessionHeaderStrings(t, fields); got != want {
			t.Fatalf("inbound=%v: got %q, want %q", inbound, got, want)
		}
		for index, tag := range sessionHeaderStringTags {
			fields.Remove(tag)
			if value, reject := fields.GetString(tag); value != "" || reject == nil {
				t.Fatalf("missing configured tag %d returned %q, %v", tag, value, reject)
			} else if reference, present := reject.RefTagID(); !present || reference != tag || reject.RejectReason() != 8 || !reject.IsBusinessReject() {
				t.Fatalf("missing configured tag %d changed reject: %v", tag, reject)
			}
			// An identity is only a reusable string when the actual bytes match.
			// Per-message SubIDs and all other mismatches remain owned snapshots.
			mismatch := []byte("different-" + want[index])
			fields.SetBytes(tag, mismatch)
			retained, reject := fields.GetString(tag)
			if reject != nil || retained != string(mismatch) {
				t.Fatalf("mismatched tag %d returned %q, %v", tag, retained, reject)
			}
			mismatch[0] = 'X'
			if retained != "different-"+want[index] {
				t.Fatalf("mismatched tag %d retained mutable bytes", tag)
			}
			fields.SetString(tag, want[index])
		}
	}
	// An empty configured SubID must not manufacture a present field.
	emptyID := sessionHeaderStringID("empty")
	emptyID.SenderSubID, emptyID.TargetSubID = "", ""
	emptySession := newSessionHeaderStringSession(t, emptyID)
	if _, reject := emptySession.controls.message.Header.GetString(tagSenderSubID); reject == nil {
		t.Fatal("empty configured SubID made an absent field present")
	}
}

func TestSessionHeaderStringsSurviveFrameReuseCopiesAndDetachment(t *testing.T) {
	id := sessionHeaderStringID("lifetime")
	current := newSessionHeaderStringSession(t, id)
	slot := current.boundedIn.parsed
	wire := sessionHeaderStringWire(t, id, true)
	if err := slot.Parse(wire); err != nil {
		t.Fatal(err)
	}
	message := slot.Message()
	want := sessionHeaderStringValues(id, true)
	retained := readSessionHeaderStrings(t, &message.Header.FieldMap)
	owned := materializeInboundMessage(message)
	copied := NewMessage()
	message.CopyInto(copied)
	var copiedHeader FieldMap
	copiedHeader.init()
	message.Header.CopyInto(&copiedHeader)
	message.Header.detachScopedFields()
	detached := readSessionHeaderStrings(t, &message.Header.FieldMap)
	if err := copied.attachMessageArena(DefaultParseLimits().MaxFrameBytes, DefaultParseLimits().MaxFields); err != nil {
		t.Fatal(err)
	}
	copied.Header.detachArena()
	for index := range wire {
		wire[index] = 'X'
	}
	slot.Reset()
	other := sessionHeaderStringID("next-frame")
	if err := slot.Parse(sessionHeaderStringWire(t, other, true)); err != nil {
		t.Fatal(err)
	}
	if retained != want || detached != want {
		t.Fatalf("retained strings changed after frame reuse: %q / %q", retained, detached)
	}
	for _, fields := range []*FieldMap{&owned.Header.FieldMap, &copied.Header.FieldMap, &copiedHeader} {
		if got := readSessionHeaderStrings(t, fields); got != want {
			t.Fatalf("owned copy changed after source reuse: %q, want %q", got, want)
		}
	}
	if got := readSessionHeaderStrings(t, &slot.Message().Header.FieldMap); got != sessionHeaderStringValues(other, true) {
		t.Fatalf("reused slot returned stale configured strings: %q", got)
	}
	// Restore this slot after a foreign owned copy replaced its sections.
	owned.CopyInto(slot.Message())
	slot.Reset()
	if err := slot.Parse(sessionHeaderStringWire(t, id, true)); err != nil {
		t.Fatal(err)
	}
	if got := readSessionHeaderStrings(t, &slot.Message().Header.FieldMap); got != want {
		t.Fatalf("slot reset lost configured header values: %q", got)
	}
}

func TestSessionHeaderStringsRebindCallerMessageAcrossSessions(t *testing.T) {
	firstID, secondID := sessionHeaderStringID("first"), sessionHeaderStringID("second")
	first := newSessionHeaderStringSession(t, firstID)
	second := newSessionHeaderStringSession(t, secondID)
	message := NewMessage()
	message.Header.SetString(tagMsgType, "0")
	send := func(current *session) {
		t.Helper()
		slot, err := current.prepareOutbound(message, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		slot.release()
	}
	send(first)
	retained := readSessionHeaderStrings(t, &message.Header.FieldMap)
	copy := NewMessage()
	message.CopyInto(copy)
	send(second)
	if got := readSessionHeaderStrings(t, &message.Header.FieldMap); got != sessionHeaderStringValues(secondID, false) {
		t.Fatalf("second session returned stale header strings: %q", got)
	}
	if retained != sessionHeaderStringValues(firstID, false) || readSessionHeaderStrings(t, &copy.Header.FieldMap) != retained {
		t.Fatal("cross-session rebind changed independently retained strings")
	}
	send(first)
	if got := readSessionHeaderStrings(t, &message.Header.FieldMap); got != retained {
		t.Fatalf("return to first session kept second session values: %q", got)
	}
}

func TestSessionHeaderStringsStandaloneAndNonHeaderValuesRemainOwned(t *testing.T) {
	message := NewMessage()
	value := []byte("RAILBIRD-standalone")
	message.Header.SetBytes(tagSenderCompID, value)
	retained, reject := message.Header.GetString(tagSenderCompID)
	if reject != nil {
		t.Fatal(reject)
	}
	value[0] = 'X'
	if retained != "RAILBIRD-standalone" {
		t.Fatal("standalone string retained mutable bytes")
	}
	current := newSessionHeaderStringSession(t, sessionHeaderStringID("body"))
	message = current.controls.message
	bodyValue := []byte("per-message-body-value")
	message.Body.SetBytes(tagSenderCompID, bodyValue)
	bodyString, reject := message.Body.GetString(tagSenderCompID)
	if reject != nil || bodyString != string(bodyValue) {
		t.Fatalf("body field changed: %q, %v", bodyString, reject)
	}
	bodyValue[0] = 'X'
	if bodyString != "per-message-body-value" {
		t.Fatal("non-header string retained mutable bytes")
	}
}
