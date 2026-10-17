package quickfix

import (
	"errors"
	"io"
	"net"
	"sync/atomic"
	"time"
)

var (
	ErrInvalidOutboundLimits = errors.New("quickfix: invalid outbound frame limits")
	ErrOutboundCapacity      = errors.New("quickfix: outbound frame capacity exhausted")
	ErrOutboundResetPending  = errors.New("quickfix: a logon reset is pending")
)

// outboundSlot has exactly one owner: a synchronous sender, the pending ring,
// the socket writer, or the vacant channel. Only encoded bytes cross to the
// writer; the application continues to own its original Message.
type outboundSlot struct {
	wire []byte
	ring *outboundRing
}

func (slot *outboundSlot) release() {
	slot.wire = slot.wire[:0]
	slot.ring.vacant <- slot
}

// outboundRing's pending queue is protected by the session's existing
// sendMutex. Its vacant channel transfers sole ownership from writer to sender
// without requiring the session event loop to reclaim completed writes.
type outboundRing struct {
	slots                 []outboundSlot
	vacant                chan *outboundSlot
	pending               []*outboundSlot
	head                  int
	count                 int
	journal               preparedMessageStore
	borrowedStore         BorrowedMessageStore
	log                   BorrowedLog
	replay                *MessageSlot
	writeTimeout          time.Duration
	resetPending          atomic.Bool
	scheduledResetPending atomic.Bool
}

func newOutboundRing(queueDepth, maxFrameBytes int) (*outboundRing, error) {
	maximum := int(^uint(0) >> 1)
	if queueDepth <= 0 || maxFrameBytes <= 0 || queueDepth >= maximum/1024 || maxFrameBytes > maximum/1024 || queueDepth+1 > maximum/maxFrameBytes {
		return nil, ErrInvalidOutboundLimits
	}
	count := queueDepth + 1
	ring := &outboundRing{
		slots: make([]outboundSlot, count), vacant: make(chan *outboundSlot, count),
		pending: make([]*outboundSlot, queueDepth),
	}
	arena := make([]byte, count*maxFrameBytes)
	for index := range ring.slots {
		start, end := index*maxFrameBytes, (index+1)*maxFrameBytes
		slot := &ring.slots[index]
		slot.wire, slot.ring = arena[start:start:end], ring
		ring.vacant <- slot
	}
	return ring, nil
}

// reserve must precede sequence assignment and persistence. Replacement may
// use the progress slot when the pending queue is full; existing pending bytes
// remain untouched until the replacement has encoded and persisted.
func (ring *outboundRing) reserve(replacement bool) (*outboundSlot, error) {
	if !replacement && ring.count == len(ring.pending) {
		return nil, ErrOutboundCapacity
	}
	select {
	case slot := <-ring.vacant:
		return slot, nil
	default:
		return nil, ErrOutboundCapacity
	}
}

func (ring *outboundRing) enqueue(slot *outboundSlot) {
	if ring.count == len(ring.pending) {
		panic("quickfix: outbound slot enqueued without pending reservation")
	}
	index := (ring.head + ring.count) % len(ring.pending)
	ring.pending[index] = slot
	ring.count++
}

func (ring *outboundRing) front() *outboundSlot {
	if ring.count == 0 {
		return nil
	}
	return ring.pending[ring.head]
}

func (ring *outboundRing) pop() *outboundSlot {
	slot := ring.front()
	if slot == nil {
		return nil
	}
	ring.pending[ring.head] = nil
	ring.head = (ring.head + 1) % len(ring.pending)
	ring.count--
	return slot
}

func (ring *outboundRing) dropPending() {
	for ring.count != 0 {
		ring.pop().release()
	}
}

// outboundWriter is connection-scoped. The sole sender closes ready while
// holding sendMutex, then finish joins this writer before slots can be reused
// by a replacement connection. A write failure leaves journal ownership alone.
type outboundWriter struct {
	connection net.Conn
	ready      chan *outboundSlot
	permits    chan struct{}
	done       chan struct{}
	wake       chan bool
	timeout    time.Duration
	failed     chan struct{}
	err        error // Published by failed or by closing done; never changed afterward.
}

func newOutboundWriter(connection net.Conn, queueDepth int, timeout time.Duration, wake chan bool) *outboundWriter {
	writer := &outboundWriter{
		connection: connection, ready: make(chan *outboundSlot, queueDepth),
		permits: make(chan struct{}, max(queueDepth, 1)), done: make(chan struct{}), failed: make(chan struct{}), wake: wake, timeout: timeout,
	}
	for count := 0; count < queueDepth; count++ {
		writer.permits <- struct{}{}
	}
	return writer
}

func (writer *outboundWriter) run() {
	defer close(writer.done)
	// The reader may have closed the connection during normal teardown.
	// Preserve the actual write result; a duplicate Close is not a failed write.
	defer writer.connection.Close()
	for {
		if cap(writer.ready) == 0 {
			// Preserve an unbuffered SocketOutboundBufferSize without logging
			// after ownership transfer: advertise the waiting receiver first.
			writer.permits <- struct{}{}
			writer.notify()
		}
		slot, open := <-writer.ready
		if !open {
			return
		}
		if cap(writer.ready) != 0 {
			writer.permits <- struct{}{}
		}
		err := writer.write(slot.wire)
		if err != nil {
			writer.err = err
			// Publish failure before returning ownership. A waiting sender must
			// not use this slot to commit another message after a failed write.
			close(writer.failed)
		}
		slot.release()
		// This notification occurs after both the completed write and the
		// ownership return. It also lets the session drain a pending queue.
		writer.notify()
		if err != nil {
			return
		}
	}
}

func (writer *outboundWriter) notify() {
	select {
	case writer.wake <- true:
	default:
	}
}

// failure is read only after failed is observed or done closes.
func (writer *outboundWriter) failure() error {
	if writer.err != nil {
		return writer.err
	}
	return io.ErrClosedPipe
}

func (writer *outboundWriter) write(wire []byte) error {
	if writer.timeout > 0 {
		if err := writer.connection.SetWriteDeadline(time.Now().Add(writer.timeout)); err != nil {
			return err
		}
	}
	for len(wire) != 0 {
		written, err := writer.connection.Write(wire)
		if written < 0 || written > len(wire) {
			return io.ErrShortWrite
		}
		wire = wire[written:]
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

// finish is called once by the sender, after it stops publishing new slots.
// A successful writer drains all queued writes; after an error, finish returns
// the remaining queued slots without writing them. The journal retains them.
func (writer *outboundWriter) finish() error {
	close(writer.ready)
	<-writer.done
	for slot := range writer.ready {
		slot.release()
	}
	return writer.err
}
