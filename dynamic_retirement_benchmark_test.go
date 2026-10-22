package quickfix

import (
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
)

type retirementBenchmarkApplication struct{ Application }

func (retirementBenchmarkApplication) OnCreate(SessionID) {}

// Track store ownership independently of the registry, as resource-owning
// factories do. No artificial payload is added to the measured store.
type retirementBenchmarkFactory struct {
	mu     sync.Mutex
	stores map[*retirementBenchmarkStore]struct{}
	ended  chan struct{}
}

type retirementBenchmarkStore struct {
	MessageStore
	factory *retirementBenchmarkFactory
}

func (f *retirementBenchmarkFactory) Create(id SessionID) (MessageStore, error) {
	inner, err := NewMemoryStoreFactory().Create(id)
	if err != nil {
		return nil, err
	}
	s := &retirementBenchmarkStore{MessageStore: inner, factory: f}
	f.mu.Lock()
	f.stores[s] = struct{}{}
	f.mu.Unlock()
	return s, nil
}

func (s *retirementBenchmarkStore) Close() error {
	s.factory.mu.Lock()
	delete(s.factory.stores, s)
	s.factory.mu.Unlock()
	if s.factory.ended != nil {
		s.factory.ended <- struct{}{}
	}
	return s.MessageStore.Close()
}

type retirementBenchmarkLog struct {
	Log
	ended chan struct{}
}

func (l retirementBenchmarkLog) OnEventf(format string, _ ...interface{}) {
	// The unchanged baseline exits the dynamic wrapper here instead of
	// completing it. The candidate signals the same observation from Close.
	if format == "Unregister dynamic session %v failed: %v" {
		l.ended <- struct{}{}
	}
}

func BenchmarkDynamicSessionDuplicateCreation(b *testing.B) {
	id := SessionID{BeginString: BeginStringFIX44, SenderCompID: "DUPLICATE", TargetCompID: "PEER"}
	factory := &retirementBenchmarkFactory{stores: make(map[*retirementBenchmarkStore]struct{})}
	settings := NewSessionSettings()
	s, err := (sessionFactory{}).createSession(id, factory, settings, NewNullLogFactory(), retirementBenchmarkApplication{})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = UnregisterSession(id); _ = s.store.Close() }()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := (sessionFactory{}).createSession(id, factory, settings, NewNullLogFactory(), retirementBenchmarkApplication{}); err != errDuplicateSessionID {
			b.Fatalf("duplicate: %v", err)
		}
	}
}

// Run with -benchtime=1x. Each sample constructs eight real sessions, runs their
// actual dynamic owner and session loop, and unregisters before stopping. This
// measures retained heap after GC, not wire latency or allocations per message.
// The buggy baseline cannot join its dynamic owner; that is an explicit result,
// bounded to eight sessions per sample and released when the test process exits.
func BenchmarkDynamicSessionRetainedHeap(b *testing.B) {
	if b.N != 1 {
		b.Fatal("retention fixture requires -benchtime=1x")
	}
	const count = 8
	ended := make(chan struct{}, count)
	factory := &retirementBenchmarkFactory{stores: make(map[*retirementBenchmarkStore]struct{}), ended: ended}
	global, err := NewNullLogFactory().Create()
	if err != nil {
		b.Fatal(err)
	}
	a := &Acceptor{dynamicSessionChan: make(chan *session), globalLog: retirementBenchmarkLog{Log: global, ended: ended}}
	done := make(chan struct{})
	go func() { a.dynamicSessionsLoop(); close(done) }()
	settings := NewSessionSettings()
	// Warm construction before measuring retained memory.
	warm, err := (sessionFactory{}).newSession(SessionID{BeginString: BeginStringFIX44}, NewMemoryStoreFactory(), settings, NewNullLogFactory(), retirementBenchmarkApplication{})
	if err != nil {
		b.Fatal(err)
	}
	_ = warm.store.Close()
	warm = nil
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	b.ResetTimer()
	for i := 0; i < count; i++ {
		id := SessionID{BeginString: BeginStringFIX44, SenderCompID: "RETIRE", TargetCompID: strconv.Itoa(i)}
		s, err := (sessionFactory{}).createSession(id, factory, settings, NewNullLogFactory(), retirementBenchmarkApplication{})
		if err != nil {
			b.Fatal(err)
		}
		a.dynamicSessionChan <- s
		if err := UnregisterSession(id); err != nil {
			b.Fatal(err)
		}
		go s.stop()
	}
	for i := 0; i < count; i++ {
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			b.Fatal("session did not finish")
		}
	}
	close(a.dynamicSessionChan)
	joined := 0
	select {
	case <-done:
		joined = 1
	case <-time.After(50 * time.Millisecond):
	}
	b.StopTimer()
	runtime.GC()
	runtime.ReadMemStats(&after)
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	factory.mu.Lock()
	remaining := len(factory.stores)
	factory.mu.Unlock()
	b.ReportMetric(float64(retained)/count, "retained-B/session")
	b.ReportMetric(float64(remaining), "open-stores")
	b.ReportMetric(float64(joined), "owner-joined")
	runtime.KeepAlive(factory)
	runtime.KeepAlive(a)
}
