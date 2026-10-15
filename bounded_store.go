package quickfix

import (
	"errors"
	"time"
)

var (
	ErrInvalidJournalLimits    = errors.New("quickfix: invalid bounded journal limits")
	ErrJournalFull             = errors.New("quickfix: bounded journal is full")
	ErrJournalMessageCapacity  = errors.New("quickfix: message exceeds bounded journal capacity")
	ErrJournalRangeUnavailable = errors.New("quickfix: requested journal range is unavailable")
	ErrInvalidJournalSequence  = errors.New("quickfix: invalid bounded journal sequence")
	ErrJournalReservation      = errors.New("quickfix: invalid or active bounded journal reservation")
)

type boundedJournalEntry struct {
	sequence int
	length   int
}

// BoundedStore is a fixed-capacity, in-memory resend journal. Construct it with
// NewBoundedStore. The session owns the store exclusively; callers must not use
// it concurrently or mutate it from an IterateMessages callback. It is not a
// durable store. Neither completed writes nor new messages evict retained data.
// Only an explicit RetireBefore acknowledgement or Reset releases old entries.
type BoundedStore struct {
	entries          []boundedJournalEntry
	arena            []byte
	maxMessageBytes  int
	nextSender       int
	nextTarget       int
	floor            int
	creationTime     time.Time
	reservedIndex    int
	reservedSequence int
	reservedReset    bool
}

// NewBoundedStore allocates every journal entry and message byte up front.
// capacity and maxMessageBytes must both be positive. Successful warmed store
// operations do not allocate, except the compatibility snapshot GetMessages.
func NewBoundedStore(capacity, maxMessageBytes int) (*BoundedStore, error) {
	maximum := int(^uint(0) >> 1)
	if capacity <= 0 || maxMessageBytes <= 0 || capacity > maximum/1024 || maxMessageBytes > maximum/1024 || capacity > maximum/maxMessageBytes {
		return nil, ErrInvalidJournalLimits
	}
	return &BoundedStore{
		entries:         make([]boundedJournalEntry, capacity),
		arena:           make([]byte, capacity*maxMessageBytes),
		maxMessageBytes: maxMessageBytes,
		nextSender:      1, nextTarget: 1, floor: 1,
		creationTime: time.Now(), reservedIndex: -1,
	}, nil
}

func (store *BoundedStore) NextSenderMsgSeqNum() int { return store.nextSender }
func (store *BoundedStore) NextTargetMsgSeqNum() int { return store.nextTarget }

func (store *BoundedStore) IncrNextSenderMsgSeqNum() error {
	if store.reservedIndex >= 0 {
		return ErrJournalReservation
	}
	if store.nextSender == int(^uint(0)>>1) {
		return ErrInvalidJournalSequence
	}
	store.nextSender++
	return nil
}

func (store *BoundedStore) IncrNextTargetMsgSeqNum() error {
	if store.reservedIndex >= 0 {
		return ErrJournalReservation
	}
	if store.nextTarget == int(^uint(0)>>1) {
		return ErrInvalidJournalSequence
	}
	store.nextTarget++
	return nil
}

func (store *BoundedStore) SetNextSenderMsgSeqNum(next int) error {
	if store.reservedIndex >= 0 {
		return ErrJournalReservation
	}
	if next < 1 {
		return ErrInvalidJournalSequence
	}
	if next < store.floor {
		return ErrJournalRangeUnavailable
	}
	store.nextSender = next
	return nil
}

func (store *BoundedStore) SetNextTargetMsgSeqNum(next int) error {
	if store.reservedIndex >= 0 {
		return ErrJournalReservation
	}
	if next < 1 {
		return ErrInvalidJournalSequence
	}
	store.nextTarget = next
	return nil
}

func (store *BoundedStore) CreationTime() time.Time         { return store.creationTime }
func (store *BoundedStore) SetCreationTime(value time.Time) { store.creationTime = value }

// reserve holds exactly one entry for the session's next encode. Reservation
// changes no committed bytes, sequences, recovery floor, or creation time.
// Commit or abort it before making another mutating store call.
func (store *BoundedStore) reserve(sequence int) (int, error) {
	if store.reservedIndex >= 0 {
		return -1, ErrJournalReservation
	}
	if sequence < 1 {
		return -1, ErrInvalidJournalSequence
	}
	if sequence < store.floor {
		return -1, ErrJournalRangeUnavailable
	}
	vacant := -1
	for index := range store.entries {
		entry := &store.entries[index]
		if entry.sequence == sequence {
			store.reservedIndex, store.reservedSequence = index, sequence
			return index, nil
		}
		if entry.sequence == 0 && vacant < 0 {
			vacant = index
		}
	}
	if vacant < 0 {
		return -1, ErrJournalFull
	}
	store.reservedIndex, store.reservedSequence = vacant, sequence
	return vacant, nil
}

func (store *BoundedStore) commit(index, sequence int, wire []byte, incr bool) error {
	if index < 0 || index != store.reservedIndex || sequence != store.reservedSequence || store.reservedReset {
		return ErrJournalReservation
	}
	if len(wire) > store.maxMessageBytes {
		return ErrJournalMessageCapacity
	}
	if incr && (sequence != store.nextSender || store.nextSender == int(^uint(0)>>1)) {
		return ErrInvalidJournalSequence
	}
	start := index * store.maxMessageBytes
	copy(store.arena[start:start+len(wire)], wire)
	store.entries[index] = boundedJournalEntry{sequence: sequence, length: len(wire)}
	if incr {
		store.nextSender++
	}
	store.abort(index)
	return nil
}

// abort releases only reservation bookkeeping. In particular, aborting a
// replacement or staged reset preserves every previously retained message.
func (store *BoundedStore) abort(index int) {
	if index >= 0 && index == store.reservedIndex {
		store.reservedIndex, store.reservedSequence = -1, 0
		store.reservedReset = false
	}
}

// reserveReset stages a reset logon even when the journal is full. The old
// journal remains readable and unchanged until commitReset succeeds.
func (store *BoundedStore) reserveReset() (int, error) {
	if store.reservedIndex >= 0 {
		return -1, ErrJournalReservation
	}
	store.reservedIndex, store.reservedSequence, store.reservedReset = 0, 1, true
	return 0, nil
}

// commitReset atomically starts a new sequence space with the encoded logon at
// sequence 1. Encoding must be complete before this call. All prechecks precede
// changes to retained bytes, sequences, recovery floor, and creation time.
func (store *BoundedStore) commitReset(index int, wire []byte) error {
	if index < 0 || index != store.reservedIndex || !store.reservedReset {
		return ErrJournalReservation
	}
	if len(wire) > store.maxMessageBytes {
		return ErrJournalMessageCapacity
	}
	start := index * store.maxMessageBytes
	copy(store.arena[start:start+len(wire)], wire)
	clear(store.entries)
	store.entries[index] = boundedJournalEntry{sequence: 1, length: len(wire)}
	store.nextSender, store.nextTarget, store.floor = 2, 1, 1
	store.creationTime = time.Now()
	store.abort(index)
	return nil
}

func (store *BoundedStore) SaveMessage(sequence int, wire []byte) error {
	return store.save(sequence, wire, false)
}

func (store *BoundedStore) SaveMessageAndIncrNextSenderMsgSeqNum(sequence int, wire []byte) error {
	return store.save(sequence, wire, true)
}

func (store *BoundedStore) save(sequence int, wire []byte, incr bool) error {
	if len(wire) > store.maxMessageBytes {
		return ErrJournalMessageCapacity
	}
	index, err := store.reserve(sequence)
	if err != nil {
		return err
	}
	defer store.abort(index)
	return store.commit(index, sequence, wire, incr)
}

// RetireBefore acknowledges that messages below nextSequence are no longer
// required for recovery. The session owner must obtain that acknowledgement
// from its recovery policy, never infer it from a completed socket write.
// Retirement cannot move backwards or beyond NextSenderMsgSeqNum.
func (store *BoundedStore) RetireBefore(nextSequence int) error {
	if store.reservedIndex >= 0 {
		return ErrJournalReservation
	}
	if nextSequence < store.floor || nextSequence > store.nextSender {
		return ErrJournalRangeUnavailable
	}
	for index := range store.entries {
		if store.entries[index].sequence < nextSequence {
			store.entries[index] = boundedJournalEntry{}
		}
	}
	store.floor = nextSequence
	return nil
}

// retainedRange checks the complete requested recovery window before any
// resend is emitted. Missing entries within the known window are allowed:
// admin or deliberately skipped sequence numbers can require gap fills.
func (store *BoundedStore) retainedRange(begin, end int) error {
	if begin < store.floor || end < begin || end >= store.nextSender {
		return ErrJournalRangeUnavailable
	}
	return nil
}

// message borrows read-only journal bytes. The borrow ends before the next
// store mutation; it cannot outlive replacement, retirement, or reset.
func (store *BoundedStore) message(sequence int) ([]byte, bool) {
	if sequence < store.floor {
		return nil, false
	}
	for index := range store.entries {
		if store.entries[index].sequence == sequence {
			return store.entryBytes(index), true
		}
	}
	return nil, false
}

func (store *BoundedStore) entryBytes(index int) []byte {
	start := index * store.maxMessageBytes
	end := start + store.entries[index].length
	return store.arena[start:end:end]
}

// nextEntry orders retained entries without per-call sorting or scratch. Its
// work is bounded by journal capacity, including sparse sequence spaces.
func (store *BoundedStore) nextEntry(begin, end int) int {
	selected := -1
	for index := range store.entries {
		sequence := store.entries[index].sequence
		if sequence >= begin && sequence <= end && (selected < 0 || sequence < store.entries[selected].sequence) {
			selected = index
		}
	}
	return selected
}

// IterateMessages calls cb in sequence order with read-only borrowed bytes.
// The callback must finish using the bytes before returning, must not retain
// them, and must not mutate the store. The whole range is checked first.
func (store *BoundedStore) IterateMessages(begin, end int, cb func([]byte) error) error {
	if err := store.retainedRange(begin, end); err != nil {
		return err
	}
	for {
		index := store.nextEntry(begin, end)
		if index < 0 {
			return nil
		}
		if err := cb(store.entryBytes(index)); err != nil {
			return err
		}
		begin = store.entries[index].sequence + 1 // retainedRange excludes maxInt.
	}
}

// GetMessages returns independent message snapshots for compatibility callers.
// It allocates and is outside the bounded zero-allocation contract.
func (store *BoundedStore) GetMessages(begin, end int) ([][]byte, error) {
	if err := store.retainedRange(begin, end); err != nil {
		return nil, err
	}
	count := 0
	for _, entry := range store.entries {
		if entry.sequence >= begin && entry.sequence <= end {
			count++
		}
	}
	if count == 0 {
		return nil, nil
	}
	messages := make([][]byte, count)
	for position := range messages {
		index := store.nextEntry(begin, end)
		messages[position] = append([]byte(nil), store.entryBytes(index)...)
		begin = store.entries[index].sequence + 1
	}
	return messages, nil
}

func (*BoundedStore) Refresh() error { return nil }
func (*BoundedStore) Close() error   { return nil }

// Reset explicitly discards the journal and starts both sequences at 1.
func (store *BoundedStore) Reset() error {
	if store.reservedIndex >= 0 {
		return ErrJournalReservation
	}
	clear(store.entries)
	store.nextSender, store.nextTarget, store.floor = 1, 1, 1
	store.creationTime = time.Now()
	return nil
}

type boundedStoreFactory struct {
	capacity, maxMessageBytes int
}

func (factory boundedStoreFactory) Create(SessionID) (MessageStore, error) {
	store, err := NewBoundedStore(factory.capacity, factory.maxMessageBytes)
	if err != nil {
		return nil, err
	}
	return store, nil
}

// NewBoundedStoreFactory creates independent journals for each session. Invalid
// configuration is reported by Create, matching the MessageStoreFactory API.
func NewBoundedStoreFactory(capacity, maxMessageBytes int) MessageStoreFactory {
	return boundedStoreFactory{capacity: capacity, maxMessageBytes: maxMessageBytes}
}
