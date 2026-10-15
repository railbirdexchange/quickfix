package quickfix

import (
	"bytes"
	"errors"
	"strconv"
	"time"
)

var (
	ErrInvalidOutboundMessageLimits = errors.New("quickfix: invalid outbound message limits")
	ErrOutboundBuilderCapacity      = errors.New("quickfix: outbound builder byte capacity exhausted")
	ErrInvalidOutboundField         = errors.New("quickfix: invalid outbound field or group encoding")
	ErrInvalidOutboundSource        = errors.New("quickfix: invalid or aliased outbound copy source")
	ErrOutboundMessageInactive      = errors.New("quickfix: outbound message is not owned by the caller")
)

// OutboundMessage owns fixed field indexes, TagValue storage, and serialized
// field bytes. It has one owner and is not safe for concurrent use. Construct it
// before the measured path; mutations, Reset, and AppendTo do not grow storage.
//
// maxFields and maxBuilderBytes are mutation budgets between resets. Replacing
// a field consumes new metadata and bytes; Remove does not reclaim either.
// These budgets are separate from the final encoded frame's output capacity.
// The first mutation error is sticky until Reset, allowing a sender to reject a
// partially prepared message before assigning sequence numbers or persisting it.
type OutboundMessage struct {
	message    Message
	header     OutboundFields
	body       OutboundFields
	trailer    OutboundFields
	fields     []TagValue
	usedFields int
	arena      []byte
	err        error
	owner      *boundedOutboundSession
	active     bool
}

// OutboundFields mutates one section of its owning OutboundMessage. Every
// setter copies its input; no caller bytes are retained. On failure, all message
// storage remains unchanged apart from the owner's sticky Err value.
type OutboundFields struct {
	message *OutboundMessage
	fields  *FieldMap
}

// OutboundField describes one serialized field in a caller-supplied group.
// Values may contain SOH, including length-delimited DATA or XML fields.
type OutboundField struct {
	Tag   Tag
	Value []byte
}

// NewOutboundMessage allocates all builder storage and returns an active,
// standalone message. Both capacities must be positive. maxFields counts every
// scalar mutation and every field in each group, across all three sections.
func NewOutboundMessage(maxFields, maxBuilderBytes int) (*OutboundMessage, error) {
	maximum := int(^uint(0) >> 1)
	if maxFields <= 0 || maxFields > maximum/1024 || maxBuilderBytes <= 0 || maxBuilderBytes > maximum/2 {
		return nil, ErrInvalidOutboundMessageLimits
	}
	message := &OutboundMessage{fields: make([]TagValue, maxFields), arena: make([]byte, 0, maxBuilderBytes), active: true}
	message.message.Header.initFixed(headerFieldOrdering, maxFields)
	message.message.Body.initFixed(normalFieldOrder, maxFields)
	message.message.Trailer.initFixed(trailerFieldOrdering, maxFields)
	message.header = OutboundFields{message: message, fields: &message.message.Header.FieldMap}
	message.body = OutboundFields{message: message, fields: &message.message.Body.FieldMap}
	message.trailer = OutboundFields{message: message, fields: &message.message.Trailer.FieldMap}
	return message, nil
}

// Message returns a read-only field/append view. Its byte views expire on the
// next mutation, Reset, or ownership transfer. Do not use legacy setters through
// this view. Like a legacy constructed Message, it has no parsed fields list;
// dictionary validation requiring parsed wire order must use an encoded parse.
func (message *OutboundMessage) Message() *Message { return &message.message }

// Header returns the builder's header mutators.
func (message *OutboundMessage) Header() *OutboundFields { return &message.header }

// Body returns the builder's body mutators.
func (message *OutboundMessage) Body() *OutboundFields { return &message.body }

// Trailer returns the builder's trailer mutators.
func (message *OutboundMessage) Trailer() *OutboundFields { return &message.trailer }

// Err returns the first failed mutation since Reset.
func (message *OutboundMessage) Err() error { return message.err }

// Reset releases all field views and restores both mutation budgets. It does
// not change session ownership or activate a builder returned to its owner.
func (message *OutboundMessage) Reset() {
	message.message.Header.fixedClear()
	message.message.Body.fixedClear()
	message.message.Trailer.fixedClear()
	clear(message.fields[:message.usedFields])
	message.usedFields = 0
	message.arena = message.arena[:0]
	message.err = nil
	message.message.ReceiveTime = time.Time{}
	message.message.rawMessage = nil
	message.message.bodyBytes = nil
	message.message.fields = nil
	message.message.groupStack = nil
	message.message.inbound = nil
}

func (message *OutboundMessage) fail(err error) error {
	if message.err == nil {
		message.err = err
	}
	return message.err
}

func (message *OutboundMessage) writable() error {
	if message.err != nil {
		return message.err
	}
	if !message.active {
		return message.fail(ErrOutboundMessageInactive)
	}
	return nil
}

func outboundFieldLength(tag Tag, valueLength, remaining int) (int, error) {
	if tag <= 0 {
		return 0, ErrInvalidOutboundField
	}
	header := intTextLength(int(tag)) + 2 // '=' and final SOH.
	if header > remaining || valueLength > remaining-header {
		return 0, ErrOutboundBuilderCapacity
	}
	return header + valueLength, nil
}

func (fields *OutboundFields) reserveScalar(tag Tag, valueLength int) error {
	message := fields.message
	if err := message.writable(); err != nil {
		return err
	}
	if message.usedFields == len(message.fields) {
		return message.fail(ErrFieldCapacity)
	}
	_, err := outboundFieldLength(tag, valueLength, cap(message.arena)-len(message.arena))
	if err != nil {
		return message.fail(err)
	}
	return nil
}

// writeField runs only after a complete capacity preflight. The supplied value
// is either bytes or a string, so string setters do not construct byte slices.
func (message *OutboundMessage) writeField(tag Tag, value []byte, text string, useText bool) {
	start := len(message.arena)
	message.arena = strconv.AppendInt(message.arena, int64(tag), 10)
	message.arena = append(message.arena, '=')
	valueStart := len(message.arena)
	if useText {
		message.arena = append(message.arena, text...)
	} else {
		message.arena = append(message.arena, value...)
	}
	valueEnd := len(message.arena)
	message.arena = append(message.arena, '\x01')
	end := len(message.arena)
	message.fields[message.usedFields] = TagValue{tag: tag, value: message.arena[valueStart:valueEnd:valueEnd], bytes: message.arena[start:end:end]}
	message.usedFields++
}

func (fields *OutboundFields) publish(tag Tag, first int) {
	message := fields.message
	// Each new map key requires at least one newly reserved TagValue. Each map
	// has the whole metadata budget, so a successful preflight guarantees room.
	if !fields.fields.put(tag, message.fields[first:message.usedFields:message.usedFields]) {
		panic("quickfix: outbound builder field reservation invariant")
	}
}

// SetBytes copies a scalar value, including length-delimited binary DATA bytes.
// The caller supplies any required matching length field and validates syntax.
func (fields *OutboundFields) SetBytes(tag Tag, value []byte) error {
	if err := fields.reserveScalar(tag, len(value)); err != nil {
		return err
	}
	first := fields.message.usedFields
	fields.message.writeField(tag, value, "", false)
	fields.publish(tag, first)
	return nil
}

// SetString copies a scalar string without an intermediate byte allocation.
func (fields *OutboundFields) SetString(tag Tag, value string) error {
	if err := fields.reserveScalar(tag, len(value)); err != nil {
		return err
	}
	first := fields.message.usedFields
	fields.message.writeField(tag, nil, value, true)
	fields.publish(tag, first)
	return nil
}

// SetInt writes the signed decimal representation of value.
func (fields *OutboundFields) SetInt(tag Tag, value int) error {
	var scratch [32]byte
	encoded := strconv.AppendInt(scratch[:0], int64(value), 10)
	return fields.SetBytes(tag, encoded)
}

// SetBool writes FIX's Y or N representation.
func (fields *OutboundFields) SetBool(tag Tag, value bool) error {
	if value {
		return fields.SetString(tag, "Y")
	}
	return fields.SetString(tag, "N")
}

// SetTime writes a UTC timestamp using its configured precision and truncation.
func (fields *OutboundFields) SetTime(tag Tag, value FIXUTCTimestamp) error {
	var scratch [64]byte
	encoded, err := value.AppendTo(scratch[:0])
	if err != nil {
		return fields.message.fail(err)
	}
	return fields.SetBytes(tag, encoded)
}

// Remove removes an active map key without reclaiming its mutation budget.
func (fields *OutboundFields) Remove(tag Tag) error {
	if err := fields.message.writable(); err != nil {
		return err
	}
	fields.fields.fixedRemove(tag)
	return nil
}

// SetGroup copies a complete count-and-members sequence into contiguous owned
// TagValues. The first field's tag must equal tag. Nested groups retain their
// supplied order. Values can contain embedded SOH DATA or XML bytes: the caller
// must supply matching length fields. Dictionary membership, count correctness,
// required fields, and value semantics remain the caller/session's validation.
func (fields *OutboundFields) SetGroup(tag Tag, values []OutboundField) error {
	message := fields.message
	if err := message.writable(); err != nil {
		return err
	}
	if len(values) == 0 || values[0].Tag != tag {
		return message.fail(ErrInvalidOutboundField)
	}
	if len(values) > len(message.fields)-message.usedFields {
		return message.fail(ErrFieldCapacity)
	}
	remaining := cap(message.arena) - len(message.arena)
	for _, value := range values {
		length, err := outboundFieldLength(value.Tag, len(value.Value), remaining)
		if err != nil {
			return message.fail(err)
		}
		remaining -= length
	}
	first := message.usedFields
	for _, value := range values {
		message.writeField(value.Tag, value.Value, "", false)
	}
	fields.publish(tag, first)
	return nil
}

func outboundRawField(wire []byte) (tag Tag, separator, end int, err error) {
	end = bytes.IndexByte(wire, '\x01')
	if end < 0 {
		return 0, 0, 0, ErrInvalidOutboundField
	}
	separator = bytes.IndexByte(wire[:end], '=')
	if separator <= 0 {
		return 0, 0, 0, ErrInvalidOutboundField
	}
	number, parseErr := boundedUnsigned(wire[:separator], int(^uint(0)>>1), ErrInvalidOutboundField)
	if parseErr != nil || number == 0 {
		return 0, 0, 0, ErrInvalidOutboundField
	}
	return Tag(number), separator, end + 1, nil
}

// SetRawGroup copies a serialized textual count-and-members group without
// templates or clones. The first tag must equal tag; every field must terminate
// with SOH. Embedded SOH in binary DATA/XML values is not supported by this
// convenience scanner: use SetGroup for those values. Group semantics require
// separate dictionary validation, as with SetGroup.
func (fields *OutboundFields) SetRawGroup(tag Tag, wire []byte) error {
	message := fields.message
	if err := message.writable(); err != nil {
		return err
	}
	if len(wire) == 0 || tag <= 0 {
		return message.fail(ErrInvalidOutboundField)
	}
	if len(wire) > cap(message.arena)-len(message.arena) {
		return message.fail(ErrOutboundBuilderCapacity)
	}
	count := 0
	for offset := 0; offset < len(wire); {
		parsedTag, _, length, err := outboundRawField(wire[offset:])
		if err != nil || count == 0 && parsedTag != tag {
			return message.fail(ErrInvalidOutboundField)
		}
		count++
		if count > len(message.fields)-message.usedFields {
			return message.fail(ErrFieldCapacity)
		}
		offset += length
	}
	first := message.usedFields
	start := len(message.arena)
	message.arena = append(message.arena, wire...)
	owned := message.arena[start:len(message.arena):len(message.arena)]
	for offset := 0; offset < len(owned); {
		parsedTag, separator, length, _ := outboundRawField(owned[offset:])
		end := offset + length
		message.fields[message.usedFields] = TagValue{tag: parsedTag, value: owned[offset+separator+1 : end-1 : end-1], bytes: owned[offset:end:end]}
		message.usedFields++
		offset = end
	}
	fields.publish(tag, first)
	return nil
}

// CopyFrom replaces all sections with owned copies of the source's serialized
// fields, preserving each group's field order and binary values. The source
// must remain exclusively owned and unchanged during the call. It is never
// retained or mutated. Copying consumes the remaining append-only mutation
// budgets; call Reset first when starting a new message. Capacity failures
// leave existing destination fields and budgets intact apart from sticky Err.
// Copying the exact view is a no-op;
// other shallow aliases of this builder's maps are rejected before mutation.
func (message *OutboundMessage) CopyFrom(source *Message) error {
	if err := message.writable(); err != nil {
		return err
	}
	if source == nil {
		return message.fail(ErrInvalidOutboundSource)
	}
	if source == &message.message {
		return nil
	}
	sources := [3]*FieldMap{&source.Header.FieldMap, &source.Body.FieldMap, &source.Trailer.FieldMap}
	destinations := [3]*OutboundFields{&message.header, &message.body, &message.trailer}
	count, remaining := message.usedFields, cap(message.arena)-len(message.arena)
	var invalid error
	for _, fields := range sources {
		if fields.fixed != nil && (fields.fixed == message.message.Header.fixed || fields.fixed == message.message.Body.fixed || fields.fixed == message.message.Trailer.fixed) {
			return message.fail(ErrInvalidOutboundSource)
		}
		fields.eachField(func(tag Tag, values field) {
			if invalid != nil {
				return
			}
			if len(values) == 0 || tag <= 0 || values[0].tag != tag {
				invalid = ErrInvalidOutboundSource
				return
			}
			for _, value := range values {
				if count == len(message.fields) {
					invalid = ErrFieldCapacity
					return
				}
				if len(value.bytes) > remaining {
					invalid = ErrOutboundBuilderCapacity
					return
				}
				separator := bytes.IndexByte(value.bytes, '=')
				if value.tag <= 0 || separator <= 0 || len(value.bytes) < separator+2 || value.bytes[len(value.bytes)-1] != '\x01' {
					invalid = ErrInvalidOutboundSource
					return
				}
				count++
				remaining -= len(value.bytes)
			}
		})
	}
	if invalid != nil {
		return message.fail(invalid)
	}
	// Copy into previously unused arena/slab space. A source independently
	// parsed over an existing builder byte view stays unchanged throughout.
	// Old metadata remains in the mutation slab until Reset reclaims it.
	message.message.Header.fixedClear()
	message.message.Body.fixedClear()
	message.message.Trailer.fixedClear()
	for section, fields := range sources {
		fields.eachField(func(tag Tag, values field) {
			first := message.usedFields
			for _, value := range values {
				start := len(message.arena)
				message.arena = append(message.arena, value.bytes...)
				end := len(message.arena)
				separator := bytes.IndexByte(value.bytes, '=')
				message.fields[message.usedFields] = TagValue{tag: value.tag, value: message.arena[start+separator+1 : end-1 : end-1], bytes: message.arena[start:end:end]}
				message.usedFields++
			}
			destinations[section].publish(tag, first)
		})
	}
	message.message.ReceiveTime = source.ReceiveTime
	return nil
}
