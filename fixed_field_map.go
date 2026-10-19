package quickfix

import (
	"errors"
	"slices"
)

// ErrFieldCapacity indicates that a bounded field map has no vacant field slot.
// Strict bounded setters cannot return an error and panic on exhaustion.
var ErrFieldCapacity = errors.New("quickfix: configured field capacity exhausted")

const (
	fixedFieldVacant uint8 = iota
	fixedFieldOccupied
	fixedFieldRemoved
)

type fixedFieldEntry struct {
	tag    Tag
	values field
	scalar [1]TagValue
	state  uint8
}

// fixedFieldMap belongs to one message owner. Its pointer is shared by FieldMap
// value copies so lookup, removal, and clearing observe the same active fields.
type fixedFieldMap struct {
	entries []fixedFieldEntry
	tags    []Tag
	touched []int
}

// initFixed allocates the entire lookup and tag index before use. Parsed fields
// borrow their TagValue slices from the owning message slot. Ordinary setters
// remain allocating compatibility APIs. Strict bounded maps never enlarge their
// capacity; session callback setters may detach to ordinary storage instead.
func (fields *FieldMap) initFixed(ordering tagOrder, maxFields int) {
	maximumInt := int(^uint(0) >> 1)
	if maxFields <= 0 || maxFields > maximumInt/2 {
		panic("quickfix: invalid fixed field capacity")
	}
	tableSize := 1
	for tableSize < maxFields {
		if tableSize > maximumInt/2 {
			panic("quickfix: fixed field lookup capacity overflows int")
		}
		tableSize *= 2
	}
	if tableSize > maximumInt/2 {
		panic("quickfix: fixed field lookup capacity overflows int")
	}
	backing := &fixedFieldMap{
		entries: make([]fixedFieldEntry, tableSize*2),
		tags:    make([]Tag, 0, maxFields),
		touched: make([]int, 0, tableSize*2),
	}
	*fields = FieldMap{fixed: backing, tagSort: tagSort{tags: backing.tags, compare: ordering}, missingRejects: fields.missingRejects}
}

// find returns either the existing entry or a vacant/tombstone insertion slot.
// The bounded probe also handles a table whose unused entries became tombstones
// after many disjoint tag combinations, without rebuilding or allocating.
func (backing *fixedFieldMap) find(tag Tag) (int, bool) {
	hash := uint64(tag) * 11400714819323198485
	hash ^= hash >> 32
	mask := len(backing.entries) - 1
	index := int(hash & uint64(mask))
	removed := -1
	for probes := 0; probes < len(backing.entries); probes++ {
		entry := &backing.entries[index]
		switch entry.state {
		case fixedFieldVacant:
			if removed >= 0 {
				return removed, false
			}
			return index, false
		case fixedFieldRemoved:
			if removed < 0 {
				removed = index
			}
		case fixedFieldOccupied:
			if entry.tag == tag {
				return index, true
			}
		}
		index = (index + 1) & mask
	}
	return removed, false
}

func (fields *FieldMap) lookup(tag Tag) (values field, found bool) {
	if fields.fixed != nil {
		return fields.fixed.lookup(tag)
	}
	values, found = fields.tagLookup[tag]
	return
}

func (backing *fixedFieldMap) lookup(tag Tag) (field, bool) {
	index, found := backing.find(tag)
	if !found {
		return nil, false
	}
	return backing.entries[index].values, true
}

// put stores a borrowed field range. A false result leaves all state unchanged.
// The caller must own the field map exclusively or hold its legacy write lock.
func (fields *FieldMap) put(tag Tag, values field) bool {
	if fields.fixed == nil {
		if _, found := fields.tagLookup[tag]; !found {
			fields.tags = append(fields.tags, tag)
		}
		fields.tagLookup[tag] = values
		return true
	}
	index, found := fields.fixed.find(tag)
	if !found {
		if len(fields.fixed.tags) == cap(fields.fixed.tags) || index < 0 {
			return false
		}
		count := len(fields.fixed.tags)
		fields.fixed.tags = fields.fixed.tags[:count+1]
		fields.fixed.tags[count] = tag
	}
	entry := &fields.fixed.entries[index]
	if entry.state == fixedFieldVacant {
		// A removed entry remains in this list until clear. Recording only the
		// first use bounds it by the table size even under remove/reinsert churn.
		count := len(fields.fixed.touched)
		fields.fixed.touched = fields.fixed.touched[:count+1]
		fields.fixed.touched[count] = index
	}
	entry.tag = tag
	entry.values = values
	entry.state = fixedFieldOccupied
	fields.tags = fields.fixed.tags
	return true
}

func (fields *FieldMap) fieldTags() []Tag {
	if fields.fixed != nil {
		return fields.fixed.tags
	}
	return fields.tags
}

func (fields *FieldMap) fieldCount() int {
	if fields.fixed != nil {
		return len(fields.fixed.tags)
	}
	return len(fields.tagLookup)
}

func (fields *FieldMap) eachField(visit func(Tag, field)) {
	if fields.fixed == nil {
		for tag, values := range fields.tagLookup {
			visit(tag, values)
		}
		return
	}
	for index := range fields.fixed.entries {
		entry := &fields.fixed.entries[index]
		if entry.state == fixedFieldOccupied {
			visit(entry.tag, entry.values)
		}
	}
}

func (fields *FieldMap) fixedGetOrCreate(tag Tag) field {
	index, found := fields.fixed.find(tag)
	if !found && (len(fields.fixed.tags) == cap(fields.fixed.tags) || index < 0) {
		if fields.scoped {
			fields.detachScopedFields()
			return fields.getOrCreate(tag)
		}
		panic(ErrFieldCapacity)
	}
	entry := &fields.fixed.entries[index]
	if found {
		entry.scalar[0] = entry.values[0]
	} else {
		entry.scalar[0] = TagValue{}
	}
	fields.put(tag, entry.scalar[:])
	return entry.scalar[:]
}

func (fields *FieldMap) fixedRemove(tag Tag) {
	index, found := fields.fixed.find(tag)
	if !found {
		return
	}
	fields.fixed.entries[index] = fixedFieldEntry{state: fixedFieldRemoved}
	for tagIndex, candidate := range fields.fixed.tags {
		if candidate == tag {
			copy(fields.fixed.tags[tagIndex:], fields.fixed.tags[tagIndex+1:])
			fields.fixed.tags = fields.fixed.tags[:len(fields.fixed.tags)-1]
			fields.tags = fields.fixed.tags
			return
		}
	}
}

func (fields *FieldMap) fixedClear() {
	for _, index := range fields.fixed.touched {
		fields.fixed.entries[index] = fixedFieldEntry{}
	}
	fields.fixed.touched = fields.fixed.touched[:0]
	fields.fixed.tags = fields.fixed.tags[:0]
	fields.tags = fields.fixed.tags
}

func (fields *FieldMap) fixedSortedTags() []Tag {
	fields.tags = fields.fixed.tags
	slices.SortFunc(fields.tags, func(left, right Tag) int {
		if fields.compare(left, right) {
			return -1
		}
		if fields.compare(right, left) {
			return 1
		}
		return 0
	})
	return fields.tags
}

func (backing *fixedFieldMap) total() int {
	total := 0
	for index := range backing.entries {
		entry := &backing.entries[index]
		if entry.state != fixedFieldOccupied {
			continue
		}
		for _, value := range entry.values {
			if value.tag != tagCheckSum {
				total += value.total()
			}
		}
	}
	return total
}

func (backing *fixedFieldMap) length() int {
	length := 0
	for index := range backing.entries {
		entry := &backing.entries[index]
		if entry.state != fixedFieldOccupied {
			continue
		}
		for _, value := range entry.values {
			switch value.tag {
			case tagBeginString, tagBodyLength, tagCheckSum:
			default:
				length += value.length()
			}
		}
	}
	return length
}

func (fields *FieldMap) rlock() {
	if fields.fixed == nil {
		fields.rwLock.RLock()
	}
}

func (fields *FieldMap) runlock() {
	if fields.fixed == nil {
		fields.rwLock.RUnlock()
	}
}

func (fields *FieldMap) lock() {
	if fields.fixed == nil {
		fields.rwLock.Lock()
	}
}

func (fields *FieldMap) unlock() {
	if fields.fixed == nil {
		fields.rwLock.Unlock()
	}
}
