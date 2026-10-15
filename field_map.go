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
	"sort"
	"sync"
	"time"
)

// field stores a slice of TagValues.
type field []TagValue

func fieldTag(f field) Tag {
	return f[0].tag
}

func initField(f field, tag Tag, value []byte) {
	f[0].init(tag, value)
}

func writeField(f field, buffer *bytes.Buffer) {
	for _, tv := range f {
		buffer.Write(tv.bytes)
	}
}

// tagOrder true if tag i should occur before tag j.
type tagOrder func(i, j Tag) bool

type tagSort struct {
	tags    []Tag
	compare tagOrder
}

func (t tagSort) Len() int           { return len(t.tags) }
func (t tagSort) Swap(i, j int)      { t.tags[i], t.tags[j] = t.tags[j], t.tags[i] }
func (t tagSort) Less(i, j int) bool { return t.compare(t.tags[i], t.tags[j]) }

// FieldMap is a collection of fix fields that make up a fix message.
type FieldMap struct {
	tagLookup map[Tag]field
	tagSort
	rwLock *sync.RWMutex
	fixed  *fixedFieldMap
}

// ascending tags.
func normalFieldOrder(i, j Tag) bool { return i < j }

func (m *FieldMap) init() {
	m.initWithOrdering(normalFieldOrder)
}

func (m *FieldMap) initWithOrdering(ordering tagOrder) {
	m.fixed = nil
	m.rwLock = &sync.RWMutex{}
	m.tagLookup = make(map[Tag]field)
	m.compare = ordering
}

// Tags returns all of the Field Tags in this FieldMap.
func (m FieldMap) Tags() []Tag {
	m.rlock()
	defer m.runlock()

	tags := make([]Tag, 0, m.fieldCount())
	m.eachField(func(tag Tag, _ field) { tags = append(tags, tag) })

	return tags
}

// Get parses out a field in this FieldMap. Returned reject may indicate the field is not present, or the field value is invalid.
func (m FieldMap) Get(parser Field) MessageRejectError {
	return m.GetField(parser.Tag(), parser)
}

// Has returns true if the Tag is present in this FieldMap.
func (m FieldMap) Has(tag Tag) bool {
	m.rlock()
	defer m.runlock()

	_, ok := m.lookup(tag)
	return ok
}

// GetField parses of a field with Tag tag. Returned reject may indicate the field is not present, or the field value is invalid.
func (m FieldMap) GetField(tag Tag, parser FieldValueReader) MessageRejectError {
	m.rlock()
	defer m.runlock()

	f, ok := m.lookup(tag)
	if !ok {
		return ConditionallyRequiredFieldMissing(tag)
	}

	if err := parser.Read(f[0].value); err != nil {
		return IncorrectDataFormatForValue(tag)
	}

	return nil
}

// GetField parses of a field with Tag tag. Returned reject may indicate the field is not present, or the field value is invalid.
func (m FieldMap) getFieldNoLock(tag Tag, parser FieldValueReader) MessageRejectError {
	f, ok := m.lookup(tag)
	if !ok {
		return ConditionallyRequiredFieldMissing(tag)
	}

	if err := parser.Read(f[0].value); err != nil {
		return IncorrectDataFormatForValue(tag)
	}

	return nil
}

// GetBytes is a zero-copy GetField wrapper for []bytes fields.
func (m FieldMap) GetBytes(tag Tag) ([]byte, MessageRejectError) {
	m.rlock()
	defer m.runlock()

	f, ok := m.lookup(tag)
	if !ok {
		return nil, ConditionallyRequiredFieldMissing(tag)
	}

	return f[0].value, nil
}

// getBytesNoLock is a lock free zero-copy GetField wrapper for []bytes fields.
func (m FieldMap) getBytesNoLock(tag Tag) ([]byte, MessageRejectError) {
	f, ok := m.lookup(tag)
	if !ok {
		return nil, ConditionallyRequiredFieldMissing(tag)
	}

	return f[0].value, nil
}

// GetBool is a GetField wrapper for bool fields.
func (m FieldMap) GetBool(tag Tag) (bool, MessageRejectError) {
	var val FIXBoolean
	if err := m.GetField(tag, &val); err != nil {
		return false, err
	}
	return bool(val), nil
}

// GetInt is a GetField wrapper for int fields.
func (m FieldMap) GetInt(tag Tag) (int, MessageRejectError) {
	bytes, err := m.GetBytes(tag)
	if err != nil {
		return 0, err
	}

	var val FIXInt
	if val.Read(bytes) != nil {
		err = IncorrectDataFormatForValue(tag)
	}

	return int(val), err
}

// GetInt is a lock free GetField wrapper for int fields.
func (m FieldMap) getIntNoLock(tag Tag) (int, MessageRejectError) {
	bytes, err := m.getBytesNoLock(tag)
	if err != nil {
		return 0, err
	}

	var val FIXInt
	if val.Read(bytes) != nil {
		err = IncorrectDataFormatForValue(tag)
	}

	return int(val), err
}

// GetTime is a GetField wrapper for utc timestamp fields.
func (m FieldMap) GetTime(tag Tag) (t time.Time, err MessageRejectError) {
	m.rlock()
	defer m.runlock()

	bytes, err := m.GetBytes(tag)
	if err != nil {
		return
	}

	var val FIXUTCTimestamp
	if val.Read(bytes) != nil {
		err = IncorrectDataFormatForValue(tag)
	}

	return val.Time, err
}

// GetString is a GetField wrapper for string fields.
func (m FieldMap) GetString(tag Tag) (string, MessageRejectError) {
	var val FIXString
	if err := m.GetField(tag, &val); err != nil {
		return "", err
	}
	return string(val), nil
}

// GetString is a GetField wrapper for string fields.
func (m FieldMap) getStringNoLock(tag Tag) (string, MessageRejectError) {
	var val FIXString
	if err := m.getFieldNoLock(tag, &val); err != nil {
		return "", err
	}
	return string(val), nil
}

// GetGroup is a Get function specific to Group Fields.
func (m FieldMap) GetGroup(parser FieldGroupReader) MessageRejectError {
	m.rlock()
	defer m.runlock()

	f, ok := m.lookup(parser.Tag())
	if !ok {
		return ConditionallyRequiredFieldMissing(parser.Tag())
	}

	if _, err := parser.Read(f); err != nil {
		if msgRejErr, ok := err.(MessageRejectError); ok {
			return msgRejErr
		}
		return IncorrectDataFormatForValue(parser.Tag())
	}

	return nil
}

// SetField sets the field with Tag tag.
func (m *FieldMap) SetField(tag Tag, field FieldValueWriter) *FieldMap {
	return m.SetBytes(tag, field.Write())
}

// SetBytes sets bytes. It is an allocating compatibility API. A bounded map
// panics with ErrFieldCapacity before mutation if no field slot is available.
func (m *FieldMap) SetBytes(tag Tag, value []byte) *FieldMap {
	f := m.getOrCreate(tag)
	initField(f, tag, value)
	return m
}

// SetBool is a SetField wrapper for bool fields.
func (m *FieldMap) SetBool(tag Tag, value bool) *FieldMap {
	return m.SetField(tag, FIXBoolean(value))
}

// SetInt is a SetField wrapper for int fields.
func (m *FieldMap) SetInt(tag Tag, value int) *FieldMap {
	v := FIXInt(value)
	return m.SetBytes(tag, v.Write())
}

// SetString is a SetField wrapper for string fields.
func (m *FieldMap) SetString(tag Tag, value string) *FieldMap {
	return m.SetBytes(tag, []byte(value))
}

// Remove removes a tag from field map.
func (m *FieldMap) Remove(tag Tag) {
	if m.fixed != nil {
		m.fixedRemove(tag)
		return
	}
	m.lock()
	defer m.unlock()

	delete(m.tagLookup, tag)
}

// Clear purges all fields from field map.
func (m *FieldMap) Clear() {
	m.lock()
	defer m.unlock()
	m.clearNoLock()
}

func (m *FieldMap) clearNoLock() {
	if m.fixed != nil {
		m.fixedClear()
		return
	}
	m.tags = m.tags[0:0]
	for k := range m.tagLookup {
		delete(m.tagLookup, k)
	}
}

// CopyInto overwrites the given FieldMap with this one.
func (m *FieldMap) CopyInto(to *FieldMap) {
	m.rlock()
	defer m.runlock()
	if to.fixed != nil {
		if m.fixed == to.fixed {
			return
		}
		if m.fieldCount() > cap(to.fixed.tags) {
			panic(ErrFieldCapacity)
		}
		to.fixedClear()
		m.eachField(func(tag Tag, values field) {
			to.fixedGetOrCreate(tag)[0] = values[0]
		})
		to.compare = m.compare
		return
	}

	to.tagLookup = make(map[Tag]field)
	m.eachField(func(tag Tag, f field) {
		clone := make(field, 1)
		clone[0] = f[0]
		to.tagLookup[tag] = clone
	})
	to.tags = make([]Tag, len(m.fieldTags()))
	copy(to.tags, m.fieldTags())
	to.compare = m.compare
}

func (m *FieldMap) add(f field) {
	if !m.put(fieldTag(f), f) {
		panic(ErrFieldCapacity)
	}
}

func (m *FieldMap) getOrCreate(tag Tag) field {
	if m.fixed != nil {
		return m.fixedGetOrCreate(tag)
	}
	m.rwLock.Lock()
	defer m.rwLock.Unlock()

	if f, ok := m.tagLookup[tag]; ok {
		f = f[:1]
		return f
	}

	f := make(field, 1)
	m.tagLookup[tag] = f
	m.tags = append(m.tags, tag)
	return f
}

// Set is a setter for fields.
func (m *FieldMap) Set(field FieldWriter) *FieldMap {
	f := m.getOrCreate(field.Tag())
	initField(f, field.Tag(), field.Write())
	return m
}

// SetGroup is a setter specific to group fields.
func (m *FieldMap) SetGroup(field FieldGroupWriter) *FieldMap {
	m.lock()
	defer m.unlock()
	if m.fixed != nil {
		if _, found := m.lookup(field.Tag()); !found && m.fieldCount() == cap(m.fixed.tags) {
			panic(ErrFieldCapacity)
		}
	}
	if !m.put(field.Tag(), field.Write()) {
		panic(ErrFieldCapacity)
	}
	return m
}

func (m *FieldMap) sortedTags() []Tag {
	if m.fixed != nil {
		return m.fixedSortedTags()
	}
	sort.Sort(m)
	return m.tags
}

func (m FieldMap) write(buffer *bytes.Buffer) {
	m.lock()
	defer m.unlock()

	for _, tag := range m.sortedTags() {
		if f, ok := m.lookup(tag); ok {
			writeField(f, buffer)
		}
	}
}

func (m FieldMap) total() int {
	if m.fixed != nil {
		return m.fixed.total()
	}
	m.rwLock.RLock()
	defer m.rwLock.RUnlock()

	total := 0
	for _, fields := range m.tagLookup {
		for _, tv := range fields {
			switch tv.tag {
			case tagCheckSum: // Tag does not contribute to total.
			default:
				total += tv.total()
			}
		}
	}

	return total
}

func (m FieldMap) length() int {
	if m.fixed != nil {
		return m.fixed.length()
	}
	m.rwLock.RLock()
	defer m.rwLock.RUnlock()

	length := 0
	for _, fields := range m.tagLookup {
		for _, tv := range fields {
			switch tv.tag {
			case tagBeginString, tagBodyLength, tagCheckSum: // Tags do not contribute to length.
			default:
				length += tv.length()
			}
		}
	}

	return length
}
