package quickfix

import (
	"bytes"
	"errors"
	"testing"
)

type heartbeatStoreDecorator struct {
	*heartbeatBorrowedStore
	overrides int
}

func (store *heartbeatStoreDecorator) SaveMessageAndIncrNextSenderMsgSeqNum(sequence int, wire []byte) error {
	store.overrides++
	return store.heartbeatLegacyStore.SaveMessageAndIncrNextSenderMsgSeqNum(sequence, wire)
}

type heartbeatOptInDecorator struct {
	*heartbeatStoreDecorator
	borrowedOverrides int
}

func (store *heartbeatOptInDecorator) BorrowedStoreOwner() MessageStore { return store }

func (store *heartbeatOptInDecorator) SaveMessageAndIncrNextSenderMsgSeqNumBorrowed(sequence int, wire []byte) error {
	store.borrowedOverrides++
	return store.heartbeatBorrowedStore.SaveMessageAndIncrNextSenderMsgSeqNumBorrowed(sequence, wire)
}

// MessageStore permits value implementations. The slice makes this dynamic
// type noncomparable, so capability selection must not compare its identity.
type heartbeatNoncomparableStore struct {
	*heartbeatBorrowedStore
	marker []byte
}

func (store heartbeatNoncomparableStore) BorrowedStoreOwner() MessageStore {
	panic("noncomparable store must stay on the legacy path")
}

// The static type is comparable, but its interface payload can still make
// comparing two values panic. Setup must check the concrete value as well.
type heartbeatInterfaceValueStore struct {
	*heartbeatBorrowedStore
	marker interface{}
}

func (store heartbeatInterfaceValueStore) BorrowedStoreOwner() MessageStore {
	panic("noncomparable interface payload must stay on the legacy path")
}

type heartbeatReturnedOwnerStore struct {
	*heartbeatBorrowedStore
	marker interface{}
}

func (store heartbeatReturnedOwnerStore) BorrowedStoreOwner() MessageStore {
	return heartbeatReturnedOwnerStore{heartbeatBorrowedStore: store.heartbeatBorrowedStore, marker: []byte{1}}
}

func configureHeartbeatStore(test testing.TB, fixture *heartbeatStoreFixture, store MessageStore) {
	test.Helper()
	fixture.session.store = store
	if err := fixture.session.configureOutbound(NewSessionSettings()); err != nil {
		test.Fatal(err)
	}
}

func TestHeartbeatStoreControlDispatch(t *testing.T) {
	for _, kind := range []string{"idle_heartbeat", "timer_heartbeat", "test_request"} {
		for _, storeKind := range []string{"bounded", "legacy_external", "borrowed_external"} {
			t.Run(kind+"/"+storeKind, func(t *testing.T) {
				fixture := newHeartbeatStoreFixture(t, storeKind)
				defer fixture.close()
				if err := fixture.process(kind); err != nil {
					t.Fatal(err)
				}
				if fixture.borrowed != nil {
					if fixture.borrowed.borrowedCalls != 1 || fixture.external.legacyCalls != 0 {
						t.Fatalf("borrowed=%d legacy=%d", fixture.borrowed.borrowedCalls, fixture.external.legacyCalls)
					}
				} else if fixture.external != nil && fixture.external.legacyCalls != 1 {
					t.Fatalf("legacy=%d", fixture.external.legacyCalls)
				}
			})
		}
	}
}

func TestHeartbeatStoreCapabilityOwnership(t *testing.T) {
	for _, kind := range []string{"promoted_decorator", "explicit_decorator", "noncomparable_value", "noncomparable_interface_payload", "noncomparable_returned_owner"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newHeartbeatStoreFixture(t, "borrowed_external")
			defer fixture.close()
			decorator := &heartbeatStoreDecorator{heartbeatBorrowedStore: fixture.borrowed}
			explicit := &heartbeatOptInDecorator{heartbeatStoreDecorator: decorator}
			var store MessageStore
			switch kind {
			case "promoted_decorator":
				store = decorator
			case "explicit_decorator":
				store = explicit
			case "noncomparable_value":
				store = heartbeatNoncomparableStore{heartbeatBorrowedStore: fixture.borrowed, marker: []byte{1}}
			case "noncomparable_interface_payload":
				store = heartbeatInterfaceValueStore{heartbeatBorrowedStore: fixture.borrowed, marker: []byte{1}}
			case "noncomparable_returned_owner":
				store = heartbeatReturnedOwnerStore{heartbeatBorrowedStore: fixture.borrowed}
			}
			configureHeartbeatStore(t, fixture, store)
			if err := fixture.process("idle_heartbeat"); err != nil {
				t.Fatal(err)
			}
			if kind == "explicit_decorator" {
				if explicit.borrowedOverrides != 1 || fixture.borrowed.borrowedCalls != 1 || decorator.overrides != 0 || fixture.external.legacyCalls != 0 {
					t.Fatal("explicit owner did not use the decorator's borrowed override")
				}
			} else {
				if fixture.external.legacyCalls != 1 || fixture.borrowed.borrowedCalls != 0 {
					t.Fatal("legacy store contract was bypassed by a promoted capability")
				}
				if kind == "promoted_decorator" && decorator.overrides != 1 {
					t.Fatal("legacy decorator override was bypassed")
				}
			}
		})
	}
}

type heartbeatFailingStore struct {
	*heartbeatBorrowedStore
	failure  error
	attempts int
}

func (store *heartbeatFailingStore) BorrowedStoreOwner() MessageStore { return store }

func (store *heartbeatFailingStore) SaveMessageAndIncrNextSenderMsgSeqNumBorrowed(sequence int, wire []byte) error {
	store.attempts++
	if store.failure != nil {
		return store.failure
	}
	return store.heartbeatBorrowedStore.SaveMessageAndIncrNextSenderMsgSeqNumBorrowed(sequence, wire)
}

func TestHeartbeatStoreFailureReturnsOwnership(t *testing.T) {
	fixture := newHeartbeatStoreFixture(t, "borrowed_external")
	defer fixture.close()
	failure := errors.New("store save rejected")
	store := &heartbeatFailingStore{heartbeatBorrowedStore: fixture.borrowed, failure: failure}
	configureHeartbeatStore(t, fixture, store)
	sender, target := fixture.store.NextSenderMsgSeqNum(), fixture.store.NextTargetMsgSeqNum()
	floor := fixture.store.floor
	retained := bytes.Clone(fixture.store.arena)
	entries := append([]boundedJournalEntry(nil), fixture.store.entries...)
	if err := fixture.session.sendHeartbeat(nil, nil); err != failure {
		t.Fatalf("error=%v, want store rejection", err)
	}
	if store.attempts != 1 || fixture.controlApplication.calls != 1 || fixture.external.legacyCalls != 0 {
		t.Fatal("store rejection bypassed callback or borrowed store dispatch")
	}
	if fixture.store.NextSenderMsgSeqNum() != sender || fixture.store.NextTargetMsgSeqNum() != target || fixture.store.floor != floor || fixture.store.reservedIndex != -1 {
		t.Fatal("failed save changed sequence or journal ownership")
	}
	if !bytes.Equal(fixture.store.arena, retained) {
		t.Fatal("failed save changed retained bytes")
	}
	for index, entry := range entries {
		if fixture.store.entries[index] != entry {
			t.Fatal("failed save changed retained metadata")
		}
	}
	if fixture.session.controls.busy || fixture.session.outbound.count != 0 || len(fixture.session.outbound.vacant) != len(fixture.session.outbound.slots) || fixture.connection.used != 0 {
		t.Fatal("failed save leaked ownership or queued a write")
	}
	store.failure = nil
	if err := fixture.process("idle_heartbeat"); err != nil {
		t.Fatal(err)
	}
	if store.attempts != 2 || fixture.store.NextSenderMsgSeqNum() != sender+1 {
		t.Fatal("recovery did not reuse the rejected sequence")
	}
}

// The legacy store is permitted to retain the exact slice it receives. The
// borrowed store instead copies it before returning from the synchronous call.
type heartbeatRetainingLegacyStore struct {
	*heartbeatLegacyStore
	retained []byte
}

func (store *heartbeatRetainingLegacyStore) SaveMessageAndIncrNextSenderMsgSeqNum(sequence int, wire []byte) error {
	if store.retained == nil {
		store.retained = wire
	}
	return store.heartbeatLegacyStore.SaveMessageAndIncrNextSenderMsgSeqNum(sequence, wire)
}

type heartbeatRetainingBorrowedStore struct {
	*heartbeatBorrowedStore
	ring             *outboundRing
	retained         []byte
	sawOwnedSlotView bool
}

func (store *heartbeatRetainingBorrowedStore) BorrowedStoreOwner() MessageStore { return store }

func (store *heartbeatRetainingBorrowedStore) SaveMessageAndIncrNextSenderMsgSeqNumBorrowed(sequence int, wire []byte) error {
	// Inspect aliases only during the borrowed call, while the sender owns the
	// slot. Never save a borrowed slice for later inspection or mutation.
	for index := range store.ring.slots {
		candidate := store.ring.slots[index].wire
		if len(candidate) != 0 && &candidate[0] == &wire[0] {
			store.sawOwnedSlotView = true
		}
	}
	if store.retained == nil {
		store.retained = bytes.Clone(wire)
	}
	return store.heartbeatBorrowedStore.SaveMessageAndIncrNextSenderMsgSeqNumBorrowed(sequence, wire)
}

func TestHeartbeatStoreRetainedBytesSurviveSlotReuse(t *testing.T) {
	for _, borrowed := range []bool{false, true} {
		name := "legacy_owned_argument"
		if borrowed {
			name = "borrowed_store_owned_copy"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newHeartbeatStoreFixture(t, "borrowed_external")
			defer fixture.close()
			legacy := &heartbeatRetainingLegacyStore{heartbeatLegacyStore: fixture.external}
			capable := &heartbeatRetainingBorrowedStore{heartbeatBorrowedStore: fixture.borrowed}
			var store MessageStore = legacy
			if borrowed {
				store = capable
			}
			configureHeartbeatStore(t, fixture, store)
			capable.ring = fixture.session.outbound
			firstSequence := fixture.store.NextSenderMsgSeqNum()
			if err := fixture.process("test_request"); err != nil {
				t.Fatal(err)
			}
			firstIndex := fixture.store.nextEntry(firstSequence, firstSequence)
			original := bytes.Clone(fixture.store.entryBytes(firstIndex))
			for iteration := 0; iteration < 2*len(fixture.session.outbound.slots); iteration++ {
				if err := fixture.process("idle_heartbeat"); err != nil {
					t.Fatal(err)
				}
			}
			retained := legacy.retained
			if borrowed {
				retained = capable.retained
				if !capable.sawOwnedSlotView {
					t.Fatal("borrowed store did not receive the current sender-owned slot")
				}
			}
			if !bytes.Equal(retained, original) || !bytes.Equal(fixture.store.entryBytes(firstIndex), original) {
				t.Fatal("output slot reuse changed store-owned retained bytes")
			}
		})
	}
}

func TestHeartbeatStoreDisabledPersistenceStillAdvancesSequence(t *testing.T) {
	fixture := newHeartbeatStoreFixture(t, "borrowed_external")
	defer fixture.close()
	fixture.session.DisableMessagePersist = true
	sequence := fixture.store.NextSenderMsgSeqNum()
	if err := fixture.session.sendHeartbeat(nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := completeScopedControl(fixture.scopedOutboundBenchmark); err != nil {
		t.Fatal(err)
	}
	if fixture.store.NextSenderMsgSeqNum() != sequence+1 || fixture.controlApplication.calls != 1 || fixture.borrowed.borrowedCalls != 0 || fixture.external.legacyCalls != 0 {
		t.Fatal("disabled persistence changed callback, sequence or save behavior")
	}
	if fixture.store.nextEntry(sequence, sequence) != -1 {
		t.Fatal("disabled persistence saved a message")
	}
}
