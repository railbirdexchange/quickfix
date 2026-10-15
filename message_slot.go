package quickfix

import (
	"bytes"
	"errors"

	"github.com/quickfixgo/quickfix/datadictionary"
)

var (
	ErrFrameCapacity        = errors.New("quickfix: frame capacity exceeded")
	ErrGroupCapacity        = errors.New("quickfix: group entry capacity exceeded")
	ErrGroupDepth           = errors.New("quickfix: group nesting capacity exceeded")
	ErrInvalidParseLimits   = errors.New("quickfix: invalid parse limits")
	ErrInvalidUnsignedValue = errors.New("quickfix: invalid unsigned field value")
)

// ParseLimits bounds the storage and work of a MessageSlot. MaxFields includes
// header, trailer, group counts and every repeated field. MaxGroupEntries bounds
// both the sum of declared group counts and observed entry delimiters across
// all groups in the frame, including nested groups.
type ParseLimits struct {
	MaxFrameBytes   int
	MaxFields       int
	MaxGroupEntries int
	MaxGroupDepth   int
}

// DefaultParseLimits provides a 16 KiB frame, 512 fields, 128 total group entries
// and eight nested group levels. Applications may configure tighter limits or
// construct larger slots before starting their session.
func DefaultParseLimits() ParseLimits {
	return ParseLimits{MaxFrameBytes: 16 * 1024, MaxFields: 512, MaxGroupEntries: 128, MaxGroupDepth: 8}
}

// MessageSlot owns fixed-capacity parse indexes and borrows the input frame.
// Construct it before starting the session and transfer exclusive ownership of
// the slot and frame together. A slot is not safe for concurrent use.
//
// Message(), its field bytes and borrowed group views expire on the next Parse
// or Reset. The input frame must remain unchanged for that lifetime. Legacy
// convenience setters, string getters, CopyInto and RepeatingGroup readers may
// allocate; use byte getters and borrowed group views on the bounded path.
type MessageSlot struct {
	message             Message
	raw                 bytes.Buffer
	limits              ParseLimits
	transportDictionary *datadictionary.DataDictionary
	appDictionary       *datadictionary.DataDictionary
	valid               bool
}

// NewMessageSlot allocates all parse storage once. Dictionaries must remain
// immutable for the slot's lifetime. No parsing operation grows this storage.
func NewMessageSlot(transport, application *datadictionary.DataDictionary, limits ParseLimits) (*MessageSlot, error) {
	// Every field needs at least a tag, '=', and SOH. These relationships also
	// prevent capacity arithmetic overflow when constructing lookup tables.
	if limits.MaxFrameBytes < 12 || limits.MaxFields < 4 || limits.MaxFields > limits.MaxFrameBytes/3 || limits.MaxFields > int(^uint(0)>>1)/1024 ||
		limits.MaxGroupEntries < 0 || limits.MaxGroupEntries > limits.MaxFields ||
		limits.MaxGroupDepth < 0 || limits.MaxGroupDepth > limits.MaxFields {
		return nil, ErrInvalidParseLimits
	}
	slot := &MessageSlot{limits: limits, transportDictionary: transport, appDictionary: application}
	slot.message.Header.initFixed(headerFieldOrdering, limits.MaxFields)
	slot.message.Body.initFixed(normalFieldOrder, limits.MaxFields)
	slot.message.Trailer.initFixed(trailerFieldOrdering, limits.MaxFields)
	slot.message.fields = make([]TagValue, 0, limits.MaxFields)
	slot.message.groupStack = make([]*datadictionary.FieldDef, 0, limits.MaxGroupDepth)
	return slot, nil
}

// Parse borrows frame and parses it with the configured dictionaries. On any
// error Message returns nil; no session sequence or store state is changed.
// Parsing and subsequent dictionary validation are separate operations.
func (slot *MessageSlot) Parse(frame []byte) error {
	slot.valid = false
	if len(frame) > slot.limits.MaxFrameBytes {
		return ErrFrameCapacity
	}
	slot.raw = *bytes.NewBuffer(frame)
	slot.message.rawMessage = &slot.raw
	parser := msgParser{
		msg:                     &slot.message,
		transportDataDictionary: slot.transportDictionary,
		appDataDictionary:       slot.appDictionary,
		rawBytes:                frame,
		limits:                  &slot.limits,
	}
	if err := doParsing(&parser); err != nil {
		return err
	}
	slot.valid = true
	return nil
}

// Message returns a borrowed message after successful parsing, otherwise nil.
func (slot *MessageSlot) Message() *Message {
	if !slot.valid {
		return nil
	}
	return &slot.message
}

// Group returns a borrowed dictionary-backed body group. It does not construct
// mutable Group objects or clone generated templates. Validate the message
// before invoking application handlers, as with the legacy API.
func (slot *MessageSlot) Group(tag Tag) (BorrowedGroup, error) {
	if !slot.valid || slot.appDictionary == nil {
		return BorrowedGroup{}, ErrBorrowedGroupNotFound
	}
	typeField, found := slot.message.Header.lookup(tagMsgType)
	if !found || len(typeField) == 0 {
		return BorrowedGroup{}, ErrBorrowedGroupNotFound
	}
	definition := slot.appDictionary.Messages[string(typeField[0].value)]
	if definition == nil {
		return BorrowedGroup{}, ErrBorrowedGroupNotFound
	}
	values, found := slot.message.Body.lookup(tag)
	if !found {
		return BorrowedGroup{}, ErrBorrowedGroupNotFound
	}
	return newBorrowedGroup(values, definition.Fields[int(tag)])
}

// boundedUnsigned scans a nonnegative decimal without overflowing or allocating.
func boundedUnsigned(value []byte, limit int, capacityError error) (int, error) {
	if len(value) == 0 {
		return 0, ErrInvalidUnsignedValue
	}
	count := 0
	exceeded := false
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, ErrInvalidUnsignedValue
		}
		digit := int(c - '0')
		if exceeded {
			continue
		}
		if count > limit/10 || count == limit/10 && digit > limit%10 {
			exceeded = true
			continue
		}
		count = count*10 + digit
	}
	if exceeded {
		return 0, capacityError
	}
	return count, nil
}

// Reset ends all borrowed lifetimes and releases references to the input frame.
// It retains the slot's fixed storage for reuse.
func (slot *MessageSlot) Reset() {
	slot.valid = false
	slot.message.Header.clearNoLock()
	slot.message.Body.clearNoLock()
	slot.message.Trailer.clearNoLock()
	clear(slot.message.fields[:cap(slot.message.fields)])
	slot.message.fields = slot.message.fields[:0]
	clear(slot.message.groupStack[:cap(slot.message.groupStack)])
	slot.message.groupStack = slot.message.groupStack[:0]
	slot.message.bodyBytes = nil
	slot.message.rawMessage = nil
	slot.raw = bytes.Buffer{}
}
