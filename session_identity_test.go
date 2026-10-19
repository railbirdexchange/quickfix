package quickfix

import (
	"strconv"
	"strings"
	"sync"
	"testing"
)

func registerIdentityFixture(t *testing.T, id SessionID) {
	t.Helper()
	if err := registerSession(&session{sessionID: id}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = UnregisterSession(id) })
}

func TestSessionIdentityFormatting(t *testing.T) {
	for mask := 0; mask < 32; mask++ {
		id := SessionID{BeginString: "FIXT.1.1", SenderCompID: "S", TargetCompID: "T"}
		optional := []*string{&id.SenderSubID, &id.SenderLocationID, &id.TargetSubID, &id.TargetLocationID, &id.Qualifier}
		for bit, field := range optional {
			if mask&(1<<bit) != 0 {
				*field = strconv.Itoa(bit)
			}
		}
		expected := "FIXT.1.1:S"
		if mask&1 != 0 {
			expected += "/0"
		}
		if mask&2 != 0 {
			expected += "/1"
		}
		expected += "->T"
		if mask&4 != 0 {
			expected += "/2"
		}
		if mask&8 != 0 {
			expected += "/3"
		}
		if mask&16 != 0 {
			expected += ":4"
		}
		t.Run(strconv.Itoa(mask), func(t *testing.T) {
			if got := id.String(); got != expected {
				t.Fatalf("unregistered: %q != %q", got, expected)
			}
			registerIdentityFixture(t, id)
			if got := id.String(); got != expected {
				t.Fatalf("registered: %q != %q", got, expected)
			}
		})
	}
	for _, fixture := range []struct {
		id   SessionID
		want string
	}{
		{SessionID{}, ":->"},
		{SessionID{SenderCompID: "会員", TargetCompID: "β", Qualifier: "取引"}, ":会員->β:取引"},
		{SessionID{SenderCompID: strings.Repeat("S", 1024)}, ":" + strings.Repeat("S", 1024) + "->"},
	} {
		if got := fixture.id.String(); got != fixture.want {
			t.Fatalf("%q != %q", got, fixture.want)
		}
		registerIdentityFixture(t, fixture.id)
		if got := fixture.id.String(); got != fixture.want {
			t.Fatalf("registered: %q != %q", got, fixture.want)
		}
	}
}

func TestSessionIdentityLifecycle(t *testing.T) {
	id := SessionID{BeginString: "FIXT.1.1", SenderCompID: "identity-lifecycle", TargetCompID: "peer", Qualifier: "one"}
	registerIdentityFixture(t, id)
	retained := id.String()
	snapshot := registeredSessionNames.Load()
	if err := registerSession(&session{sessionID: id}); err != errDuplicateSessionID {
		t.Fatalf("duplicate: %v", err)
	}
	if registeredSessionNames.Load() != snapshot {
		t.Fatal("duplicate changed snapshot")
	}
	other := id
	other.Qualifier = "two"
	if err := UnregisterSession(other); err != errUnknownSession {
		t.Fatalf("unknown: %v", err)
	}
	if registeredSessionNames.Load() != snapshot {
		t.Fatal("unknown removal changed snapshot")
	}
	registerIdentityFixture(t, other)
	if err := UnregisterSession(id); err != nil {
		t.Fatal(err)
	}
	names := registeredSessionNames.Load()
	if _, present := (*names)[id]; present {
		t.Fatal("removed ID retained in current snapshot")
	}
	if (*names)[other] != "FIXT.1.1:identity-lifecycle->peer:two" {
		t.Fatal("other ID removed")
	}
	if (*snapshot)[id] != retained {
		t.Fatal("published snapshot mutated")
	}
	if err := registerSession(&session{sessionID: id}); err != nil {
		t.Fatal(err)
	}
	if got := id.String(); got != retained {
		t.Fatalf("reregister: %q != %q", got, retained)
	}
	if retained != "FIXT.1.1:identity-lifecycle->peer:one" {
		t.Fatal("retained string changed")
	}
}

func TestSessionIdentityUsesFullValue(t *testing.T) {
	id := SessionID{"FIXT.1.1", "T", "TS", "TL", "S", "SS", "SL", "Q"}
	registerIdentityFixture(t, id)
	copyID := id
	if copyID != id || map[SessionID]bool{id: true}[copyID] != true {
		t.Fatal("copied ID identity changed")
	}
	if allocations := testing.AllocsPerRun(1000, func() { sessionIdentityText = copyID.String() }); allocations != 0 {
		t.Fatalf("registered copied ID: %v allocations", allocations)
	}
	for field := 0; field < 8; field++ {
		changed := id
		fields := []*string{&changed.BeginString, &changed.TargetCompID, &changed.TargetSubID, &changed.TargetLocationID, &changed.SenderCompID, &changed.SenderSubID, &changed.SenderLocationID, &changed.Qualifier}
		*fields[field] += "changed"
		if changed.String() == id.String() {
			t.Fatalf("field %d reused wrong ID text", field)
		}
		if changed.String() != changed.format() {
			t.Fatalf("field %d fallback differs", field)
		}
	}
}

func TestSessionIdentityConcurrentLifecycle(t *testing.T) {
	id := SessionID{BeginString: "FIX.4.4", SenderCompID: "identity-concurrent", TargetCompID: "peer"}
	registerIdentityFixture(t, id)
	want := id.String()
	var readers sync.WaitGroup
	start := make(chan struct{})
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			for range 1000 {
				if got := id.String(); got != want {
					t.Errorf("concurrent string: %q != %q", got, want)
					return
				}
			}
		}()
	}
	close(start)
	for range 100 {
		if err := UnregisterSession(id); err != nil {
			t.Error(err)
			break
		}
		if err := registerSession(&session{sessionID: id}); err != nil {
			t.Error(err)
			break
		}
	}
	readers.Wait()
}
