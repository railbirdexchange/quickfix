package quickfix

import (
	"bytes"
	"io"
	"net"
	"time"
)

// A connection's session owner closes stop exactly once. The reader owns the
// parser and closes both completion channels. No slot is shared across a send.
type boundedReadConnection struct {
	connection net.Conn
	parser     *parser
	ring       *inboundRing
	stop       chan struct{}
	done       chan struct{}
	messageIn  chan fixIn
}

func startBoundedRead(connection net.Conn, previous *parser, inbound *boundedInboundSession, messageIn chan fixIn, first []byte, received time.Time) (*boundedReadConnection, error) {
	if len(first) > inbound.limits.MaxFrameBytes {
		return nil, ErrFrameCapacity
	}
	reader := previous.reader
	if len(previous.buffer) != 0 {
		// Acceptor session discovery may have read ahead. Transfer those bytes
		// during connection setup instead of losing coalesced following frames.
		reader = io.MultiReader(bytes.NewReader(previous.buffer), reader)
	}
	parser, err := newBoundedParser(reader, inbound.limits.MaxFrameBytes)
	if err != nil {
		return nil, err
	}
	control := &boundedReadConnection{connection: connection, parser: parser, ring: inbound.ring,
		stop: make(chan struct{}), done: make(chan struct{}), messageIn: messageIn}
	go control.read(first, received)
	return control, nil
}

func (reader *boundedReadConnection) read(first []byte, received time.Time) {
	defer close(reader.done)
	defer close(reader.messageIn)
	for {
		select {
		case <-reader.stop:
			return
		default:
		}
		var slot *inboundSlot
		select {
		case <-reader.stop:
			return
		case slot = <-reader.ring.vacant:
		}
		if first != nil {
			slot.frame = slot.frame[:len(first)]
			copy(slot.frame, first)
			first = nil
		} else {
			frame, err := reader.parser.ReadMessageInto(slot.frame)
			if err != nil {
				slot.release()
				select {
				case <-reader.stop:
				case reader.messageIn <- fixIn{readError: err}:
				}
				return
			}
			slot.frame = frame
			received = reader.parser.lastRead
		}
		select {
		case reader.messageIn <- fixIn{slot: slot, receiveTime: received}:
		case <-reader.stop:
			slot.release()
			return
		}
	}
}

func (reader *boundedReadConnection) stopAndDrain(messageIn <-chan fixIn) {
	close(reader.stop)
	// Closing the cancellation channel also covers a reader blocked on a
	// vacant slot or a full inbound channel; a read deadline interrupts Read.
	if err := reader.connection.SetReadDeadline(time.Now()); err != nil {
		// A connection unable to interrupt reads cannot safely retain its ring.
		_ = reader.connection.Close()
	}
	for message := range messageIn {
		if message.slot != nil {
			message.slot.release()
		}
	}
	<-reader.done
}
