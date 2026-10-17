package quickfix

import (
	"errors"
	"strconv"
	"unsafe"
)

var errMessageArenaLimits = errors.New("quickfix: reused message has different capacity limits from the destination session")

// messageArena belongs to one caller-owned Message. The live wire arena, owned
// value plane and alias staging area each have maxFrameBytes bytes; metadata and
// field indexes are allocated once. The logical limit is one encoded frame.
// No arena storage is transferred to a writer. A shallow value copy detaches
// through the allocating compatibility boundary before its first mutation.
type messageArena struct {
	owner         *Message
	sections      [3]*FieldMap
	backings      [3]*fixedFieldMap
	errors        [3]error
	bytes         []byte
	values        []byte
	staging       []byte
	records       []messageArenaField
	groupValues   []TagValue
	recordIndexes [3][]int
	used          int
	fieldCount    int
	maxFields     int
	beginLength   int
	bodyLength    int
	wireFields    int // Excludes the always-generated BodyLength and CheckSum.
}

type messageArenaField struct {
	section      int
	tag          Tag
	offset       int
	length       int
	values       field
	owned        bool // Getter values belong to the parallel owned-value plane.
	groupOffset  int
	boundedGroup bool
}

// attachMessageArena is setup for ordinary outbound integration. It preserves
// the current fields and groups, and never changes an oversized message.
func (message *Message) attachMessageArena(maxFrameBytes, maxFields int) error {
	if maxFrameBytes < 12 || maxFrameBytes > int(^uint(0)>>1)/3 || maxFields < 4 || maxFields > maxFrameBytes/3 || maxFields > int(^uint(0)>>1)/1024 {
		return ErrInvalidParseLimits
	}
	if err := message.constructionError(); err != nil {
		return err
	}
	if message.arena != nil && message.arena.owner == message {
		if !message.arena.intact() {
			// Public Header/Body/Trailer assignment is an allocating ownership
			// boundary. Preserve the replacement and detach surviving sections.
			message.arena.detach()
		} else if len(message.arena.bytes) != maxFrameBytes || message.arena.maxFields != maxFields {
			return errMessageArenaLimits
		} else {
			return message.constructionError()
		}
	}
	sections := [3]*FieldMap{&message.Header.FieldMap, &message.Body.FieldMap, &message.Trailer.FieldMap}
	used, count, beginLength, bodyLength, wireFields := 0, 0, 0, 0, 0
	var capacityError error
	for section, fields := range sections {
		fields.eachField(func(tag Tag, values field) {
			if capacityError != nil {
				return
			}
			if len(values) > maxFields-count {
				capacityError = ErrFieldCapacity
				return
			}
			count += len(values)
			if !arenaGeneratedField(section, tag) {
				wireFields += len(values)
			}
			for _, value := range values {
				if len(value.bytes) > maxFrameBytes-used {
					capacityError = ErrFrameCapacity
					return
				}
				used += len(value.bytes)
				if section == 0 && tag == tagBeginString {
					beginLength += len(value.bytes)
				} else if !(section == 0 && tag == tagBodyLength || section == 2 && tag == tagCheckSum) {
					bodyLength += len(value.bytes)
				}
			}
		})
	}
	if capacityError != nil {
		return capacityError
	}
	if wireFields > maxFields-2 {
		return ErrFieldCapacity
	}
	if beginLength+bodyLength+10+intTextLength(bodyLength) > maxFrameBytes {
		return ErrFrameCapacity
	}

	arena := &messageArena{owner: message, sections: sections, maxFields: maxFields, records: make([]messageArenaField, 0, maxFields)}
	arena.groupValues = make([]TagValue, 0, maxFields)
	storage := make([]byte, 3*maxFrameBytes)
	arena.bytes = storage[:maxFrameBytes:maxFrameBytes]
	arena.values = storage[maxFrameBytes : 2*maxFrameBytes : 2*maxFrameBytes]
	arena.staging = storage[2*maxFrameBytes:]
	for section, fields := range sections {
		original := *fields
		fields.initFixed(original.compare, maxFields)
		fields.arena, fields.arenaSection, fields.reusable = arena, section, true
		arena.backings[section] = fields.fixed
		arena.recordIndexes[section] = make([]int, len(fields.fixed.entries))
		original.eachField(func(tag Tag, values field) {
			// This cold copy preserves group ranges and leaves caller-owned
			// SetBytes values aliased. Reusable source values must detach too.
			copied := make(field, len(values))
			copy(copied, values)
			if original.scoped || original.reusable {
				copied = cloneOwnedField(values)
			}
			record := messageArenaField{section: section, tag: tag, offset: arena.used, values: copied}
			for index := range copied {
				wire := copied[index].bytes
				copy(arena.bytes[arena.used:], wire)
				copied[index].bytes = arena.bytes[arena.used : arena.used+len(wire) : arena.used+len(wire)]
				arena.used += len(wire)
				record.length += len(wire)
			}
			arena.records = append(arena.records, record)
			arena.fieldCount += len(copied)
			fields.put(tag, copied)
			entry, _ := fields.fixed.find(tag)
			arena.recordIndexes[section][entry] = len(arena.records)
		})
	}
	arena.beginLength, arena.bodyLength, arena.wireFields = beginLength, bodyLength, wireFields
	message.arena = arena
	return nil
}

func (message *Message) constructionError() error {
	for _, fields := range [...]*FieldMap{&message.Header.FieldMap, &message.Body.FieldMap, &message.Trailer.FieldMap} {
		if err := fields.constructionError(); err != nil {
			return err
		}
	}
	return nil
}

func (fields *FieldMap) constructionError() error {
	if fields.constructionErr != nil {
		return fields.constructionErr
	}
	if fields.arena != nil {
		return fields.arena.errors[fields.arenaSection]
	}
	return nil
}

func (fields *FieldMap) mutableArena() *messageArena {
	if fields.arena == nil {
		return nil
	}
	if fields.arena.sections[fields.arenaSection] != fields {
		fields.detachArena()
		return nil
	}
	if !fields.arena.intact() {
		fields.arena.detach()
		return nil
	}
	return fields.arena
}

func (arena *messageArena) intact() bool {
	for index, section := range arena.sections {
		if section.arena != arena || section.arenaSection != index || section.fixed != arena.backings[index] {
			return false
		}
	}
	return true
}

// detachArena is an allocating convenience boundary. Detaching an original
// section detaches its whole message so other sections cannot retain stale
// byte/count accounting. A copied section detaches only that copied value.
func (fields *FieldMap) detachArena() {
	arena := fields.arena
	if arena == nil {
		return
	}
	if arena.sections[fields.arenaSection] != fields {
		fields.detachArenaSection()
		return
	}
	arena.detach()
}

func (arena *messageArena) detach() {
	for _, section := range arena.sections {
		if section.arena == arena {
			section.detachArenaSection()
		}
	}
	arena.owner.arena = nil
}

func (fields *FieldMap) detachArenaSection() {
	original := *fields
	constructionErr := fields.constructionError()
	fields.arena = nil
	fields.tags = nil
	fields.initWithOrdering(original.compare)
	fields.constructionErr = constructionErr
	original.eachField(func(tag Tag, values field) { fields.put(tag, cloneOwnedField(values)) })
}

func (arena *messageArena) find(section int, tag Tag) int {
	if entry, found := arena.sections[section].fixed.find(tag); found {
		return arena.recordIndexes[section][entry] - 1
	}
	return -1
}

func arenaGeneratedField(section int, tag Tag) bool {
	return section == 0 && tag == tagBodyLength || section == 2 && tag == tagCheckSum
}

func (arena *messageArena) account(section int, tag Tag, lengthDelta, countDelta int) {
	arena.fieldCount += countDelta
	if arenaGeneratedField(section, tag) {
		return
	}
	arena.wireFields += countDelta
	if section == 0 && tag == tagBeginString {
		arena.beginLength += lengthDelta
	} else {
		arena.bodyLength += lengthDelta
	}
}

func (arena *messageArena) preflight(section int, tag Tag, index, length, count int) bool {
	if arena.errors[section] != nil {
		return false
	}
	if index < 0 && len(arena.records) == cap(arena.records) {
		arena.errors[section] = ErrFieldCapacity
		return false
	}
	oldLength, oldCount := 0, 0
	if index >= 0 {
		oldLength, oldCount = arena.records[index].length, len(arena.records[index].values)
	}
	if count > arena.maxFields-(arena.fieldCount-oldCount) {
		arena.errors[section] = ErrFieldCapacity
		return false
	}
	if !arenaGeneratedField(section, tag) && count > arena.maxFields-2-(arena.wireFields-oldCount) {
		arena.errors[section] = ErrFieldCapacity
		return false
	}
	if length > len(arena.bytes)-(arena.used-oldLength) {
		arena.errors[section] = ErrFrameCapacity
		return false
	}
	beginLength, bodyLength := arena.beginLength, arena.bodyLength
	if section == 0 && tag == tagBeginString {
		beginLength += length - oldLength
	} else if !arenaGeneratedField(section, tag) {
		bodyLength += length - oldLength
	}
	if beginLength+bodyLength+10+intTextLength(bodyLength) > len(arena.bytes) {
		arena.errors[section] = ErrFrameCapacity
		return false
	}
	return true
}

// arenaOverlaps only compares addresses while both slices remain live. It never
// reconstructs pointers or stores uintptrs. This handles arbitrary subslices in
// constant time without scanning the frame on every external SetBytes call.
func arenaOverlaps(value, storage []byte) bool {
	if len(value) == 0 || len(storage) == 0 {
		return false
	}
	valueStart := uintptr(unsafe.Pointer(unsafe.SliceData(value)))
	storageStart := uintptr(unsafe.Pointer(unsafe.SliceData(storage)))
	return valueStart >= storageStart && valueStart-storageStart < uintptr(len(storage)) ||
		storageStart >= valueStart && storageStart-valueStart < uintptr(len(value))
}

func (arena *messageArena) setBytes(fields *FieldMap, tag Tag, value []byte) {
	// Check capacity before touching even the staging plane. Only arbitrary
	// byte input needs alias classification; typed formatters use private stack
	// bytes and never need pointer inspection.
	if !arena.scalarFits(fields, tag, len(value)) {
		return
	}
	external, owned := value, false
	if arenaOverlaps(value, arena.bytes) || arenaOverlaps(value, arena.values) {
		copy(arena.staging, value)
		value, external, owned = arena.staging[:len(value)], nil, true
	}
	arena.setScalar(fields, tag, value, "", external, owned)
}

func (arena *messageArena) scalarFits(fields *FieldMap, tag Tag, valueLength int) bool {
	section := fields.arenaSection
	index := arena.find(section, tag)
	prefixLength := intTextLength(int(tag)) + 1
	if valueLength > len(arena.bytes)-prefixLength-1 {
		if arena.errors[section] == nil {
			arena.errors[section] = ErrFrameCapacity
		}
		return false
	}
	return arena.preflight(section, tag, index, prefixLength+valueLength+1, 1)
}

func (arena *messageArena) setScalar(fields *FieldMap, tag Tag, value []byte, text string, external []byte, owned bool) {
	valueLength := len(value)
	if text != "" {
		valueLength = len(text)
	}
	if !arena.scalarFits(fields, tag, valueLength) {
		return
	}
	section := fields.arenaSection
	index := arena.find(section, tag)
	prefixLength := intTextLength(int(tag)) + 1
	length := prefixLength + valueLength + 1
	offset := arena.used
	if index >= 0 {
		offset = arena.records[index].offset
		arena.resize(index, length)
	} else {
		index = len(arena.records)
		arena.records = arena.records[:index+1]
		arena.records[index] = messageArenaField{section: section, tag: tag, offset: offset}
		arena.used += length
	}
	record := &arena.records[index]
	arena.account(section, tag, length-record.length, 1-len(record.values))
	arena.releaseGroupValues(record)
	record.length, record.owned = length, owned
	record.values = fields.fixedGetOrCreate(tag)
	entry, _ := fields.fixed.find(tag)
	arena.recordIndexes[section][entry] = index + 1
	wire := arena.bytes[offset : offset : offset+length]
	wire = strconv.AppendInt(wire, int64(tag), 10)
	wire = append(wire, '=')
	if text != "" {
		wire = append(wire, text...)
	} else {
		wire = append(wire, value...)
	}
	wire = append(wire, '\x01')
	if owned {
		external = arena.values[offset+prefixLength : offset+length-1 : offset+length-1]
		copy(external, wire[prefixLength:length-1])
	}
	record.values[0] = TagValue{tag: tag, value: external, bytes: wire}
}

func (arena *messageArena) resize(index, length int) {
	record := &arena.records[index]
	delta := length - record.length
	if delta == 0 {
		return
	}
	end := record.offset + record.length
	copy(arena.bytes[end+delta:], arena.bytes[end:arena.used])
	copy(arena.values[end+delta:], arena.values[end:arena.used])
	arena.used += delta
	for current := range arena.records {
		if current == index || arena.records[current].offset < end {
			continue
		}
		shifted := &arena.records[current]
		shifted.offset += delta
		arena.bind(shifted)
	}
}

func (arena *messageArena) bind(record *messageArenaField) {
	offset := record.offset
	for index := range record.values {
		value := &record.values[index]
		length := len(value.bytes)
		value.bytes = arena.bytes[offset : offset+length : offset+length]
		if record.owned {
			value.value = arena.values[offset+intTextLength(int(value.tag))+1 : offset+length-1 : offset+length-1]
		}
		offset += length
	}
}

func (arena *messageArena) remove(fields *FieldMap, tag Tag) {
	index := arena.find(fields.arenaSection, tag)
	if index < 0 {
		return
	}
	arena.resize(index, 0)
	arena.account(fields.arenaSection, tag, -arena.records[index].length, -len(arena.records[index].values))
	arena.releaseGroupValues(&arena.records[index])
	entry, _ := fields.fixed.find(tag)
	arena.recordIndexes[fields.arenaSection][entry] = 0
	fields.fixedRemove(tag)
	copy(arena.records[index:], arena.records[index+1:])
	arena.records[len(arena.records)-1] = messageArenaField{}
	arena.records = arena.records[:len(arena.records)-1]
	for current := index; current < len(arena.records); current++ {
		record := &arena.records[current]
		entry, _ := arena.sections[record.section].fixed.find(record.tag)
		arena.recordIndexes[record.section][entry] = current + 1
	}
}

// FieldGroupWriter.Write remains an allocating compatibility boundary. The
// returned group range is copied before any move, preserving aliasing sources.
func (arena *messageArena) setGroup(fields *FieldMap, tag Tag, values field) {
	section := fields.arenaSection
	index := arena.find(section, tag)
	length := 0
	for _, value := range values {
		if len(value.bytes) > len(arena.bytes)-length {
			if arena.errors[section] == nil {
				arena.errors[section] = ErrFrameCapacity
			}
			return
		}
		length += len(value.bytes)
	}
	if !arena.preflight(section, tag, index, length, len(values)) {
		return
	}
	copied := cloneOwnedField(values)
	offset := arena.used
	if index >= 0 {
		offset = arena.records[index].offset
		arena.resize(index, length)
	} else {
		index = len(arena.records)
		arena.records = arena.records[:index+1]
		arena.records[index] = messageArenaField{section: section, tag: tag, offset: offset}
		arena.used += length
	}
	record := &arena.records[index]
	arena.account(section, tag, length-record.length, len(copied)-len(record.values))
	arena.releaseGroupValues(record)
	record.length, record.values, record.owned = length, copied, false
	for current := range copied {
		wire := copied[current].bytes
		copy(arena.bytes[offset:], wire)
		copied[current].bytes = arena.bytes[offset : offset+len(wire) : offset+len(wire)]
		offset += len(wire)
	}
	fields.put(tag, copied)
	entry, _ := fields.fixed.find(tag)
	arena.recordIndexes[section][entry] = index + 1
}

func (arena *messageArena) clear(fields *FieldMap) {
	for index := len(arena.records) - 1; index >= 0; index-- {
		if record := &arena.records[index]; record.section == fields.arenaSection {
			arena.remove(fields, record.tag)
		}
	}
	fields.fixedClear()
	arena.errors[fields.arenaSection] = nil
}
