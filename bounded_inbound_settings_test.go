package quickfix

import (
	"testing"

	"github.com/quickfixgo/quickfix/config"
)

type settingsLegacyLog struct{ Log }
type settingsLegacyLogFactory struct{ LogFactory }

func (settingsLegacyLogFactory) CreateSessionLog(SessionID) (Log, error) {
	return settingsLegacyLog{Log: nullLog{}}, nil
}

func TestScopedInboundSettings(t *testing.T) {
	id := SessionID{BeginString: "FIX.4.4", SenderCompID: "S", TargetCompID: "T"}
	for _, test := range []struct {
		name                  string
		settings              map[string]string
		channel, queue, stash int
		limits                ParseLimits
	}{
		{"defaults", nil, 1, 1, 32, DefaultParseLimits()},
		{"large_existing_channel", map[string]string{config.InChanCapacity: "30000"}, 30000, 32, 32, DefaultParseLimits()},
		{"explicit_frame_queue", map[string]string{config.InChanCapacity: "30000", config.InboundFrameQueueCapacity: "7"}, 30000, 7, 32, DefaultParseLimits()},
		{"frame_queue_respects_channel", map[string]string{config.InboundFrameQueueCapacity: "7"}, 1, 1, 32, DefaultParseLimits()},
		{"explicit_limits", map[string]string{
			config.InChanCapacity: "0", config.BoundedInboundStashCapacity: "2",
			config.BoundedMaxFrameBytes: "1024", config.BoundedMaxFields: "64",
			config.BoundedMaxGroupEntries: "16", config.BoundedMaxGroupDepth: "3",
		}, 0, 0, 2, ParseLimits{1024, 64, 16, 3}},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := NewSessionSettings()
			for key, value := range test.settings {
				settings.Set(key, value)
			}
			// The ordinary Application and logger require no lifetime opt-in.
			s, err := (sessionFactory{}).newSession(id, NewMemoryStoreFactory(), settings, settingsLegacyLogFactory{}, &MockApp{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.store.Close() })
			if s.boundedIn == nil || s.boundedIn.limits != test.limits || s.InChanCapacity != test.channel ||
				len(s.boundedIn.ring.stash) != test.stash || len(s.boundedIn.ring.slots) != test.queue+test.stash+2 {
				t.Fatalf("scoped settings not applied: %#v", s.boundedIn)
			}
			if s.boundedIn.parsed == nil || cap(s.boundedIn.parsed.message.fields) != test.limits.MaxFields {
				t.Fatal("session does not own its configured parse scratch")
			}
			if s.boundedIn.log != nil {
				t.Fatal("ordinary logger unexpectedly gained a borrowed capability")
			}
			if s.Validator == nil {
				t.Fatal("default integration removed normal validation")
			}
			for _, slot := range s.boundedIn.ring.slots {
				if cap(slot.frame) != test.limits.MaxFrameBytes {
					t.Fatal("raw frame slot capacity differs from configured limit")
				}
			}
			assertInboundRingVacant(t, s.boundedIn.ring)
		})
	}
}

func TestScopedInboundRejectsInvalidSetupBeforeStoreCreation(t *testing.T) {
	id := SessionID{BeginString: "FIX.4.4", SenderCompID: "S", TargetCompID: "T"}
	for _, test := range []struct {
		name, key, value string
		want             error
	}{
		{"negative frame queue", config.InboundFrameQueueCapacity, "-1", ErrInvalidInboundLimits},
		{"negative stash", config.BoundedInboundStashCapacity, "-1", ErrInvalidInboundLimits},
		{"short frame", config.BoundedMaxFrameBytes, "11", ErrInvalidParseLimits},
		{"too few fields", config.BoundedMaxFields, "3", ErrInvalidParseLimits},
		{"negative group depth", config.BoundedMaxGroupDepth, "-1", ErrInvalidParseLimits},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := NewSessionSettings()
			settings.Set(test.key, test.value)
			// A nil factory proves no store is opened before setup is accepted.
			_, err := (sessionFactory{}).newSession(id, nil, settings, NewNullLogFactory(), &MockApp{})
			if err != test.want {
				t.Fatalf("got %v, want %v", err, test.want)
			}
		})
	}
}
