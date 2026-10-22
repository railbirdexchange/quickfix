package quickfix

import (
	"testing"

	"github.com/quickfixgo/quickfix/datadictionary"
)

func TestMessageBorrowGroupCallbackScope(t *testing.T) {
	dictionary, err := datadictionary.Parse("spec/FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	slot, err := NewMessageSlot(dictionary, dictionary, DefaultParseLimits())
	if err != nil {
		t.Fatal(err)
	}
	slot.enableScopedCallbacks()
	wire := []byte(allocationBaselineRepeatingGroupMessage)
	if err := slot.Parse(wire); err != nil {
		t.Fatal(err)
	}
	message := slot.Message()
	group, err := message.BorrowGroup(453)
	if err != nil || group.Len() != 1 {
		t.Fatalf("group = %v, error = %v", group, err)
	}
	iterator := group.Iter()
	entry, found := iterator.Next()
	if value, present := entry.GetBytes(448); !found || !present || string(value) != "4501" {
		t.Fatalf("unexpected party: %q", value)
	}
	// A shallow Message copy must read its current sections, not reach back to
	// the originating slot's message after a caller replaces a section.
	copyOfMessage := *message
	copyOfMessage.Body = NewMessage().Body
	if _, err := copyOfMessage.BorrowGroup(453); err != ErrBorrowedGroupNotFound {
		t.Fatalf("replacement body returned the original group: %v", err)
	}
	slot.Reset()
	if _, err := message.BorrowGroup(453); err != ErrBorrowedGroupNotFound {
		t.Fatalf("reset left an active group: %v", err)
	}
	if _, err := NewMessage().BorrowGroup(453); err != ErrBorrowedGroupNotFound {
		t.Fatalf("unparsed message returned a group: %v", err)
	}
}

func TestMessageBorrowGroupNoAllocations(t *testing.T) {
	dictionary, err := datadictionary.Parse("spec/FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	slot, err := NewMessageSlot(dictionary, dictionary, DefaultParseLimits())
	if err != nil {
		t.Fatal(err)
	}
	slot.enableScopedCallbacks()
	wire := []byte(allocationBaselineRepeatingGroupMessage)
	if allocations := testing.AllocsPerRun(1000, func() {
		if err := slot.Parse(wire); err != nil {
			panic(err)
		}
		group, err := slot.Message().BorrowGroup(453)
		if err != nil {
			panic(err)
		}
		iterator := group.Iter()
		entry, found := iterator.Next()
		value, present := entry.GetBytes(448)
		if !found || !present || string(value) != "4501" {
			panic("incorrect borrowed party")
		}
		slot.Reset()
	}); allocations != 0 {
		t.Fatalf("parse and callback group access allocated %v objects", allocations)
	}
}

// Both cases consume the same callback-scoped message and read the same party
// bytes. Only the owned compatibility group versus borrowed view differs.
func BenchmarkCallbackGroupAccess(benchmark *testing.B) {
	dictionary, err := datadictionary.Parse("spec/FIX44.xml")
	if err != nil {
		benchmark.Fatal(err)
	}
	for _, borrowed := range []bool{false, true} {
		name := "owned_group"
		if borrowed {
			name = "borrowed_group"
		}
		benchmark.Run(name, func(benchmark *testing.B) {
			slot, err := NewMessageSlot(dictionary, dictionary, DefaultParseLimits())
			if err != nil {
				benchmark.Fatal(err)
			}
			slot.enableScopedCallbacks()
			if err := slot.Parse([]byte(allocationBaselineRepeatingGroupMessage)); err != nil {
				benchmark.Fatal(err)
			}
			read := func() {
				if borrowed {
					group, err := slot.Message().BorrowGroup(453)
					if err != nil {
						benchmark.Fatal(err)
					}
					iterator := group.Iter()
					entry, found := iterator.Next()
					value, present := entry.GetBytes(448)
					if !found || !present || string(value) != "4501" {
						benchmark.Fatal("incorrect borrowed party")
					}
					return
				}
				group := NewRepeatingGroup(453, GroupTemplate{GroupElement(448), GroupElement(447), GroupElement(452)})
				if err := slot.Message().Body.GetGroup(group); err != nil {
					benchmark.Fatal(err)
				}
				value, err := group.Get(0).GetBytes(448)
				if err != nil || string(value) != "4501" {
					benchmark.Fatal("incorrect owned party")
				}
			}
			for index := 0; index < 100; index++ {
				read()
			}
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for index := 0; index < benchmark.N; index++ {
				read()
			}
		})
	}
}
