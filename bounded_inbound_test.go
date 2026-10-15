package quickfix

import (
	"bytes"
	"testing"
)

func inboundRingTestLimits() ParseLimits {
	return ParseLimits{MaxFrameBytes: 1024, MaxFields: 32, MaxGroupEntries: 8, MaxGroupDepth: 4}
}

func inboundRingTestAcquire(t *testing.T, ring *inboundRing) *inboundSlot {
	t.Helper()
	select {
	case slot := <-ring.vacant:
		return slot
	default:
		t.Fatal("expected a vacant inbound slot")
		return nil
	}
}

func inboundRingTestParse(t *testing.T, slot *inboundSlot, wire []byte) {
	t.Helper()
	if len(wire) > cap(slot.frame) {
		t.Fatal("fixture exceeds frame capacity")
	}
	slot.frame = slot.frame[:len(wire)]
	copy(slot.frame, wire)
	if err := slot.parsed.Parse(slot.frame); err != nil {
		t.Fatal(err)
	}
	slot.parsed.message.inbound = slot
}

func assertInboundRingVacant(t *testing.T, ring *inboundRing) {
	t.Helper()
	if len(ring.vacant) != len(ring.slots) {
		t.Fatalf("vacant slots = %d, want %d", len(ring.vacant), len(ring.slots))
	}
	seen := make(map[*inboundSlot]bool, len(ring.slots))
	for range ring.slots {
		slot := inboundRingTestAcquire(t, ring)
		if seen[slot] {
			t.Fatal("vacant channel contains a slot more than once")
		}
		seen[slot] = true
		if slot.retained || len(slot.frame) != 0 || slot.parsed.Message() != nil || slot.parsed.message.inbound != nil {
			t.Fatal("released slot retained frame, parsed message, or ownership state")
		}
		if slot.parsed.message.rawMessage != nil || len(slot.parsed.message.fields) != 0 {
			t.Fatal("released slot retained parsed input references")
		}
		slot.release()
	}
}

func TestInboundRingFixedCapacity(t *testing.T) {
	limits := inboundRingTestLimits()
	for _, size := range []struct{ queue, stash int }{{0, 0}, {1, 2}, {3, 4}} {
		ring, err := newInboundRing(nil, nil, limits, size.queue, size.stash)
		if err != nil {
			t.Fatal(err)
		}
		want := size.queue + size.stash + 2
		if len(ring.slots) != want || cap(ring.vacant) != want || len(ring.vacant) != want || len(ring.stash) != size.stash {
			t.Fatalf("incorrect capacities for queue=%d, stash=%d", size.queue, size.stash)
		}
		owned := make([]*inboundSlot, want)
		for index := range owned {
			owned[index] = inboundRingTestAcquire(t, ring)
			slot := owned[index]
			if slot != &ring.slots[index] || slot.ring != ring || len(slot.frame) != 0 || cap(slot.frame) != limits.MaxFrameBytes || slot.parsed == nil {
				t.Fatal("slot does not own the expected fixed storage")
			}
			slot.frame = slot.frame[:cap(slot.frame)]
			slot.frame[0], slot.frame[len(slot.frame)-1] = byte(index+1), byte(index+1)
		}
		select {
		case <-ring.vacant:
			t.Fatal("ring supplied more slots than its configured capacity")
		default:
		}
		for index, slot := range owned {
			if slot.frame[0] != byte(index+1) || slot.frame[len(slot.frame)-1] != byte(index+1) {
				t.Fatal("slot frame storage overlaps a neighboring slot")
			}
			slot.release()
		}
		assertInboundRingVacant(t, ring)
	}
}

func TestInboundRingInvalidLimits(t *testing.T) {
	maximum := int(^uint(0) >> 1)
	for _, size := range []struct{ queue, stash int }{
		{-1, 0}, {0, -1}, {maximum, 0}, {0, maximum}, {maximum / 2, maximum / 2}, {maximum / 2, 0},
	} {
		if ring, err := newInboundRing(nil, nil, inboundRingTestLimits(), size.queue, size.stash); ring != nil || err != ErrInvalidInboundLimits {
			t.Fatalf("queue=%d stash=%d: ring=%v, error=%v", size.queue, size.stash, ring, err)
		}
	}
	invalidParse := inboundRingTestLimits()
	invalidParse.MaxFields = 0
	if ring, err := newInboundRing(nil, nil, invalidParse, 0, 0); ring != nil || err != ErrInvalidParseLimits {
		t.Fatalf("invalid parse limits: ring=%v, error=%v", ring, err)
	}
	invalidFrames := inboundRingTestLimits()
	invalidFrames.MaxFrameBytes = maximum
	if ring, err := newInboundRing(nil, nil, invalidFrames, 0, 0); ring != nil || err != ErrInvalidInboundLimits {
		t.Fatalf("overflowing frame slab: ring=%v, error=%v", ring, err)
	}
}

func TestInboundRingRetainTakeAndRelease(t *testing.T) {
	ring, err := newInboundRing(nil, nil, inboundRingTestLimits(), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	wire := buildFIXT11Wire("35=D\x01", "11=retained\x01")
	slot := inboundRingTestAcquire(t, ring)
	inboundRingTestParse(t, slot, wire)
	index, err := ring.stashIndex(7)
	if err != nil {
		t.Fatal(err)
	}
	if slot.retained || ring.stash[index].slot != nil {
		t.Fatal("capacity reservation changed retention state")
	}
	ring.retain(index, 7, slot)
	if !slot.retained || len(ring.vacant) != len(ring.slots)-1 {
		t.Fatal("retaining a slot returned it prematurely")
	}
	if index, err := ring.stashIndex(8); index != -1 || err != ErrInboundStashCapacity {
		t.Fatalf("full stash: index=%d, error=%v", index, err)
	}
	if ring.take(8) != nil {
		t.Fatal("missing sequence returned a slot")
	}
	if taken := ring.take(7); taken != slot {
		t.Fatal("take did not transfer the retained slot")
	}
	if slot.retained || !bytes.Equal(slot.frame, wire) || slot.parsed.Message() == nil || slot.parsed.message.inbound != slot {
		t.Fatal("take changed retained frame, parsed message, or active ownership")
	}
	if ring.take(7) != nil || len(ring.vacant) != len(ring.slots)-1 {
		t.Fatal("take either retained or prematurely returned the active slot")
	}
	slot.release()
	assertInboundRingVacant(t, ring)
}

func TestInboundRingDuplicateReplacement(t *testing.T) {
	ring, err := newInboundRing(nil, nil, inboundRingTestLimits(), 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range []int{7, 8} {
		slot := inboundRingTestAcquire(t, ring)
		inboundRingTestParse(t, slot, buildFIXT11Wire("35=D\x01", "11=original\x01"))
		index, err := ring.stashIndex(seq)
		if err != nil {
			t.Fatal(err)
		}
		ring.retain(index, seq, slot)
	}
	first := ring.take(7)
	first.release()
	// The vacant first entry must not hide an existing sequence later in stash.
	index, err := ring.stashIndex(8)
	if err != nil || index != 1 {
		t.Fatalf("existing sequence after vacant entry: index=%d, error=%v", index, err)
	}
	replacement := inboundRingTestAcquire(t, ring)
	wire := buildFIXT11Wire("35=D\x01", "11=replacement\x01")
	inboundRingTestParse(t, replacement, wire)
	before := len(ring.vacant)
	ring.retain(index, 8, replacement)
	if len(ring.vacant) != before+1 || !replacement.retained {
		t.Fatal("replacement did not release exactly the previous slot")
	}
	if taken := ring.take(8); taken != replacement || !bytes.Equal(taken.frame, wire) {
		t.Fatal("replacement frame was not retained")
	}
	replacement.release()
	ring.clearStash()
	assertInboundRingVacant(t, ring)
}

func TestInboundRingDiscardAndClear(t *testing.T) {
	ring, err := newInboundRing(nil, nil, inboundRingTestLimits(), 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range []int{1, 3, 5} {
		index, err := ring.stashIndex(seq)
		if err != nil {
			t.Fatal(err)
		}
		ring.retain(index, seq, inboundRingTestAcquire(t, ring))
	}
	ring.discardBefore(3)
	if ring.take(1) != nil || len(ring.vacant) != len(ring.slots)-2 {
		t.Fatal("discardBefore did not release exactly the older sequence")
	}
	equal := ring.take(3)
	if equal == nil {
		t.Fatal("discardBefore removed the equal sequence")
	}
	equal.release()
	ring.clearStash()
	ring.clearStash()
	if ring.take(5) != nil {
		t.Fatal("clearStash retained a sequence")
	}
	assertInboundRingVacant(t, ring)
}

func TestInboundRingStashLeavesReaderAndSessionHeadroom(t *testing.T) {
	const queueDepth, maxStash = 3, 2
	ring, err := newInboundRing(nil, nil, inboundRingTestLimits(), queueDepth, maxStash)
	if err != nil {
		t.Fatal(err)
	}
	for seq := 1; seq <= maxStash; seq++ {
		index, err := ring.stashIndex(seq)
		if err != nil {
			t.Fatal(err)
		}
		ring.retain(index, seq, inboundRingTestAcquire(t, ring))
	}
	if len(ring.vacant) != queueDepth+2 {
		t.Fatal("full stash consumed reader/session headroom")
	}
	queued := make([]*inboundSlot, queueDepth)
	for index := range queued {
		queued[index] = inboundRingTestAcquire(t, ring)
	}
	active := inboundRingTestAcquire(t, ring)
	reader := inboundRingTestAcquire(t, ring)
	if len(ring.vacant) != 0 {
		t.Fatal("unexpected extra slot after filling every ownership position")
	}
	if _, err := ring.stashIndex(3); err != ErrInboundStashCapacity {
		t.Fatal("full stash accepted another retained sequence")
	}
	active.release()
	reused := inboundRingTestAcquire(t, ring)
	if reused != active || reused.retained {
		t.Fatal("reader could not reuse the completed active slot")
	}
	reused.release()
	reader.release()
	for _, slot := range queued {
		slot.release()
	}
	ring.clearStash()
	assertInboundRingVacant(t, ring)
}

func TestInboundRingZeroStashAndSteadyStateAllocations(t *testing.T) {
	withoutStash, err := newInboundRing(nil, nil, inboundRingTestLimits(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if index, err := withoutStash.stashIndex(1); index != -1 || err != ErrInboundStashCapacity {
		t.Fatalf("zero stash: index=%d, error=%v", index, err)
	}
	withoutStash.discardBefore(2)
	withoutStash.clearStash()
	assertInboundRingVacant(t, withoutStash)
	ring, err := newInboundRing(nil, nil, inboundRingTestLimits(), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	wire := buildFIXT11Wire("35=D\x01", "11=value\x01")
	allocations := testing.AllocsPerRun(100, func() {
		slot := <-ring.vacant
		slot.frame = slot.frame[:len(wire)]
		copy(slot.frame, wire)
		if err := slot.parsed.Parse(slot.frame); err != nil {
			t.Fatal(err)
		}
		slot.parsed.message.inbound = slot
		index, err := ring.stashIndex(1)
		if err != nil {
			t.Fatal(err)
		}
		ring.retain(index, 1, slot)
		if ring.take(1) != slot {
			t.Fatal("take lost the retained slot")
		}
		slot.release()
	})
	if allocations != 0 {
		t.Fatalf("slot acquire/parse/retain/take/release allocated %g objects, want 0", allocations)
	}
	assertInboundRingVacant(t, ring)
}
