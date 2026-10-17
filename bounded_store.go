package quickfix

import (
	"errors"
	"sync/atomic"
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
// NewBoundedStore. The session serializes sender and journal operations using
// its existing send and resend locks. Sequence getters and target-counter
// operations may run concurrently with sender operations; they do not borrow
// journal storage. Direct journal, retirement, reset and creation-time operations
// require a quiescent owner, and journal callbacks must not mutate the store.
// The store is not durable. Neither completed writes nor new messages evict
// retained data. Only an explicit RetireBefore acknowledgement or Reset releases
// old entries. Capacity bounds the sequence window starting at the recovery floor,
// including gaps, rather than merely the number of populated entries.
type BoundedStore struct {
	entries          []boundedJournalEntry
	arena            []byte
	maxMessageBytes  int
	nextSender       atomic.Int64
	nextTarget       atomic.Int64
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
	store := &BoundedStore{
		entries:         make([]boundedJournalEntry, capacity),
		arena:           make([]byte, capacity*maxMessageBytes),
		maxMessageBytes: maxMessageBytes,
		floor:           1,
		creationTime:    time.Now(), reservedIndex: -1,
	}
	store.nextSender.Store(1)
	store.nextTarget.Store(1)
	return store, nil
}

func (store *BoundedStore) NextSenderMsgSeqNum() int { return int(store.nextSender.Load()) }
func (store *BoundedStore) NextTargetMsgSeqNum() int { return int(store.nextTarget.Load()) }

func (store *BoundedStore) preparedStoreOwner() MessageStore { return store }

func incrementJournalSequence(sequence *atomic.Int64) error {
	for {
		next := sequence.Load()
		if next == int64(int(^uint(0)>>1)) {
			return ErrInvalidJournalSequence
		}
		if sequence.CompareAndSwap(next, next+1) {
			return nil
		}
	}
}

func (store *BoundedStore) IncrNextSenderMsgSeqNum() error {
	if store.reservedIndex >= 0 {
		return ErrJournalReservation
	}
	return incrementJournalSequence(&store.nextSender)
}

func (store *BoundedStore) IncrNextTargetMsgSeqNum() error {
	return incrementJournalSequence(&store.nextTarget)
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
	store.nextSender.Store(int64(next))
	return nil
}

func (store *BoundedStore) SetNextTargetMsgSeqNum(next int) error {
	if next < 1 {
		return ErrInvalidJournalSequence
	}
	store.nextTarget.Store(int64(next))
	return nil
}

func (store *BoundedStore) CreationTime() time.Time         { return store.creationTime }
func (store *BoundedStore) SetCreationTime(value time.Time) { store.creationTime = value }

// reserve holds exactly one entry for the session's next encode. Reservation
// changes no committed bytes, sequences, recovery floor, or creation time.
// Commit or abort it before another sender or journal mutation. Independent
// target-counter updates may proceed while the sender holds a reservation.
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
	// Subtract rather than computing floor+capacity, which can overflow near
	// the maximum supported sequence number.
	if sequence-store.floor >= len(store.entries) {
		return -1, ErrJournalFull
	}
	index := (sequence - 1) % len(store.entries)
	if existing := store.entries[index].sequence; existing != 0 && existing != sequence {
		return -1, ErrJournalFull
	}
	store.reservedIndex, store.reservedSequence = index, sequence
	return index, nil
}

func (store *BoundedStore) commit(index, sequence int, wire []byte, incr bool) error {
	if index < 0 || index != store.reservedIndex || sequence != store.reservedSequence || store.reservedReset {
		return ErrJournalReservation
	}
	if len(wire) > store.maxMessageBytes {
		return ErrJournalMessageCapacity
	}
	if incr && (sequence != store.NextSenderMsgSeqNum() || sequence == int(^uint(0)>>1)) {
		return ErrInvalidJournalSequence
	}
	start := index * store.maxMessageBytes
	copy(store.arena[start:start+len(wire)], wire)
	store.entries[index] = boundedJournalEntry{sequence: sequence, length: len(wire)}
	if incr {
		store.nextSender.Store(int64(sequence + 1))
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

// commitReset starts a new sequence space with the encoded logon at sequence 1.
// It is an owner-serialized transaction: encoding and every fallible precheck
// precede changes to retained bytes, sequences, recovery floor and creation time.
// The two atomic counters do not form a concurrently atomic snapshot; reset
// requires the owner to quiesce other store mutations and journal readers.
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
	store.nextSender.Store(2)
	store.nextTarget.Store(1)
	store.floor = 1
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
	if nextSequence < store.floor || nextSequence > store.NextSenderMsgSeqNum() {
		return ErrJournalRangeUnavailable
	}
	count := min(nextSequence-store.floor, len(store.entries))
	index := (store.floor - 1) % len(store.entries)
	for range count {
		store.entries[index] = boundedJournalEntry{}
		index++
		if index == len(store.entries) {
			index = 0
		}
	}
	store.floor = nextSequence
	return nil
}

// retainedRange checks the complete requested recovery window before any
// resend is emitted. Missing entries within the known window are allowed:
// admin or deliberately skipped sequence numbers can require gap fills.
func (store *BoundedStore) retainedRange(begin, end int) error {
	if begin < store.floor || end < begin || end >= store.NextSenderMsgSeqNum() {
		return ErrJournalRangeUnavailable
	}
	return nil
}

// message borrows read-only journal bytes. The borrow ends before the next
// store mutation; it cannot outlive replacement, retirement, or reset.
func (store *BoundedStore) message(sequence int) ([]byte, bool) {
	if sequence < store.floor || sequence-store.floor >= len(store.entries) {
		return nil, false
	}
	index := (sequence - 1) % len(store.entries)
	if store.entries[index].sequence == sequence {
		return store.entryBytes(index), true
	}
	return nil, false
}

func (store *BoundedStore) entryBytes(index int) []byte {
	start := index * store.maxMessageBytes
	end := start + store.entries[index].length
	return store.arena[start:end:end]
}

// nextEntry scans only the intersection with the configured sequence window.
// Successive calls advance begin, so a complete iteration examines each window
// position at most once even when the requested range contains huge gaps.
func (store *BoundedStore) nextEntry(begin, end int) int {
	begin = max(begin, store.floor)
	end = min(end, store.floor+min(len(store.entries)-1, int(^uint(0)>>1)-store.floor))
	if begin > end {
		return -1
	}
	index := (begin - 1) % len(store.entries)
	for sequence := begin; ; sequence++ {
		if store.entries[index].sequence == sequence {
			return index
		}
		if sequence == end {
			return -1
		}
		index++
		if index == len(store.entries) {
			index = 0
		}
	}
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
	store.nextSender.Store(1)
	store.nextTarget.Store(1)
	store.floor = 1
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
