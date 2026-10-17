package quickfix

import (
	"bytes"
	"errors"
	"strconv"
)

// ErrInvalidGroupValues indicates an invalid count or tag in SetGroupValues.
var ErrInvalidGroupValues = errors.New("quickfix: invalid ordered group values")

// GroupField is one field in the wire order of an outgoing repeating group.
// Value is used when non-nil; otherwise Text is used. Both representations are
// copied synchronously by SetGroupValues, including empty and binary values.
type GroupField struct {
	Tag   Tag
	Value []byte
	Text  string
}

func (value GroupField) length() int {
	if value.Value != nil {
		return len(value.Value)
	}
	return len(value.Text)
}

// SetGroupValues replaces a repeating group using fields already in wire order.
// The values slice excludes the generated count field. Nested groups are flattened in
// order, including their count fields. The caller supplies the correct instance
// count, delimiters and dictionary order; this method does not infer a template.
//
// The method owns a snapshot of all supplied values on return. With an attached
// outbound message arena, it uses fixed metadata and byte storage and allocates
// nothing. Standalone maps retain their allocating, owned-storage behavior.
// Borrowed getter values expire on the next mutation of the same message.
//
// Capacity errors preserve existing fields and latch the section's construction
// error until Clear, just like other bounded setters. Invalid tags/counts leave
// the message unchanged. No partial group is installed on either failure.
func (fields *FieldMap) SetGroupValues(countTag Tag, count int, values []GroupField) error {
	if countTag <= 0 || count < 0 || count > len(values) || count == 0 && len(values) != 0 {
		return ErrInvalidGroupValues
	}
	for _, value := range values {
		if value.Tag <= 0 {
			return ErrInvalidGroupValues
		}
	}
	arena := fields.mutableArena()
	if err := fields.constructionError(); err != nil {
		return err
	}
	if arena != nil {
		return arena.setGroupValues(fields, countTag, count, values)
	}
	if fields.fixed != nil {
		if _, found := fields.lookup(countTag); !found && fields.fieldCount() == cap(fields.fixed.tags) {
			if !fields.scoped {
				return ErrFieldCapacity
			}
			fields.detachScopedFields()
		}
	}
	owned := make(field, len(values)+1)
	owned[0].init(countTag, strconv.AppendInt(nil, int64(count), 10))
	for index, value := range values {
		var copied []byte
		if value.Value != nil {
			copied = bytes.Clone(value.Value)
		} else {
			copied = []byte(value.Text)
		}
		owned[index+1].init(value.Tag, copied)
	}
	fields.lock()
	defer fields.unlock()
	if !fields.put(countTag, owned) {
		return ErrFieldCapacity
	}
	return nil
}

func (arena *messageArena) setGroupValues(fields *FieldMap, countTag Tag, count int, values []GroupField) error {
	section := fields.arenaSection
	index := arena.find(section, countTag)
	// Every group field, including its count, consumes the same configured
	// field budget as scalar and parsed fields.
	if len(values) >= arena.maxFields {
		arena.errors[section] = ErrFieldCapacity
		return ErrFieldCapacity
	}
	length := intTextLength(int(countTag)) + 2 + intTextLength(count)
	for _, value := range values {
		prefix := intTextLength(int(value.Tag)) + 2
		if prefix > len(arena.bytes)-length || value.length() > len(arena.bytes)-length-prefix {
			arena.errors[section] = ErrFrameCapacity
			return ErrFrameCapacity
		}
		length += prefix + value.length()
	}
	if !arena.preflight(section, countTag, index, length, len(values)+1) {
		return arena.errors[section]
	}
	// Snapshot every input before any wire/value-plane move. This also handles
	// a group assembled from arbitrary subslices of its own previous values.
	staged := strconv.AppendInt(arena.staging[:0], int64(countTag), 10)
	staged = append(staged, '=')
	staged = strconv.AppendInt(staged, int64(count), 10)
	staged = append(staged, '\x01')
	for _, value := range values {
		staged = strconv.AppendInt(staged, int64(value.Tag), 10)
		staged = append(staged, '=')
		if value.Value != nil {
			staged = append(staged, value.Value...)
		} else {
			staged = append(staged, value.Text...)
		}
		staged = append(staged, '\x01')
	}
	offset := arena.used
	if index >= 0 {
		offset = arena.records[index].offset
		arena.resize(index, length)
	} else {
		index = len(arena.records)
		arena.records = arena.records[:index+1]
		arena.records[index] = messageArenaField{section: section, tag: countTag, offset: offset}
		arena.used += length
	}
	record := &arena.records[index]
	arena.account(section, countTag, length-record.length, len(values)+1-len(record.values))
	arena.releaseGroupValues(record)
	start := len(arena.groupValues)
	arena.groupValues = arena.groupValues[:start+len(values)+1]
	record.values = arena.groupValues[start:len(arena.groupValues):len(arena.groupValues)]
	record.groupOffset, record.boundedGroup = start, true
	record.length, record.owned = length, true
	copy(arena.bytes[offset:offset+length], staged)
	copy(arena.values[offset:offset+length], staged)
	countLength := intTextLength(int(countTag)) + 2 + intTextLength(count)
	arena.bindGroupValue(&record.values[0], countTag, offset, countLength)
	offset += countLength
	for current, value := range values {
		valueLength := intTextLength(int(value.Tag)) + 2 + value.length()
		arena.bindGroupValue(&record.values[current+1], value.Tag, offset, valueLength)
		offset += valueLength
	}
	fields.put(countTag, record.values)
	entry, _ := fields.fixed.find(countTag)
	arena.recordIndexes[section][entry] = index + 1
	return nil
}

func (arena *messageArena) bindGroupValue(value *TagValue, tag Tag, offset, length int) {
	*value = TagValue{
		tag:   tag,
		value: arena.values[offset+intTextLength(int(tag))+1 : offset+length-1 : offset+length-1],
		bytes: arena.bytes[offset : offset+length : offset+length],
	}
}

// releaseGroupValues compacts the fixed metadata slab, updating all live group
// ranges before the released record is replaced or removed. Wire/value storage
// has its own independent compaction in resize.
func (arena *messageArena) releaseGroupValues(record *messageArenaField) {
	if !record.boundedGroup {
		return
	}
	start, count := record.groupOffset, len(record.values)
	end := start + count
	copy(arena.groupValues[start:], arena.groupValues[end:])
	newLength := len(arena.groupValues) - count
	clear(arena.groupValues[newLength:])
	arena.groupValues = arena.groupValues[:newLength]
	record.boundedGroup = false
	for current := range arena.records {
		shifted := &arena.records[current]
		if !shifted.boundedGroup || shifted.groupOffset < end {
			continue
		}
		shifted.groupOffset -= count
		shifted.values = arena.groupValues[shifted.groupOffset : shifted.groupOffset+len(shifted.values) : shifted.groupOffset+len(shifted.values)]
		arena.sections[shifted.section].put(shifted.tag, shifted.values)
	}
}
