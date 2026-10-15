package quickfix

import (
	"testing"

	"github.com/quickfixgo/quickfix/config"
)

type settingsBorrowedApplication struct{ Application }

func (settingsBorrowedApplication) FromAdminBorrowed(*MessageSlot, SessionID) MessageRejectError {
	return nil
}
func (settingsBorrowedApplication) FromAppBorrowed(*MessageSlot, SessionID) MessageRejectError {
	return nil
}

type settingsLegacyLog struct{ Log }
type settingsLegacyLogFactory struct{ LogFactory }

func (settingsLegacyLogFactory) CreateSessionLog(SessionID) (Log, error) {
	return settingsLegacyLog{Log: nullLog{}}, nil
}

func TestBoundedInboundSettings(t *testing.T) {
	id := SessionID{BeginString: "FIX.4.4", SenderCompID: "S", TargetCompID: "T"}
	application := settingsBorrowedApplication{Application: &MockApp{}}
	for _, test := range []struct {
		name         string
		settings     map[string]string
		queue, stash int
		limits       ParseLimits
	}{
		{"defaults", nil, 32, 32, DefaultParseLimits()},
		{"explicit", map[string]string{
			config.InChanCapacity: "0", config.BoundedInboundStashCapacity: "2",
			config.BoundedMaxFrameBytes: "1024", config.BoundedMaxFields: "64",
			config.BoundedMaxGroupEntries: "16", config.BoundedMaxGroupDepth: "3",
		}, 0, 2, ParseLimits{1024, 64, 16, 3}},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := NewSessionSettings()
			settings.Set(config.BoundedInbound, "Y")
			for key, value := range test.settings {
				settings.Set(key, value)
			}
			s, err := (sessionFactory{}).newSession(id, NewMemoryStoreFactory(), settings, NewNullLogFactory(), application)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.store.Close() })
			if s.boundedIn == nil || s.boundedIn.limits != test.limits || s.InChanCapacity != test.queue ||
				len(s.boundedIn.ring.stash) != test.stash || len(s.boundedIn.ring.slots) != test.queue+test.stash+2 {
				t.Fatalf("bounded settings not applied: %#v", s.boundedIn)
			}
			if s.boundedIn.validator.maxFields != test.limits.MaxFields {
				t.Fatal("validator capacity differs from parse slots")
			}
		})
	}
	for _, enabled := range []string{"", "N"} {
		settings := NewSessionSettings()
		if enabled != "" {
			settings.Set(config.BoundedInbound, enabled)
		}
		s, err := (sessionFactory{}).newSession(id, NewMemoryStoreFactory(), settings, NewNullLogFactory(), &MockApp{})
		if err != nil {
			t.Fatal(err)
		}
		_ = s.store.Close()
		if s.boundedIn != nil || s.InChanCapacity != 1 {
			t.Fatal("legacy defaults changed")
		}
	}
}

func TestBoundedInboundRejectsInvalidSetupBeforeStoreCreation(t *testing.T) {
	id := SessionID{BeginString: "FIX.4.4", SenderCompID: "S", TargetCompID: "T"}
	borrowed := settingsBorrowedApplication{Application: &MockApp{}}
	for _, test := range []struct {
		name, key, value string
		app              Application
		log              LogFactory
		want             error
	}{
		{"handler lifetime", "", "", &MockApp{}, NewNullLogFactory(), ErrBorrowedApplicationRequired},
		{"logger lifetime", "", "", borrowed, settingsLegacyLogFactory{}, ErrBorrowedLogRequired},
		{"negative stash", config.BoundedInboundStashCapacity, "-1", borrowed, NewNullLogFactory(), ErrInvalidInboundLimits},
		{"short frame", config.BoundedMaxFrameBytes, "11", borrowed, NewNullLogFactory(), ErrInvalidParseLimits},
		{"too few fields", config.BoundedMaxFields, "3", borrowed, NewNullLogFactory(), ErrInvalidParseLimits},
		{"negative group depth", config.BoundedMaxGroupDepth, "-1", borrowed, NewNullLogFactory(), ErrInvalidParseLimits},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := NewSessionSettings()
			settings.Set(config.BoundedInbound, "Y")
			if test.key != "" {
				settings.Set(test.key, test.value)
			}
			// A nil factory proves no store is opened before setup is accepted.
			_, err := (sessionFactory{}).newSession(id, nil, settings, test.log, test.app)
			if err != test.want {
				t.Fatalf("got %v, want %v", err, test.want)
			}
		})
	}
}
