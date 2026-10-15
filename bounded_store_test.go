package quickfix

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type boundedStoreSnapshot struct {
	entries                []boundedJournalEntry
	arena                  []byte
	nextSender, nextTarget int
	floor                  int
	creation               time.Time
}

func snapshotBoundedStore(store *BoundedStore) boundedStoreSnapshot {
	return boundedStoreSnapshot{
		entries:    append([]boundedJournalEntry(nil), store.entries...),
		arena:      append([]byte(nil), store.arena...),
		nextSender: store.NextSenderMsgSeqNum(), nextTarget: store.NextTargetMsgSeqNum(),
		floor: store.floor, creation: store.CreationTime(),
	}
}

func newBoundedStoreForTest(t *testing.T, capacity, maxBytes int) *BoundedStore {
	t.Helper()
	store, err := NewBoundedStore(capacity, maxBytes)
	require.NoError(t, err)
	return store
}

func TestBoundedStoreConstructorAndFactory(t *testing.T) {
	maximum := int(^uint(0) >> 1)
	for _, limits := range [][2]int{{0, 1}, {1, 0}, {-1, 1}, {1, -1}, {maximum, 1}, {1, maximum}, {maximum / 1024, 2048}} {
		store, err := NewBoundedStore(limits[0], limits[1])
		require.Nil(t, store)
		require.ErrorIs(t, err, ErrInvalidJournalLimits)
		factory := NewBoundedStoreFactory(limits[0], limits[1])
		created, err := factory.Create(SessionID{})
		require.Nil(t, created)
		require.ErrorIs(t, err, ErrInvalidJournalLimits)
	}
	factory := NewBoundedStoreFactory(2, 8)
	first, err := factory.Create(SessionID{})
	require.NoError(t, err)
	second, err := factory.Create(SessionID{})
	require.NoError(t, err)
	require.NotSame(t, first, second)
	require.NoError(t, first.SaveMessageAndIncrNextSenderMsgSeqNum(1, []byte("first")))
	require.Equal(t, 1, second.NextSenderMsgSeqNum())
	store := first.(*BoundedStore)
	require.Len(t, store.entries, 2)
	require.Equal(t, 2, cap(store.entries))
	require.Len(t, store.arena, 16)
	require.Equal(t, 16, cap(store.arena))
	require.False(t, store.CreationTime().IsZero())
}

func TestBoundedStoreCopiesInputAndBoundsBorrow(t *testing.T) {
	store := newBoundedStoreForTest(t, 2, 8)
	input := []byte("12345678")
	require.NoError(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(1, input))
	require.NoError(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(2, []byte("next")))
	input[0] = 'X'
	first, ok := store.message(1)
	require.True(t, ok)
	require.Equal(t, "12345678", string(first))
	require.Equal(t, len(first), cap(first), "borrowed slices must not expose neighboring storage")
	second, ok := store.message(2)
	require.True(t, ok)
	require.Equal(t, "next", string(second))
	require.Equal(t, len(second), cap(second))
	require.NoError(t, store.SaveMessage(1, []byte("short")))
	first, ok = store.message(1)
	require.True(t, ok)
	require.Equal(t, "short", string(first))
	require.Equal(t, 5, cap(first))
	second, ok = store.message(2)
	require.True(t, ok)
	require.Equal(t, "next", string(second), "replacement crossed the slot boundary")
	require.NoError(t, store.SaveMessage(1, nil))
	first, ok = store.message(1)
	require.True(t, ok, "an empty stored value still occupies its sequence entry")
	require.Empty(t, first)
	require.Zero(t, cap(first))
}

func TestBoundedStoreReservationAbortAndFullAtomicity(t *testing.T) {
	store := newBoundedStoreForTest(t, 1, 8)
	before := snapshotBoundedStore(store)
	index, err := store.reserve(1)
	require.NoError(t, err)
	require.Equal(t, before, snapshotBoundedStore(store))
	_, err = store.reserve(1)
	require.ErrorIs(t, err, ErrJournalReservation)
	store.abort(index + 1)
	require.Equal(t, index, store.reservedIndex, "wrong-index abort must not release another reservation")
	require.ErrorIs(t, store.commit(index, 1, []byte("too-large"), true), ErrJournalMessageCapacity)
	require.Equal(t, before, snapshotBoundedStore(store))
	store.abort(index)
	require.Equal(t, before, snapshotBoundedStore(store))
	index, err = store.reserve(1)
	require.NoError(t, err)
	require.ErrorIs(t, store.commit(index, 2, []byte("wrong"), true), ErrJournalReservation)
	require.Equal(t, before, snapshotBoundedStore(store))
	require.NoError(t, store.commit(index, 1, []byte("retained"), true))
	require.Equal(t, 2, store.NextSenderMsgSeqNum())
	before = snapshotBoundedStore(store)
	index, err = store.reserve(2)
	require.Equal(t, -1, index)
	require.ErrorIs(t, err, ErrJournalFull)
	require.ErrorIs(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(2, []byte("next")), ErrJournalFull)
	require.Equal(t, before, snapshotBoundedStore(store))
	index, err = store.reserve(1)
	require.NoError(t, err, "replacement must reserve the existing entry in a full journal")
	store.abort(index)
	require.Equal(t, before, snapshotBoundedStore(store))
	require.NoError(t, store.SaveMessage(1, []byte("new")))
	require.Equal(t, 2, store.NextSenderMsgSeqNum(), "replacement must not advance sender state")
	wire, ok := store.message(1)
	require.True(t, ok)
	require.Equal(t, "new", string(wire))
}

func TestBoundedStoreRetirementAndRangePreflight(t *testing.T) {
	store := newBoundedStoreForTest(t, 3, 16)
	// Save out of order, including an intentional gap at sequence 2.
	require.NoError(t, store.SaveMessage(3, []byte("three")))
	require.NoError(t, store.SaveMessage(1, []byte("one")))
	require.NoError(t, store.SetNextSenderMsgSeqNum(4))
	require.NoError(t, store.retainedRange(1, 3))
	var seen []string
	require.NoError(t, store.IterateMessages(1, 3, func(wire []byte) error {
		seen = append(seen, string(wire))
		return nil
	}))
	require.Equal(t, []string{"one", "three"}, seen)
	for _, bounds := range [][2]int{{0, 3}, {1, 4}, {3, 2}, {4, 4}} {
		called := false
		err := store.IterateMessages(bounds[0], bounds[1], func([]byte) error {
			called = true
			return nil
		})
		require.ErrorIs(t, err, ErrJournalRangeUnavailable)
		require.False(t, called, "unavailable range must fail before any resend callback")
	}
	require.NoError(t, store.RetireBefore(3))
	_, ok := store.message(1)
	require.False(t, ok)
	wire, ok := store.message(3)
	require.True(t, ok)
	require.Equal(t, "three", string(wire))
	require.ErrorIs(t, store.retainedRange(1, 3), ErrJournalRangeUnavailable)
	require.NoError(t, store.retainedRange(3, 3))
	before := snapshotBoundedStore(store)
	for _, floor := range []int{-1, 0, 2, 5} {
		require.ErrorIs(t, store.RetireBefore(floor), ErrJournalRangeUnavailable)
		require.Equal(t, before, snapshotBoundedStore(store))
	}
	require.ErrorIs(t, store.SaveMessage(2, []byte("retired")), ErrJournalRangeUnavailable)
	require.ErrorIs(t, store.SetNextSenderMsgSeqNum(2), ErrJournalRangeUnavailable)
	require.Equal(t, before, snapshotBoundedStore(store))
	require.NoError(t, store.RetireBefore(3), "acknowledging the current floor is idempotent")
	require.NoError(t, store.RetireBefore(4))
	require.NoError(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(4, []byte("reused")))
	require.NoError(t, store.retainedRange(4, 4))
}

func TestBoundedStoreSparseRangeAndCallbackError(t *testing.T) {
	store := newBoundedStoreForTest(t, 2, 8)
	maximum := int(^uint(0) >> 1)
	require.NoError(t, store.SaveMessage(1, []byte("first")))
	require.NoError(t, store.SaveMessage(maximum-1, []byte("last")))
	require.NoError(t, store.SetNextSenderMsgSeqNum(maximum))
	count := 0
	require.NoError(t, store.IterateMessages(1, maximum-1, func([]byte) error {
		count++
		return nil
	}))
	require.Equal(t, 2, count, "iteration must be bounded by stored entries, not sequence gap size")
	want := errors.New("stop resend")
	count = 0
	err := store.IterateMessages(1, maximum-1, func([]byte) error {
		count++
		return want
	})
	require.ErrorIs(t, err, want)
	require.Equal(t, 1, count)
}

func TestBoundedStoreStagedResetPreservesOldJournalUntilCommit(t *testing.T) {
	store := newBoundedStoreForTest(t, 2, 8)
	require.NoError(t, store.SaveMessage(5, []byte("old-wire")))
	require.NoError(t, store.SaveMessage(6, []byte("other")))
	require.NoError(t, store.SetNextSenderMsgSeqNum(7))
	require.NoError(t, store.SetNextTargetMsgSeqNum(9))
	require.NoError(t, store.RetireBefore(5))
	store.SetCreationTime(time.Unix(100, 0))
	before := snapshotBoundedStore(store)
	index, err := store.reserveReset()
	require.NoError(t, err, "a full journal must still permit staging an explicit reset")
	require.Equal(t, before, snapshotBoundedStore(store))
	require.NoError(t, store.retainedRange(5, 6))
	wire, ok := store.message(5)
	require.True(t, ok)
	require.Equal(t, "old-wire", string(wire))
	require.ErrorIs(t, store.commitReset(index, []byte("too-large")), ErrJournalMessageCapacity)
	require.Equal(t, before, snapshotBoundedStore(store))
	store.abort(index)
	require.Equal(t, before, snapshotBoundedStore(store))
	index, err = store.reserveReset()
	require.NoError(t, err)
	require.ErrorIs(t, store.commit(index, 1, []byte("wrong"), true), ErrJournalReservation)
	require.Equal(t, before, snapshotBoundedStore(store))
	require.NoError(t, store.commitReset(index, []byte("logon")))
	require.Equal(t, 2, store.NextSenderMsgSeqNum())
	require.Equal(t, 1, store.NextTargetMsgSeqNum())
	require.Equal(t, 1, store.floor)
	require.True(t, store.CreationTime().After(before.creation))
	_, ok = store.message(5)
	require.False(t, ok)
	_, ok = store.message(6)
	require.False(t, ok, "reset must clear entries other than its reserved destination")
	wire, ok = store.message(1)
	require.True(t, ok)
	require.Equal(t, "logon", string(wire))
	require.NoError(t, store.retainedRange(1, 1))
	require.ErrorIs(t, store.retainedRange(1, 5), ErrJournalRangeUnavailable)
}

func TestBoundedStoreReservationRequiresCommitOrAbort(t *testing.T) {
	store := newBoundedStoreForTest(t, 1, 8)
	before := snapshotBoundedStore(store)
	index, err := store.reserve(1)
	require.NoError(t, err)
	require.ErrorIs(t, store.IncrNextSenderMsgSeqNum(), ErrJournalReservation)
	require.ErrorIs(t, store.IncrNextTargetMsgSeqNum(), ErrJournalReservation)
	require.ErrorIs(t, store.SetNextSenderMsgSeqNum(2), ErrJournalReservation)
	require.ErrorIs(t, store.SetNextTargetMsgSeqNum(2), ErrJournalReservation)
	require.ErrorIs(t, store.RetireBefore(1), ErrJournalReservation)
	require.ErrorIs(t, store.Reset(), ErrJournalReservation)
	_, err = store.reserveReset()
	require.ErrorIs(t, err, ErrJournalReservation)
	require.Equal(t, before, snapshotBoundedStore(store))
	store.abort(index)
	require.NoError(t, store.Reset())
}

func TestBoundedStoreSequenceOverflowAndInvalidValues(t *testing.T) {
	store := newBoundedStoreForTest(t, 1, 8)
	before := snapshotBoundedStore(store)
	for _, value := range []int{-1, 0} {
		require.ErrorIs(t, store.SetNextSenderMsgSeqNum(value), ErrInvalidJournalSequence)
		require.ErrorIs(t, store.SetNextTargetMsgSeqNum(value), ErrInvalidJournalSequence)
		require.ErrorIs(t, store.SaveMessage(value, []byte("invalid")), ErrInvalidJournalSequence)
		require.Equal(t, before, snapshotBoundedStore(store))
	}
	require.ErrorIs(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(2, []byte("gap")), ErrInvalidJournalSequence)
	require.Equal(t, before, snapshotBoundedStore(store))
	maximum := int(^uint(0) >> 1)
	require.NoError(t, store.SetNextSenderMsgSeqNum(maximum))
	require.NoError(t, store.SetNextTargetMsgSeqNum(maximum))
	before = snapshotBoundedStore(store)
	require.ErrorIs(t, store.IncrNextSenderMsgSeqNum(), ErrInvalidJournalSequence)
	require.ErrorIs(t, store.IncrNextTargetMsgSeqNum(), ErrInvalidJournalSequence)
	require.ErrorIs(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(maximum, []byte("last")), ErrInvalidJournalSequence)
	require.Equal(t, before, snapshotBoundedStore(store))
	require.Equal(t, -1, store.reservedIndex, "failed convenience commit must abort its reservation")
}

func TestBoundedStoreLegacySnapshotsSurviveReuseAndReset(t *testing.T) {
	store := newBoundedStoreForTest(t, 1, 8)
	require.NoError(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(1, []byte("first")))
	messages, err := store.GetMessages(1, 1)
	require.NoError(t, err)
	require.Equal(t, [][]byte{[]byte("first")}, messages)
	require.NoError(t, store.RetireBefore(2))
	require.NoError(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(2, []byte("second")))
	require.NoError(t, store.Reset())
	require.NoError(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(1, []byte("third")))
	require.Equal(t, "first", string(messages[0]))
	require.NoError(t, store.Refresh())
	require.NoError(t, store.Close())
	wire, ok := store.message(1)
	require.True(t, ok)
	require.Equal(t, "third", string(wire))
}

func TestBoundedStoreZeroAllocationCycle(t *testing.T) {
	store := newBoundedStoreForTest(t, 2, 16)
	wire := []byte("fixed-wire")
	visited := 0
	visit := func(message []byte) error {
		if len(message) != len(wire) || message[0] != wire[0] {
			panic("unexpected retained wire")
		}
		visited++
		return nil
	}
	cycle := func() {
		sequence := store.NextSenderMsgSeqNum()
		index, err := store.reserve(sequence)
		if err != nil {
			panic(err)
		}
		if err := store.commit(index, sequence, wire, true); err != nil {
			panic(err)
		}
		if err := store.retainedRange(sequence, sequence); err != nil {
			panic(err)
		}
		if _, ok := store.message(sequence); !ok {
			panic("missing message")
		}
		if err := store.IterateMessages(sequence, sequence, visit); err != nil {
			panic(err)
		}
		if err := store.RetireBefore(sequence + 1); err != nil {
			panic(err)
		}
	}
	cycle()
	allocations := testing.AllocsPerRun(1000, cycle)
	require.Zero(t, allocations)
	require.Equal(t, 1002, visited)
	require.Len(t, store.entries, 2)
	require.Len(t, store.arena, 32)
}

func TestBoundedStoreZeroAllocationFailuresAndReset(t *testing.T) {
	store := newBoundedStoreForTest(t, 1, 8)
	wire, oversized := []byte("wire"), []byte("too-large")
	require.NoError(t, store.SaveMessageAndIncrNextSenderMsgSeqNum(1, wire))
	cycle := func() {
		if err := store.SaveMessageAndIncrNextSenderMsgSeqNum(2, wire); err != ErrJournalFull {
			panic("full journal accepted another entry")
		}
		if err := store.SaveMessage(1, oversized); err != ErrJournalMessageCapacity {
			panic("oversized replacement succeeded")
		}
		index, err := store.reserveReset()
		if err != nil {
			panic(err)
		}
		if err := store.commitReset(index, oversized); err != ErrJournalMessageCapacity {
			panic("oversized reset succeeded")
		}
		store.abort(index)
		index, err = store.reserveReset()
		if err != nil {
			panic(err)
		}
		if err := store.commitReset(index, wire); err != nil {
			panic(err)
		}
	}
	cycle()
	require.Zero(t, testing.AllocsPerRun(1000, cycle))
	require.Equal(t, 2, store.NextSenderMsgSeqNum())
	require.Equal(t, 1, store.NextTargetMsgSeqNum())
}
