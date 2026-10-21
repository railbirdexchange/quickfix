package quickfix

import (
	"strconv"
	"testing"
)

// The same fixtures run against the parent and candidate implementations.
// Registration, messages, routes and warm-up are outside the measured interval.
func registryBenchmarkSessions(b *testing.B) ([]SessionID, []*session) {
	b.Helper()
	ids := make([]SessionID, 100)
	registered := make([]*session, len(ids))
	for i := range ids {
		ids[i] = SessionID{BeginString: BeginStringFIXT11, SenderCompID: b.Name(), TargetCompID: strconv.Itoa(i)}
		registered[i] = &session{sessionID: ids[i], targetDefaultApplVerID: ApplVerIDFIX50SP2}
		if err := registerSession(registered[i]); err != nil {
			b.Fatal(err)
		}
	}
	b.Cleanup(func() {
		for _, id := range ids {
			if err := UnregisterSession(id); err != nil {
				b.Error(err)
			}
		}
	})
	return ids, registered
}

func BenchmarkSessionRegistry(b *testing.B) {
	b.Run("lookup_100_sessions", func(b *testing.B) {
		ids, registered := registryBenchmarkSessions(b)
		for i, id := range ids {
			if got, ok := lookupSession(id); !ok || got != registered[i] {
				b.Fatal("warm-up lookup failed")
			}
		}
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				if got, ok := lookupSession(ids[i]); !ok || got != registered[i] {
					b.Fatal("lookup returned the wrong session")
				}
				i++
				if i == len(ids) {
					i = 0
				}
			}
		})
		b.StopTimer()
	})
	b.Run("route_fixt_default_100_sessions", func(b *testing.B) {
		ids, _ := registryBenchmarkSessions(b)
		messages := make([]*Message, len(ids))
		router := NewMessageRouter()
		router.AddRoute(ApplVerIDFIX50SP2, "D", func(*Message, SessionID) MessageRejectError { return nil })
		for i := range messages {
			message := NewMessage()
			message.Header.SetString(tagBeginString, BeginStringFIXT11)
			message.Header.SetString(tagMsgType, "D")
			messages[i] = message
			if err := router.Route(message, ids[i]); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				if err := router.Route(messages[i], ids[i]); err != nil {
					b.Fatal(err)
				}
				i++
				if i == len(ids) {
					i = 0
				}
			}
		})
		b.StopTimer()
	})
	b.Run("name_100_sessions", func(b *testing.B) {
		ids, _ := registryBenchmarkSessions(b)
		names := make([]string, len(ids))
		for i, id := range ids {
			names[i] = id.String()
		}
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				if got := ids[i].String(); got != names[i] {
					b.Fatal("session name changed")
				}
				i++
				if i == len(ids) {
					i = 0
				}
			}
		})
		b.StopTimer()
	})
	b.Run("register_unregister_with_100_sessions", func(b *testing.B) {
		registryBenchmarkSessions(b)
		id := SessionID{BeginString: BeginStringFIXT11, SenderCompID: b.Name(), TargetCompID: "churn"}
		s := &session{sessionID: id}
		if err := registerSession(s); err != nil {
			b.Fatal(err)
		}
		if err := UnregisterSession(id); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := registerSession(s); err != nil {
				b.Fatal(err)
			}
			if err := UnregisterSession(id); err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
	})
}
