package quickfix

import (
	"bytes"
	"errors"
	"slices"
	"strconv"
	"strings"
)

// ErrBufferTooSmall indicates that the caller-owned output has insufficient
// remaining capacity. Append operations return the original slice unchanged.
var ErrBufferTooSmall = errors.New("quickfix: output buffer capacity exceeded")

// AppendTo appends the current message fields as FIX wire bytes, calculating
// BodyLength and CheckSum directly in dst. It does not use cached raw bytes or
// update the stored BodyLength and CheckSum fields.
//
// The caller must exclusively own the message and dst for the duration of the
// call. dst must not overlap any message field storage. The caller owns the
// returned bytes and may transfer them to a writer until its write completes.
// Insufficient capacity returns ErrBufferTooSmall without changing dst or the
// message; this method never grows the output buffer. Successful encoding may
// reorder the message's existing tag indexes in place.
func (message *Message) AppendTo(dst []byte) ([]byte, error) {
	return message.appendTo(dst, nil, false)
}

// AppendWithBodyBytesTo is AppendTo with an explicit serialized body. Resend
// processing can use it to preserve the original repeating-group field order.
// bodyBytes must not overlap dst and must remain unchanged until this call
// returns. The message body FieldMap is not used or changed.
func (message *Message) AppendWithBodyBytesTo(dst, bodyBytes []byte) ([]byte, error) {
	return message.appendTo(dst, bodyBytes, true)
}

func (message *Message) appendTo(dst, bodyBytes []byte, useBodyBytes bool) ([]byte, error) {
	if err := message.constructionError(); err != nil {
		return dst, err
	}
	// Reserve the minimum 9=0<SOH> and 10=000<SOH> first, then count
	// against the remaining capacity. Never sum unchecked wire lengths:
	// repeated tag indexes can describe more bytes than fit in an int.
	remaining := cap(dst) - len(dst)
	if remaining < 11 {
		return dst, ErrBufferTooSmall
	}
	remaining -= 11
	beginString, _ := message.Header.lookup(tagBeginString)
	bodyLength := 0
	if arena := message.arena; !useBodyBytes && arena != nil && arena.owner == message && arena.intact() {
		// Ordinary reusable messages maintain exact encoded lengths during
		// mutation. Only the original, intact owner may use those totals;
		// replacement sections and explicit resend bodies use the field walk.
		if arena.beginLength > remaining {
			return dst, ErrBufferTooSmall
		}
		remaining -= arena.beginLength
		bodyLength = arena.bodyLength
		if bodyLength > remaining {
			return dst, ErrBufferTooSmall
		}
		remaining -= bodyLength
	} else {
		for _, value := range beginString {
			if len(value.bytes) > remaining {
				return dst, ErrBufferTooSmall
			}
			remaining -= len(value.bytes)
		}
		headerLength, fits := message.Header.lengthWithin(tagBeginString, tagBodyLength, remaining)
		if !fits {
			return dst, ErrBufferTooSmall
		}
		remaining -= headerLength
		bodyLength = len(bodyBytes)
		if !useBodyBytes {
			bodyLength, fits = message.Body.lengthWithin(0, 0, remaining)
		}
		if !fits || bodyLength > remaining {
			return dst, ErrBufferTooSmall
		}
		remaining -= bodyLength
		trailerLength, fits := message.Trailer.lengthWithin(tagCheckSum, tagCheckSum, remaining)
		if !fits {
			return dst, ErrBufferTooSmall
		}
		remaining -= trailerLength
		bodyLength += headerLength + trailerLength
	}
	if intTextLength(bodyLength)-1 > remaining {
		return dst, ErrBufferTooSmall
	}

	start := len(dst)
	for _, value := range beginString {
		dst = append(dst, value.bytes...)
	}
	dst = append(dst, '9', '=')
	dst = strconv.AppendInt(dst, int64(bodyLength), 10)
	dst = append(dst, '\x01')
	dst = message.Header.appendOwned(dst, tagBeginString, tagBodyLength)
	if useBodyBytes {
		dst = append(dst, bodyBytes...)
	} else {
		dst = message.Body.appendOwned(dst, 0, 0)
	}
	dst = message.Trailer.appendOwned(dst, tagCheckSum, tagCheckSum)
	var checksum byte
	for _, value := range dst[start:] {
		checksum += value
	}
	return append(dst, '1', '0', '=', byte(checksum/100)+'0', byte(checksum/10%10)+'0', byte(checksum%10)+'0', '\x01'), nil
}

// lengthExcept and appendOwned require exclusive ownership, supplied by the
// public append contract or the existing FieldMap locks in compatibility APIs.
func (fields *FieldMap) lengthExcept(first, second Tag) int {
	length, fits := fields.lengthWithin(first, second, int(^uint(0)>>1))
	if !fits {
		panic(bytes.ErrTooLarge)
	}
	return length
}

func (fields *FieldMap) lengthWithin(first, second Tag, limit int) (int, bool) {
	length := 0
	for _, tag := range fields.fieldTags() {
		if first != 0 && tag == first || second != 0 && tag == second {
			continue
		}
		values, _ := fields.lookup(tag)
		for _, value := range values {
			if len(value.bytes) > limit-length {
				return 0, false
			}
			length += len(value.bytes)
		}
	}
	return length, true
}

func (fields *FieldMap) sortedTagsOwned() []Tag {
	tags := fields.fieldTags()
	slices.SortFunc(tags, func(left, right Tag) int {
		if fields.compare(left, right) {
			return -1
		}
		if fields.compare(right, left) {
			return 1
		}
		return 0
	})
	return tags
}

func (fields *FieldMap) appendOwned(dst []byte, first, second Tag) []byte {
	for _, tag := range fields.sortedTagsOwned() {
		if first != 0 && tag == first || second != 0 && tag == second {
			continue
		}
		values, _ := fields.lookup(tag)
		for _, value := range values {
			dst = append(dst, value.bytes...)
		}
	}
	return dst
}

func (fields *FieldMap) serializedLength() int {
	fields.rlock()
	defer fields.runlock()
	return fields.lengthExcept(0, 0)
}

func (fields *FieldMap) appendLegacy(dst []byte) []byte {
	fields.lock()
	defer fields.unlock()
	return fields.appendOwned(dst, 0, 0)
}

func (fields *FieldMap) writeString(dst *strings.Builder) {
	fields.lock()
	defer fields.unlock()
	for _, tag := range fields.sortedTagsOwned() {
		values, _ := fields.lookup(tag)
		for _, value := range values {
			dst.Write(value.bytes)
		}
	}
}

func intTextLength(value int) int {
	length := 1
	magnitude := uint(value)
	if value < 0 {
		length++
		magnitude = 0 - magnitude
	}
	for magnitude >= 10 {
		magnitude /= 10
		length++
	}
	return length
}
