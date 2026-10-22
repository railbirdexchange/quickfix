//go:build !race

package quickfix

import "testing"

// Every successful configured getter still returns an independently retainable
// string. Its immutable value may be shared across owned copies and slots.
func TestSessionHeaderStringsExactAllocationsAcrossOwnedBoundaries(t *testing.T) {
	id := sessionHeaderStringID("allocation")
	current := newSessionHeaderStringSession(t, id)
	for _, inbound := range []bool{true, false} {
		slot := current.outbound.replay
		if inbound {
			slot = current.boundedIn.parsed
		}
		if err := slot.Parse(sessionHeaderStringWire(t, id, inbound)); err != nil {
			t.Fatal(err)
		}
		source := slot.Message()
		owned := materializeInboundMessage(source)
		copied := NewMessage()
		source.CopyInto(copied)
		if err := copied.attachMessageArena(DefaultParseLimits().MaxFrameBytes, DefaultParseLimits().MaxFields); err != nil {
			t.Fatal(err)
		}
		copied.Header.detachArena()
		source.Header.detachScopedFields()
		for _, message := range []*Message{source, owned, copied} {
			if got := readSessionHeaderStrings(t, &message.Header.FieldMap); got != sessionHeaderStringValues(id, inbound) {
				t.Fatalf("invalid owned boundary: %q", got)
			}
			allocations := testing.AllocsPerRun(100, func() {
				for _, tag := range sessionHeaderStringTags {
					sessionHeaderStringResult, _ = message.Header.GetString(tag)
				}
			})
			if allocations != 0 {
				t.Fatalf("inbound=%v: seven configured getters allocated %v objects", inbound, allocations)
			}
		}
	}
}
