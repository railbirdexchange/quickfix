package quickfix

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type retirementStore struct {
	MessageStore
	entered chan struct{}
	release chan struct{}
	closed  atomic.Int32
	err     error
}

func (s *retirementStore) Close() error {
	s.closed.Add(1)
	close(s.entered)
	<-s.release
	return s.err
}

type countingStoreFactory struct{ calls int }

func (f *countingStoreFactory) Create(SessionID) (MessageStore, error) {
	f.calls++
	return newRetirementMemoryStore(), nil
}

func TestDynamicSessionRetiresStoreBeforeReleasingIdentity(t *testing.T) {
	for _, closeErr := range []error{nil, errors.New("journal unavailable")} {
		name := "success"
		if closeErr != nil {
			name = "close failure"
		}
		t.Run(name, func(t *testing.T) {
			store := &retirementStore{MessageStore: newRetirementMemoryStore(), entered: make(chan struct{}), release: make(chan struct{}), err: closeErr}
			id := SessionID{BeginString: "FIX.4.4", SenderCompID: "VENUE", TargetCompID: t.Name()}
			s, err := (sessionFactory{}).createSession(id, replayStoreFactory{store}, NewSessionSettings(), NewNullLogFactory(), &starvationApp{})
			require.NoError(t, err)
			t.Cleanup(func() { _ = UnregisterSession(id) })
			a := &Acceptor{dynamicSessionChan: make(chan *session), globalLog: s.log}
			done := make(chan struct{})
			go func() { a.dynamicSessionsLoop(); close(done) }()
			a.dynamicSessionChan <- s
			s.stop()
			select {
			case <-store.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("retired dynamic session did not close its store")
			}
			current, exists := lookupSession(id)
			require.True(t, exists, "identity must remain reserved during final checkpoint flush")
			require.Same(t, s, current)
			factory := &countingStoreFactory{}
			_, err = (sessionFactory{}).createSession(id, factory, NewSessionSettings(), NewNullLogFactory(), &starvationApp{})
			require.ErrorIs(t, err, errDuplicateSessionID)
			require.Zero(t, factory.calls, "duplicate Logon must not construct a second checkpoint writer")
			close(store.release)
			close(a.dynamicSessionChan)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("dynamic cleanup did not complete")
			}
			_, exists = lookupSession(id)
			require.False(t, exists)
			require.EqualValues(t, 1, store.closed.Load())
		})
	}
}

func TestRetiredDynamicSessionCannotUnregisterReplacement(t *testing.T) {
	store := &retirementStore{MessageStore: newRetirementMemoryStore(), entered: make(chan struct{}), release: make(chan struct{})}
	close(store.release)
	id := SessionID{BeginString: "FIX.4.4", SenderCompID: "VENUE", TargetCompID: t.Name()}
	s, err := (sessionFactory{}).createSession(id, replayStoreFactory{store}, NewSessionSettings(), NewNullLogFactory(), &starvationApp{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = UnregisterSession(id) })
	a := &Acceptor{dynamicSessionChan: make(chan *session), globalLog: s.log}
	done := make(chan struct{})
	go func() { a.dynamicSessionsLoop(); close(done) }()
	a.dynamicSessionChan <- s
	// Applications can unregister explicitly; late engine cleanup must not
	// remove the replacement's registry or address entries.
	require.NoError(t, UnregisterSession(id))
	replacement := &session{sessionID: id}
	require.NoError(t, registerSession(replacement))
	a.sessionAddr.Store(id, "replacement-address")
	s.stop()
	close(a.dynamicSessionChan)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("already-unregistered session prevented acceptor shutdown")
	}
	current, exists := lookupSession(id)
	require.True(t, exists)
	require.Same(t, replacement, current)
	address, exists := a.sessionAddr.Load(id)
	require.True(t, exists)
	require.Equal(t, "replacement-address", address)
	require.EqualValues(t, 1, store.closed.Load())
}

func newRetirementMemoryStore() MessageStore {
	store, _ := NewMemoryStoreFactory().Create(SessionID{})
	return store
}
