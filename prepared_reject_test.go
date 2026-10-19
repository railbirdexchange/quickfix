package quickfix

import (
	"strconv"
	"sync"
	"testing"

	"github.com/quickfixgo/quickfix/datadictionary"
)

func TestPreparedMissingRejectGetterAndLifetime(t *testing.T) {
	wire, dictionary := boundedOwnershipFixture(t, true)
	slot, err := NewMessageSlot(dictionary, dictionary, DefaultParseLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := slot.Parse(wire); err != nil {
		t.Fatal(err)
	}
	const tag Tag = 526
	message := slot.Message()
	fields := &message.Body.FieldMap
	want := fields.missingRejects[tag]
	if want == nil {
		t.Fatal("configured tag has no prepared reject")
	}
	var scalar FIXString
	group := NewRepeatingGroup(tag, GroupTemplate{GroupElement(448)})
	checks := []struct {
		name string
		read func() MessageRejectError
	}{
		{"string", func() MessageRejectError { _, reject := fields.GetString(tag); return reject }},
		{"bytes", func() MessageRejectError { _, reject := fields.GetBytes(tag); return reject }},
		{"field", func() MessageRejectError { return fields.GetField(tag, &scalar) }},
		{"group", func() MessageRejectError { return fields.GetGroup(group) }},
		{"unlocked_string", func() MessageRejectError { _, reject := fields.getStringNoLock(tag); return reject }},
		{"unlocked_bytes", func() MessageRejectError { _, reject := fields.getBytesNoLock(tag); return reject }},
		{"unlocked_field", func() MessageRejectError { return fields.getFieldNoLock(tag, &scalar) }},
	}
	for _, check := range checks {
		if got := check.read(); got != want {
			t.Fatalf("%s did not return the prepared reject: %v", check.name, got)
		}
	}
	owned := materializeInboundMessage(message)
	copied := NewMessage()
	message.CopyInto(copied)
	// CopyInto is an owned convenience boundary, including when its destination
	// is a public slot. The next parse must restore that slot's own dictionary.
	NewMessage().CopyInto(message)
	slot.Reset()
	if err := slot.Parse(wire); err != nil {
		t.Fatal(err)
	}
	if _, reject := message.Body.GetString(tag); reject != want {
		t.Fatal("CopyInto destination slot lost its configured error table")
	}
	slot.enableScopedCallbacks()
	// An allocating setter detaches this section from its fixed parse storage.
	fields.SetString(100001, "owned mutation")
	if _, reject := fields.GetString(tag); reject != want {
		t.Fatal("scoped detachment lost the immutable reject")
	}
	slot.Reset()
	if err := slot.Parse(wire); err != nil {
		t.Fatal(err)
	}
	for _, retained := range []*Message{slot.Message(), owned, copied} {
		if _, reject := retained.Body.GetString(tag); reject != want {
			t.Fatal("copy, materialization or slot reuse lost the immutable reject")
		}
	}
	if got, present := want.RefTagID(); !present || got != tag || want.Error() != "Conditionally Required Field Missing (526)" || want.RejectReason() != 8 || !want.IsBusinessReject() {
		t.Fatalf("retained reject changed: %v, %d, %t", want, got, present)
	}
	if err := copied.attachMessageArena(DefaultParseLimits().MaxFrameBytes, DefaultParseLimits().MaxFields); err != nil {
		t.Fatal(err)
	}
	copied.Body.detachArena()
	if _, reject := copied.Body.GetString(tag); reject != want {
		t.Fatal("arena attachment or detachment lost the immutable reject")
	}
}

func TestPreparedMissingRejectDictionaryUnionAndFallback(t *testing.T) {
	transport := &datadictionary.DataDictionary{FieldTypeByTag: map[int]*datadictionary.FieldType{8: nil, 35: nil}}
	application := &datadictionary.DataDictionary{FieldTypeByTag: map[int]*datadictionary.FieldType{35: nil, 526: nil}}
	rejects := prepareMissingRejects(transport, application)
	if len(rejects) != 3 || rejects[8] == nil || rejects[35] == nil || rejects[526] == nil {
		t.Fatal("prepared errors did not cover the dictionary union")
	}
	fields := FieldMap{missingRejects: rejects}
	fields.init()
	for _, tag := range []Tag{0, -1, 100001, Tag(int(^uint(0) >> 1)), Tag(-int(^uint(0)>>1) - 1)} {
		_, reject := fields.GetString(tag)
		if reject == nil {
			t.Fatalf("unknown tag %d did not return an error", tag)
		}
		got, present := reject.RefTagID()
		want := "Conditionally Required Field Missing (" + strconv.FormatInt(int64(tag), 10) + ")"
		if !present || got != tag || reject.Error() != want || reject.Error() != want {
			t.Fatalf("unknown tag %d returned %d, %t, %q", tag, got, present, reject.Error())
		}
	}
	if len(rejects) != 3 {
		t.Fatal("unknown-tag reads grew the prepared error table")
	}
}

func TestPreparedMissingRejectSessionStorageAndConcurrency(t *testing.T) {
	fixture := newPreparedRejectHeartbeatFixture(t)
	defer fixture.close()
	current := fixture.session
	want := current.boundedIn.missingRejects[526]
	if want == nil {
		t.Fatal("session did not prepare its dictionary errors")
	}
	for _, message := range []*Message{&current.boundedIn.parsed.message, &current.outbound.replay.message, current.controls.message} {
		for _, fields := range []*FieldMap{&message.Header.FieldMap, &message.Body.FieldMap, &message.Trailer.FieldMap} {
			if fields.missingRejects[526] != want {
				t.Fatal("session storage did not share the initialized error")
			}
		}
	}
	fixture.controlApplication.hook = func(message *Message) {
		if _, reject := message.Body.GetString(526); reject != want {
			t.Error("control callback did not use the session's immutable reject")
		}
	}
	for range 3 {
		if err := fixture.process("idle_heartbeat"); err != nil {
			t.Fatal(err)
		}
	}
	// Caller-owned output may have come from another dictionary/session.
	message := NewMessage()
	message.Header.SetString(tagMsgType, "D")
	message.useMissingRejects(prepareMissingRejects(&datadictionary.DataDictionary{FieldTypeByTag: map[int]*datadictionary.FieldType{1: nil}}, nil))
	fixture.controlApplication.hook = nil
	slot, err := current.prepareOutbound(message, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	slot.release()
	if message.Body.missingRejects[526] != want {
		t.Fatal("outgoing message kept another session's prepared errors")
	}
	// All goroutines share immutable errors, but own separate mutable messages.
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			fields := FieldMap{missingRejects: current.boundedIn.missingRejects}
			fields.init()
			for range 1000 {
				_, reject := fields.GetString(526)
				tag, present := reject.RefTagID()
				if reject != want || !present || tag != 526 || reject.Error() != "Conditionally Required Field Missing (526)" {
					t.Error("concurrent reader observed a changed reject")
					return
				}
			}
		}()
	}
	workers.Wait()
}
