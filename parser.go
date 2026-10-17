// Copyright (c) quickfixengine.org  All rights reserved.
//
// This file may be distributed under the terms of the quickfixengine.org
// license as defined by quickfixengine.org and appearing in the file
// LICENSE included in the packaging of this file.
//
// This file is provided AS IS with NO WARRANTY OF ANY KIND, INCLUDING
// THE WARRANTY OF DESIGN, MERCHANTABILITY AND FITNESS FOR A
// PARTICULAR PURPOSE.
//
// See http://www.quickfixengine.org/LICENSE for licensing information.
//
// Contact ask@quickfixengine.org if any conditions of this licensing
// are not clear to you.

package quickfix

import (
	"bytes"
	"errors"
	"io"
	"time"
)

const (
	defaultBufSize = 4096
)

var (
	errInvalidBoundedParser = errors.New("quickfix: bounded parser requires a reader and positive frame capacity")
	errParserNotBounded     = errors.New("quickfix: ReadMessageInto requires a bounded parser")
	errInvalidFrameLength   = errors.New("quickfix: invalid framed BodyLength")
	errInvalidFrameChecksum = errors.New("quickfix: checksum is not at the declared frame boundary")
)

type parser struct {
	// Buffer is a slice of bigBuffer.
	bigBuffer, buffer []byte
	reader            io.Reader
	lastRead          time.Time
	maxFrameBytes     int
	pendingReadErr    error
}

func newParser(reader io.Reader) *parser {
	return &parser{reader: reader}
}

// newBoundedParser allocates fixed framing scratch once. Its maximum frame size
// includes BeginString, BodyLength, the message body, and CheckSum. Frames equal
// to that size fit; larger frames return ErrFrameCapacity without growing it.
func newBoundedParser(reader io.Reader, maxFrameBytes int) (*parser, error) {
	if reader == nil || maxFrameBytes <= 0 {
		return nil, errInvalidBoundedParser
	}
	scratch := make([]byte, maxFrameBytes)
	return &parser{reader: reader, bigBuffer: scratch, buffer: scratch[:0], maxFrameBytes: maxFrameBytes}, nil
}

func (p *parser) readMore() (int, error) {
	if p.maxFrameBytes > 0 {
		return p.readMoreBounded()
	}
	if len(p.buffer) == cap(p.buffer) {
		var newBuffer []byte
		switch {
		// Initialize the parser.
		case len(p.bigBuffer) == 0:
			p.bigBuffer = make([]byte, defaultBufSize)
			newBuffer = p.bigBuffer[0:0]

		// Shift buffer back to the start of bigBuffer.
		case 2*len(p.buffer) <= len(p.bigBuffer):
			newBuffer = p.bigBuffer[0:len(p.buffer)]

		// Reallocate big buffer with enough space to shift buffer.
		default:
			p.bigBuffer = make([]byte, 2*len(p.buffer))
			newBuffer = p.bigBuffer[0:len(p.buffer)]
		}

		copy(newBuffer, p.buffer)
		p.buffer = newBuffer
	}

	n, e := p.reader.Read(p.buffer[len(p.buffer):cap(p.buffer)])
	p.lastRead = time.Now()
	p.buffer = p.buffer[:len(p.buffer)+n]
	return n, e
}

func (p *parser) readMoreBounded() (int, error) {
	if p.pendingReadErr != nil {
		return 0, p.pendingReadErr
	}
	if len(p.buffer) == cap(p.buffer) {
		if len(p.buffer) == len(p.bigBuffer) {
			return 0, ErrFrameCapacity
		}
		copy(p.bigBuffer, p.buffer)
		p.buffer = p.bigBuffer[:len(p.buffer)]
	}
	// Readers are discouraged from returning (0, nil). Bound retries so such
	// a reader cannot spin forever without producing a frame or an error.
	for emptyReads := 0; emptyReads < 100; emptyReads++ {
		count, err := p.reader.Read(p.buffer[len(p.buffer):cap(p.buffer)])
		p.lastRead = time.Now()
		if err != nil {
			p.pendingReadErr = err
		}
		p.buffer = p.buffer[:len(p.buffer)+count]
		if count > 0 || err != nil {
			return count, err
		}
	}
	return 0, io.ErrNoProgress
}

func (p *parser) boundedReadError(err error) error {
	if err == io.EOF && len(p.buffer) > 0 {
		return io.ErrUnexpectedEOF
	}
	return err
}

func (p *parser) requireBoundedBytes(length int) error {
	if length > p.maxFrameBytes {
		return ErrFrameCapacity
	}
	for len(p.buffer) < length {
		if count, err := p.readMoreBounded(); count == 0 && err != nil {
			return p.boundedReadError(err)
		}
	}
	return nil
}

// ReadMessageInto fills dst from index zero, disregarding its initial length.
// It requires newBoundedParser and never grows either dst or framing scratch.
// A complete frame whose size equals cap(dst) fits. On a capacity or framing
// error it returns dst[:0] and leaves destination bytes unchanged. In particular,
// a short destination does not consume the frame: retry with sufficient space.
//
// The caller exclusively owns dst, which must not alias parser scratch. Returned
// bytes belong to the caller and remain valid across later reads. This bounded
// path requires CheckSum exactly at BodyLength's declared boundary; the legacy
// ReadMessage method retains its existing framing behavior.
func (p *parser) ReadMessageInto(dst []byte) ([]byte, error) {
	dst = dst[:0]
	if p.maxFrameBytes <= 0 {
		return dst, errParserNotBounded
	}
	start, err := p.findStart()
	if err != nil {
		return dst, p.boundedReadError(err)
	}
	p.buffer = p.buffer[start:]
	beginEnd, err := p.findIndex([]byte{'\x01'})
	if err != nil {
		return dst, p.boundedReadError(err)
	}
	// Reserve the two-byte tag and its separator before indexing. The search
	// offset remains relative to buffer even when readMoreBounded compacts it.
	if beginEnd > p.maxFrameBytes-3 {
		return dst, ErrFrameCapacity
	}
	if err := p.requireBoundedBytes(beginEnd + 3); err != nil {
		return dst, err
	}
	if p.buffer[beginEnd+1] != '9' || p.buffer[beginEnd+2] != '=' {
		return dst, errInvalidFrameLength
	}
	lengthStart := beginEnd + 3
	lengthEnd, err := p.findIndexAfterOffset(lengthStart, []byte{'\x01'})
	if err != nil {
		return dst, p.boundedReadError(err)
	}
	bodyStart := lengthEnd + 1
	if bodyStart > p.maxFrameBytes-7 {
		return dst, ErrFrameCapacity
	}
	bodyLength, err := boundedUnsigned(p.buffer[lengthStart:lengthEnd], p.maxFrameBytes-bodyStart-7, ErrFrameCapacity)
	if err != nil {
		if err == ErrFrameCapacity {
			return dst, err
		}
		return dst, errInvalidFrameLength
	}
	if bodyLength == 0 {
		return dst, errInvalidFrameLength
	}
	checksumStart := bodyStart + bodyLength
	frameLength := checksumStart + 7
	if cap(dst) < frameLength {
		return dst, ErrFrameCapacity
	}
	if err := p.requireBoundedBytes(frameLength); err != nil {
		return dst, err
	}
	if p.buffer[checksumStart-1] != '\x01' ||
		p.buffer[checksumStart] != '1' || p.buffer[checksumStart+1] != '0' || p.buffer[checksumStart+2] != '=' ||
		p.buffer[checksumStart+3] < '0' || p.buffer[checksumStart+3] > '9' ||
		p.buffer[checksumStart+4] < '0' || p.buffer[checksumStart+4] > '9' ||
		p.buffer[checksumStart+5] < '0' || p.buffer[checksumStart+5] > '9' || p.buffer[checksumStart+6] != '\x01' {
		return dst, errInvalidFrameChecksum
	}
	dst = dst[:frameLength]
	copy(dst, p.buffer[:frameLength])
	p.buffer = p.buffer[frameLength:]
	return dst, nil
}

func (p *parser) findIndex(delim []byte) (int, error) {
	return p.findIndexAfterOffset(0, delim)
}

func (p *parser) findIndexAfterOffset(offset int, delim []byte) (int, error) {
	for {
		if offset > len(p.buffer) {
			if n, err := p.readMore(); n == 0 && err != nil {
				return -1, err
			}

			continue
		}

		if index := bytes.Index(p.buffer[offset:], delim); index != -1 {
			return index + offset, nil
		}

		n, err := p.readMore()

		if n == 0 && err != nil {
			return -1, err
		}
	}
}

func (p *parser) findStart() (int, error) {
	return p.findIndex([]byte("8="))
}

func (p *parser) findEndAfterOffset(offset int) (int, error) {
	index, err := p.findIndexAfterOffset(offset, []byte("\00110="))
	if err != nil {
		return index, err
	}

	index, err = p.findIndexAfterOffset(index+1, []byte("\001"))
	if err != nil {
		return index, err
	}

	return index + 1, nil
}

func (p *parser) jumpLength() (int, error) {
	lengthIndex, err := p.findIndex([]byte("9="))
	if err != nil {
		return 0, err
	}

	lengthIndex += 3

	offset, err := p.findIndexAfterOffset(lengthIndex, []byte("\001"))
	if err != nil {
		return 0, err
	}

	if offset == lengthIndex {
		return 0, errors.New("No length given")
	}

	length, err := atoi(p.buffer[lengthIndex:offset])
	if err != nil {
		return length, err
	}

	if length <= 0 {
		return length, errors.New("Invalid length")
	}

	return offset + length, nil
}

func (p *parser) ReadMessage() (msgBytes *bytes.Buffer, err error) {
	start, err := p.findStart()
	if err != nil {
		return
	}
	p.buffer = p.buffer[start:]

	index, err := p.jumpLength()
	if err != nil {
		return
	}

	index, err = p.findEndAfterOffset(index)
	if err != nil {
		return
	}

	msgBytes = new(bytes.Buffer)
	msgBytes.Reset()
	msgBytes.Write(p.buffer[:index])
	p.buffer = p.buffer[index:]

	return
}
