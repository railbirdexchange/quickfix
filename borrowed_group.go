package quickfix

import (
	"errors"

	"github.com/quickfixgo/quickfix/datadictionary"
)

// ErrInvalidBorrowedGroup indicates an invalid group count, delimiter, or nested
// group range. Dictionary validation remains responsible for FIX reject details.
var ErrInvalidBorrowedGroup = errors.New("quickfix: invalid borrowed group structure")

// ErrBorrowedGroupNotFound indicates that an immediate repeating group is absent.
var ErrBorrowedGroupNotFound = errors.New("quickfix: borrowed group not found")

// BorrowedGroup is a read-only view of a dictionary-backed repeating group.
// The group, its entries, iterators, and returned bytes borrow the message slot's
// field and input storage. They expire when the slot or its input is reused.
// The slot must have one owner, and its dictionary must remain immutable.
// This view does not replace dictionary validation or the legacy Group API.
type BorrowedGroup struct {
	definition *datadictionary.FieldDef
	fields     []TagValue
	count      int
}

// BorrowedGroupEntry is a read-only entry whose lifetime matches its group.
type BorrowedGroupEntry struct {
	definition *datadictionary.FieldDef
	fields     []TagValue
}

// BorrowedGroupIterator walks a group without constructing mutable Group values.
// It must not be used after the originating message slot or input is reused.
type BorrowedGroupIterator struct {
	group     BorrowedGroup
	offset    int
	remaining int
}

func newBorrowedGroup(values []TagValue, definition *datadictionary.FieldDef) (BorrowedGroup, error) {
	count, consumed, err := borrowedGroupSpan(values, definition)
	if err != nil || consumed != len(values) {
		return BorrowedGroup{}, ErrInvalidBorrowedGroup
	}
	return BorrowedGroup{definition: definition, fields: values[:consumed:consumed], count: count}, nil
}

// Len returns the number of entries declared and present in the group.
func (group BorrowedGroup) Len() int { return group.count }

// Iter returns an iterator positioned before the first entry.
func (group BorrowedGroup) Iter() BorrowedGroupIterator {
	return BorrowedGroupIterator{group: group, offset: 1, remaining: group.count}
}

// Next returns the next entry and true, or an empty entry and false at the end.
// Group construction verifies all entry boundaries before iteration begins.
func (iterator *BorrowedGroupIterator) Next() (BorrowedGroupEntry, bool) {
	if iterator.remaining == 0 {
		return BorrowedGroupEntry{}, false
	}
	length, err := borrowedEntrySpan(iterator.group.fields[iterator.offset:], iterator.group.definition)
	if err != nil {
		// Only mutation after construction can invalidate a borrowed view.
		iterator.remaining = 0
		return BorrowedGroupEntry{}, false
	}
	end := iterator.offset + length
	entry := BorrowedGroupEntry{
		definition: iterator.group.definition,
		fields:     iterator.group.fields[iterator.offset:end:end],
	}
	iterator.offset = end
	iterator.remaining--
	return entry, true
}

// GetBytes returns an immediate field's borrowed value, including a nested
// group's count field. Fields inside nested entries are not searched. The bytes
// must not be modified or retained after the message slot or input is reused.
func (entry BorrowedGroupEntry) GetBytes(tag Tag) ([]byte, bool) {
	values, _ := entry.find(tag)
	if len(values) == 0 {
		return nil, false
	}
	return values[0].value, true
}

// Group returns an immediate nested group. Scalar fields and absent groups
// return ErrBorrowedGroupNotFound. The nested view shares this entry's lifetime.
func (entry BorrowedGroupEntry) Group(tag Tag) (BorrowedGroup, error) {
	values, definition := entry.find(tag)
	if definition == nil || !definition.IsGroup() {
		return BorrowedGroup{}, ErrBorrowedGroupNotFound
	}
	return newBorrowedGroup(values, definition)
}

func (entry BorrowedGroupEntry) find(tag Tag) ([]TagValue, *datadictionary.FieldDef) {
	for offset := 0; offset < len(entry.fields); {
		definition := entry.definition.Child(int(entry.fields[offset].tag))
		if definition == nil {
			return nil, nil
		}
		length := 1
		if definition.IsGroup() {
			_, consumed, err := borrowedGroupSpan(entry.fields[offset:], definition)
			if err != nil {
				return nil, nil
			}
			length = consumed
		}
		end := offset + length
		if entry.fields[offset].tag == tag {
			return entry.fields[offset:end:end], definition
		}
		offset = end
	}
	return nil, nil
}

// borrowedGroupSpan scans only the declared group entries, leaving adjacent
// fields to the caller. MessageSlot has already enforced the nesting-depth and
// field-count limits, so recursion is bounded by its configured slot limits.
func borrowedGroupSpan(values []TagValue, definition *datadictionary.FieldDef) (count, consumed int, err error) {
	if definition == nil || len(definition.Fields) == 0 || len(values) == 0 || int(values[0].tag) != definition.Tag() {
		return 0, 0, ErrInvalidBorrowedGroup
	}
	count, err = borrowedGroupCount(values[0].value)
	if err != nil || count > len(values)-1 {
		return 0, 0, ErrInvalidBorrowedGroup
	}
	consumed = 1
	for entry := 0; entry < count; entry++ {
		length, err := borrowedEntrySpan(values[consumed:], definition)
		if err != nil {
			return 0, 0, err
		}
		consumed += length
	}
	return count, consumed, nil
}

func borrowedEntrySpan(values []TagValue, definition *datadictionary.FieldDef) (int, error) {
	delimiter := Tag(definition.Fields[0].Tag())
	if len(values) == 0 || values[0].tag != delimiter {
		return 0, ErrInvalidBorrowedGroup
	}
	consumed := 0
	for consumed < len(values) {
		if consumed > 0 && values[consumed].tag == delimiter {
			break
		}
		child := definition.Child(int(values[consumed].tag))
		if child == nil {
			break
		}
		if child.IsGroup() {
			_, length, err := borrowedGroupSpan(values[consumed:], child)
			if err != nil {
				return 0, err
			}
			consumed += length
		} else {
			consumed++
		}
	}
	return consumed, nil
}

func borrowedGroupCount(value []byte) (int, error) {
	if len(value) == 0 {
		return 0, ErrInvalidBorrowedGroup
	}
	negative := value[0] == '-'
	if negative {
		value = value[1:]
		if len(value) == 0 {
			return 0, ErrInvalidBorrowedGroup
		}
	}
	count := 0
	maximum := int(^uint(0) >> 1)
	for _, digit := range value {
		if digit < '0' || digit > '9' || count > (maximum-int(digit-'0'))/10 {
			return 0, ErrInvalidBorrowedGroup
		}
		count = count*10 + int(digit-'0')
	}
	if negative && count != 0 {
		return 0, ErrInvalidBorrowedGroup
	}
	return count, nil
}
