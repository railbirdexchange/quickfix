package quickfix

import (
	"sync"
	"testing"
)

func TestSessionRegistryConcurrentLookupAndNameDuringChurn(t *testing.T) {
	id := SessionID{BeginString: BeginStringFIXT11, SenderCompID: t.Name(), TargetCompID: "PEER"}
	stableID := id
	stableID.Qualifier = "stable"
	stable := &session{sessionID: stableID}
	if err := registerSession(stable); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unregisterSessionInstance(stable) })
	t.Cleanup(func() { _ = UnregisterSession(id) })

	// Published session fields remain immutable even after an instance is removed.
	instances := [2]*session{{sessionID: id}, {sessionID: id}}
	wantName, wantStableName := id.format(), stableID.format()
	start := make(chan struct{})
	var ready, readers sync.WaitGroup
	for range 4 {
		ready.Add(1)
		readers.Add(1)
		go func() {
			defer readers.Done()
			ready.Done()
			<-start
			for range 4096 {
				if got, ok := lookupSession(id); ok && got != instances[0] && got != instances[1] {
					t.Errorf("lookup returned an unpublished instance: %p", got)
					return
				}
				if got := id.String(); got != wantName {
					t.Errorf("churning session name = %q, want %q", got, wantName)
					return
				}
				if got, ok := lookupSession(stableID); !ok || got != stable {
					t.Errorf("unrelated stable session disappeared: instance %p, present %t", got, ok)
					return
				}
				if got := stableID.String(); got != wantStableName {
					t.Errorf("stable session name = %q, want %q", got, wantStableName)
					return
				}
			}
		}()
	}
	ready.Wait()
	close(start)
	for i := range 256 {
		instance := instances[i%len(instances)]
		if err := registerSession(instance); err != nil {
			t.Error(err)
			break
		}
		if i%2 == 0 {
			if err := UnregisterSession(id); err != nil {
				t.Error(err)
				break
			}
		} else {
			unregisterSessionInstance(instance)
		}
	}
	readers.Wait()
	if _, ok := lookupSession(id); ok {
		t.Error("churning session remains registered after final removal")
	}
}

func TestSessionRegistryStaleInstanceCannotRemoveReplacement(t *testing.T) {
	id := SessionID{BeginString: BeginStringFIXT11, SenderCompID: t.Name(), TargetCompID: "PEER"}
	old := &session{sessionID: id}
	replacement := &session{sessionID: id}
	t.Cleanup(func() { _ = UnregisterSession(id) })
	if err := registerSession(old); err != nil {
		t.Fatal(err)
	}
	retainedName := id.String()
	if err := UnregisterSession(id); err != nil {
		t.Fatal(err)
	}
	if err := registerSession(replacement); err != nil {
		t.Fatal(err)
	}

	unregisterSessionInstance(old)
	if got, ok := lookupSession(id); !ok || got != replacement {
		t.Fatalf("stale cleanup removed replacement: instance %p, present %t", got, ok)
	}
	if got := id.String(); got != retainedName {
		t.Fatalf("replacement name = %q, retained name = %q", got, retainedName)
	}
	unregisterSessionInstance(replacement)
	if _, ok := lookupSession(id); ok {
		t.Fatal("current instance cleanup did not remove replacement")
	}
}

func TestSessionRegistryConcurrentRegistration(t *testing.T) {
	for _, reserved := range []bool{false, true} {
		name := "unreserved"
		if reserved {
			name = "reserved"
		}
		t.Run(name, func(t *testing.T) {
			id := SessionID{BeginString: BeginStringFIXT11, SenderCompID: t.Name(), TargetCompID: "PEER"}
			t.Cleanup(func() { _ = UnregisterSession(id) })
			if reserved {
				reservation, err := reserveSessionID(id, true)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { releaseSessionReservation(id, reservation) })
			}

			type result struct {
				instance *session
				err      error
			}
			const contenders = 32
			results := make(chan result, contenders)
			start := make(chan struct{})
			var workers sync.WaitGroup
			for range contenders {
				workers.Add(1)
				go func() {
					defer workers.Done()
					instance := &session{sessionID: id}
					<-start
					results <- result{instance, registerSession(instance)}
				}()
			}
			close(start)
			workers.Wait()
			close(results)
			var winner *session
			var successes int
			for result := range results {
				if result.err == nil {
					successes++
					winner = result.instance
				} else if result.err != errDuplicateSessionID {
					t.Errorf("registration error = %v, want duplicate session", result.err)
				}
			}
			wantSuccesses := 1
			if reserved {
				wantSuccesses = 0
			}
			if successes != wantSuccesses {
				t.Fatalf("successful registrations = %d, want %d", successes, wantSuccesses)
			}
			got, ok := lookupSession(id)
			if reserved {
				if ok {
					t.Fatal("registration bypassed an existing identity reservation")
				}
			} else if !ok || got != winner {
				t.Fatalf("registered instance %p, present %t; want winner %p", got, ok, winner)
			}
		})
	}
}
