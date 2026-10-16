package quickfix

import (
	"bytes"
	"strconv"
	"time"
)

const (
	outboundSequence = iota
	outboundTimestamp
	outboundLastProcessed
	outboundBodyLength
	outboundChecksum
	outboundOriginalTimestamp
	outboundPossDup
	outboundFieldCount
)

// Engine scalar storage belongs to a reused caller Message or session parse
// slot and is never transferred to the socket writer. A fresh ordinary Message
// uses small owned scalar values until a second send establishes its scope.
type outboundFields struct {
	owner   *Message
	buffers [outboundFieldCount][80]byte
	values  [outboundFieldCount][80]byte
}

func (message *Message) hasOutboundScope() bool {
	return message.arena != nil || message.parseSlot != nil ||
		message.outboundFields != nil && message.outboundFields.owner == message
}

func (message *Message) outboundFieldBuffer(index int) []byte {
	if message.outboundFields == nil || message.outboundFields.owner != message {
		message.outboundFields = &outboundFields{owner: message}
	}
	return message.outboundFields.buffers[index][:0]
}

func (message *Message) setOutboundField(fields *FieldMap, index int, tag Tag, wire []byte, valueOffset int) {
	if arena := fields.mutableArena(); arena != nil {
		// Engine buffers are disjoint from the arena, including its value plane.
		// Account the field before later body/header mutations move those planes.
		arena.setScalar(fields, tag, wire[valueOffset:len(wire)-1], "", nil, true)
		return
	}
	// Legacy scalar setters keep the getter value separate from encoded wire.
	// Mutating GetBytes in a callback must not silently rewrite the encoding.
	valueBytes := message.outboundFields.values[index][:len(wire)-valueOffset-1]
	copy(valueBytes, wire[valueOffset:len(wire)-1])
	value := fields.getOrCreate(tag)
	value[0] = TagValue{tag: tag, bytes: wire, value: valueBytes[:len(valueBytes):len(valueBytes)]}
	fields.reusable = true
}

// Cold engine scalars own one exact allocation with separate wire and getter
// regions. Replacing a field never overwrites a caller's retained old getter.
func setColdOutboundField(fields *FieldMap, tag Tag, value []byte) {
	arena := fields.mutableArena()
	if fields.constructionErr != nil {
		return
	}
	if arena != nil {
		arena.setScalar(fields, tag, value, "", nil, true)
		return
	}
	var prefix [24]byte
	tagBytes := strconv.AppendInt(prefix[:0], int64(tag), 10)
	wireLength := len(tagBytes) + len(value) + 2
	storage := make([]byte, wireLength+len(value))
	wire := storage[:wireLength:wireLength]
	copy(wire, tagBytes)
	wire[len(tagBytes)] = '='
	copy(wire[len(tagBytes)+1:], value)
	wire[wireLength-1] = '\x01'
	ownedValue := storage[wireLength:len(storage):len(storage)]
	copy(ownedValue, value)
	field := fields.getOrCreate(tag)
	field[0] = TagValue{tag: tag, bytes: wire, value: ownedValue}
}

func (message *Message) setOutboundInt(fields *FieldMap, index int, tag Tag, value int) {
	if !message.hasOutboundScope() {
		var scratch [24]byte
		setColdOutboundField(fields, tag, strconv.AppendInt(scratch[:0], int64(value), 10))
		return
	}
	wire := strconv.AppendInt(message.outboundFieldBuffer(index), int64(tag), 10)
	wire = append(wire, '=')
	valueOffset := len(wire)
	wire = strconv.AppendInt(wire, int64(value), 10)
	wire = append(wire, '\x01')
	message.setOutboundField(fields, index, tag, wire, valueOffset)
}

func (message *Message) setOutboundBool(fields *FieldMap, index int, tag Tag, value bool) {
	if !message.hasOutboundScope() {
		encoded := [1]byte{'N'}
		if value {
			encoded[0] = 'Y'
		}
		setColdOutboundField(fields, tag, encoded[:])
		return
	}
	wire := strconv.AppendInt(message.outboundFieldBuffer(index), int64(tag), 10)
	wire = append(wire, '=')
	valueOffset := len(wire)
	encoded := byte('N')
	if value {
		encoded = 'Y'
	}
	wire = append(wire, encoded, '\x01')
	message.setOutboundField(fields, index, tag, wire, valueOffset)
}

func (message *Message) setOutboundBytes(fields *FieldMap, index int, tag Tag, value []byte) {
	if !message.hasOutboundScope() {
		// Internal values may borrow an input frame or the writer's output
		// slot. Both retained regions must belong to the caller's Message.
		setColdOutboundField(fields, tag, value)
		return
	}
	wire := strconv.AppendInt(message.outboundFieldBuffer(index), int64(tag), 10)
	wire = append(wire, '=')
	valueOffset := len(wire)
	if len(value) > cap(wire)-len(wire)-1 {
		// Nonstandard scalar values retain the existing allocating setter
		// behavior rather than growing a fixed engine field buffer.
		fields.SetBytes(tag, value)
		return
	}
	wire = append(wire, value...)
	wire = append(wire, '\x01')
	message.setOutboundField(fields, index, tag, wire, valueOffset)
}

func (message *Message) setOutboundTime(value time.Time, precision TimestampPrecision) {
	if !message.hasOutboundScope() {
		timestamp := FIXUTCTimestamp{Time: value, Precision: precision}
		var scratch [64]byte
		encoded, err := timestamp.AppendTo(scratch[:0])
		if err != nil {
			message.Header.SetField(tagSendingTime, timestamp)
			return
		}
		setColdOutboundField(&message.Header.FieldMap, tagSendingTime, encoded)
		return
	}
	wire := append(message.outboundFieldBuffer(outboundTimestamp), '5', '2', '=')
	timestamp := FIXUTCTimestamp{Time: value, Precision: precision}
	wire, err := timestamp.AppendTo(wire)
	if err != nil {
		// Current wall-clock timestamps fit the fixed buffer; preserve the
		// convenience behavior even for a platform clock outside that bound.
		message.Header.SetField(tagSendingTime, timestamp)
		return
	}
	wire = append(wire, '\x01')
	message.setOutboundField(&message.Header.FieldMap, outboundTimestamp, tagSendingTime, wire, 3)
}

// A caller may mutate GetBytes while TagValue.bytes remains a separately
// encoded old value. Both representations must match before skipping a setter.
func setCanonicalIdentity(fields *FieldMap, tag Tag, identity string) {
	fields.rlock()
	values, present := fields.lookup(tag)
	matched := false
	if present && len(values) == 1 && values[0].tag == tag && string(values[0].value) == identity {
		var prefix [24]byte
		encodedPrefix := strconv.AppendInt(prefix[:0], int64(tag), 10)
		encodedPrefix = append(encodedPrefix, '=')
		encoded := values[0].bytes
		matched = len(encoded) == len(encodedPrefix)+len(identity)+1 &&
			bytes.Equal(encoded[:len(encodedPrefix)], encodedPrefix) &&
			string(encoded[len(encodedPrefix):len(encoded)-1]) == identity && encoded[len(encoded)-1] == '\x01'
	}
	fields.runlock()
	if !matched {
		fields.SetString(tag, identity)
	}
}

func (message *Message) retainOutboundFraming(wire []byte) {
	// AppendTo emitted tag9 immediately after every BeginString TagValue.
	// Use that exact length rather than searching potentially unusual values.
	beginString, _ := message.Header.lookup(tagBeginString)
	offset := 2
	for _, value := range beginString {
		offset += len(value.bytes)
	}
	end := offset + bytes.IndexByte(wire[offset:], '\x01')
	message.setOutboundBytes(&message.Header.FieldMap, outboundBodyLength, tagBodyLength, wire[offset:end])
	message.setOutboundBytes(&message.Trailer.FieldMap, outboundChecksum, tagCheckSum, wire[len(wire)-4:len(wire)-1])
}
