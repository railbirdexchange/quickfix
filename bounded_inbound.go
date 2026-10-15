package quickfix

import (
	"errors"

	"github.com/quickfixgo/quickfix/datadictionary"
)

var (
	ErrInvalidInboundLimits = errors.New("quickfix: invalid inbound ring limits")
	ErrInboundStashCapacity = errors.New("quickfix: inbound stash capacity exceeded")
)

// inboundSlot transfers as one unit: its frame and parsed indexes always have
// the same owner. A retained slot belongs to the session's out-of-order stash.
type inboundSlot struct {
	frame    []byte
	parsed   *MessageSlot
	ring     *inboundRing
	retained bool
}

// inboundRing owns fixed storage. Only the session owner may inspect or change
// stash entries. The vacant channel transfers released slots to the reader;
// neither the channel nor the ring is reset while any slot remains in use.
type inboundRing struct {
	vacant chan *inboundSlot
	slots  []inboundSlot
	stash  []inboundStashEntry
}

type inboundStashEntry struct {
	seq  int
	slot *inboundSlot
}

func newInboundRing(transport, application *datadictionary.DataDictionary, limits ParseLimits, queueDepth, maxStash int) (*inboundRing, error) {
	maximum := int(^uint(0) >> 1)
	if queueDepth < 0 || maxStash < 0 || maxStash > maximum-2 || queueDepth > maximum-maxStash-2 {
		return nil, ErrInvalidInboundLimits
	}
	// Two additional slots cover the reader and the active session message.
	// This headroom prevents retained out-of-order frames from consuming every
	// slot required to receive and process the missing resend messages.
	count := queueDepth + maxStash + 2
	// Leave conservative arithmetic headroom for slot, stash, and channel
	// backing storage, as MessageSlot does for its fixed field indexes.
	if count > maximum/1024 {
		return nil, ErrInvalidInboundLimits
	}
	first, err := NewMessageSlot(transport, application, limits)
	if err != nil {
		return nil, err
	}
	if count > maximum/limits.MaxFrameBytes {
		return nil, ErrInvalidInboundLimits
	}
	ring := &inboundRing{
		vacant: make(chan *inboundSlot, count),
		slots:  make([]inboundSlot, count),
		stash:  make([]inboundStashEntry, maxStash),
	}
	frames := make([]byte, count*limits.MaxFrameBytes)
	for index := range ring.slots {
		parsed := first
		if index > 0 {
			parsed, err = NewMessageSlot(transport, application, limits)
			if err != nil {
				return nil, err
			}
		}
		start := index * limits.MaxFrameBytes
		end := start + limits.MaxFrameBytes
		slot := &ring.slots[index]
		*slot = inboundSlot{frame: frames[start:start:end], parsed: parsed, ring: ring}
		ring.vacant <- slot
	}
	return ring, nil
}

// release ends all frame/message borrows before returning the slot. The current
// owner calls it exactly once and must not access the slot after the send.
func (slot *inboundSlot) release() {
	slot.parsed.message.inbound = nil
	slot.parsed.Reset()
	slot.frame = slot.frame[:0]
	slot.retained = false
	slot.ring.vacant <- slot
}

// stashIndex checks capacity without changing retention or message state. The
// session must call it before resend/store side effects and keep exclusive stash
// ownership through retain. Replacing an existing sequence needs no new entry.
func (ring *inboundRing) stashIndex(seq int) (int, error) {
	vacant := -1
	for index := range ring.stash {
		entry := &ring.stash[index]
		if entry.slot == nil {
			if vacant < 0 {
				vacant = index
			}
		} else if entry.seq == seq {
			return index, nil
		}
	}
	if vacant < 0 {
		return -1, ErrInboundStashCapacity
	}
	return vacant, nil
}

// retain consumes the caller-owned slot at the index returned by stashIndex.
// An older slot for the same sequence is released before replacing its entry.
func (ring *inboundRing) retain(index, seq int, slot *inboundSlot) {
	previous := ring.stash[index].slot
	if previous != nil && previous != slot {
		previous.release()
	}
	ring.stash[index] = inboundStashEntry{seq: seq, slot: slot}
	slot.retained = true
}

// take transfers a retained slot back to active session processing. It leaves
// the frame and parsed message intact until that owner releases the slot.
func (ring *inboundRing) take(seq int) *inboundSlot {
	for index := range ring.stash {
		entry := &ring.stash[index]
		if entry.slot != nil && entry.seq == seq {
			slot := entry.slot
			*entry = inboundStashEntry{}
			slot.retained = false
			return slot
		}
	}
	return nil
}

func (ring *inboundRing) discardBefore(seq int) {
	for index := range ring.stash {
		entry := &ring.stash[index]
		if entry.slot != nil && entry.seq < seq {
			slot := entry.slot
			*entry = inboundStashEntry{}
			slot.release()
		}
	}
}

func (ring *inboundRing) clearStash() {
	for index := range ring.stash {
		entry := &ring.stash[index]
		if entry.slot != nil {
			slot := entry.slot
			*entry = inboundStashEntry{}
			slot.release()
		}
	}
}
