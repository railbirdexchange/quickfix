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
	scopedFields        [3]FieldMap
	parseFields         []TagValue
	parseGroupStack     []*datadictionary.FieldDef
	fieldsUsed          int
}

// NewMessageSlot allocates all parse storage once. Dictionaries must remain
// immutable for the slot's lifetime. No parsing operation grows this storage.
func NewMessageSlot(transport, application *datadictionary.DataDictionary, limits ParseLimits) (*MessageSlot, error) {
	if err := validateParseLimits(limits); err != nil {
		return nil, err
	}
	slot := &MessageSlot{limits: limits, transportDictionary: transport, appDictionary: application}
	slot.message.Header.initFixed(headerFieldOrdering, limits.MaxFields)
	slot.message.Body.initFixed(normalFieldOrder, limits.MaxFields)
	slot.message.Trailer.initFixed(trailerFieldOrdering, limits.MaxFields)
	slot.message.fields = make([]TagValue, 0, limits.MaxFields)
	slot.message.groupStack = make([]*datadictionary.FieldDef, 0, limits.MaxGroupDepth)
	slot.parseFields = slot.message.fields
	slot.parseGroupStack = slot.message.groupStack
	slot.message.parseSlot = slot
	return slot, nil
}

func validateParseLimits(limits ParseLimits) error {
	// Every field needs at least a tag, '=', and SOH. These relationships also
	// prevent capacity arithmetic overflow when constructing lookup tables.
	if limits.MaxFrameBytes < 12 || limits.MaxFields < 4 || limits.MaxFields > limits.MaxFrameBytes/3 || limits.MaxFields > int(^uint(0)>>1)/1024 ||
		limits.MaxGroupEntries < 0 || limits.MaxGroupEntries > limits.MaxFields ||
		limits.MaxGroupDepth < 0 || limits.MaxGroupDepth > limits.MaxFields {
		return ErrInvalidParseLimits
	}
	return nil
}

// enableScopedCallbacks is session-only compatibility setup. Public message
// slots remain strict. Save the original storage before any callback can detach
// a section through allocating setters or replace fields through CopyInto.
func (slot *MessageSlot) enableScopedCallbacks() {
	if slot.scopedFields[0].fixed != nil {
		return
	}
	slot.message.Header.scoped = true
	slot.message.Body.scoped = true
	slot.message.Trailer.scoped = true
	slot.scopedFields = [3]FieldMap{slot.message.Header.FieldMap, slot.message.Body.FieldMap, slot.message.Trailer.FieldMap}
}

func (slot *MessageSlot) restoreScopedFields() {
	// CopyInto is an allocating convenience operation for every MessageSlot.
	// Keep its replacement slices separate from the original parse storage.
	slot.message.fields = slot.parseFields
	slot.message.groupStack = slot.parseGroupStack
	slot.message.parseSlot = slot
	if slot.scopedFields[0].fixed == nil {
		return
	}
	slot.message.Header.FieldMap = slot.scopedFields[0]
	slot.message.Body.FieldMap = slot.scopedFields[1]
	slot.message.Trailer.FieldMap = slot.scopedFields[2]
}

// Parse borrows frame and parses it with the configured dictionaries. On any
// error Message returns nil; no session sequence or store state is changed.
// Parsing and subsequent dictionary validation are separate operations.
func (slot *MessageSlot) Parse(frame []byte) error {
	slot.valid = false
	slot.restoreScopedFields()
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
	err := doParsing(&parser)
	// A group can extract one boundary field and rewind before an error. Include
	// that lookahead and retain the high-water mark across shorter parses so no
	// previously extracted field can hide references to an earlier frame.
	slot.fieldsUsed = max(slot.fieldsUsed, min(parser.fieldIndex+2, cap(slot.message.fields)))
	if err != nil {
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
	if !slot.valid {
		return BorrowedGroup{}, ErrBorrowedGroupNotFound
	}
	return slot.message.BorrowGroup(tag)
}

// BorrowGroup returns a read-only body group backed by this message's parsed
// fields. It is available on MessageSlot messages, including BorrowApplication
// callbacks, without constructing or copying legacy Group objects. The view
// expires on message mutation, slot reset/reparse, or callback return. Consume
// it synchronously; use GetGroup when an independently owned group is needed.
// Messages without active dictionary-backed parse storage return
// ErrBorrowedGroupNotFound. Dictionary validation must still precede handling.
func (message *Message) BorrowGroup(tag Tag) (BorrowedGroup, error) {
	slot := message.parseSlot
	if slot == nil || !slot.valid {
		return BorrowedGroup{}, ErrBorrowedGroupNotFound
	}
	typeField, found := message.Header.lookup(tagMsgType)
	if !found || len(typeField) == 0 {
		return BorrowedGroup{}, ErrBorrowedGroupNotFound
	}
	dictionary := boundedMessageDictionary(slot.transportDictionary, slot.appDictionary, typeField[0].value)
	if dictionary == nil {
		return BorrowedGroup{}, ErrBorrowedGroupNotFound
	}
	definition := dictionary.Messages[string(typeField[0].value)]
	if definition == nil {
		return BorrowedGroup{}, ErrBorrowedGroupNotFound
	}
	values, found := message.Body.lookup(tag)
	if !found {
		return BorrowedGroup{}, ErrBorrowedGroupNotFound
	}
	return newBorrowedGroup(values, definition.Fields[int(tag)])
}

func boundedMessageDictionary(transport, application *datadictionary.DataDictionary, msgType []byte) *datadictionary.DataDictionary {
	if transport != nil && isAdminMessageType(msgType) {
		return transport
	}
	return application
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
	slot.restoreScopedFields()
	slot.message.Header.clearNoLock()
	slot.message.Body.clearNoLock()
	slot.message.Trailer.clearNoLock()
	clear(slot.message.fields[:slot.fieldsUsed])
	slot.fieldsUsed = 0
	slot.message.fields = slot.message.fields[:0]
	clear(slot.message.groupStack[:cap(slot.message.groupStack)])
	slot.message.groupStack = slot.message.groupStack[:0]
	slot.message.bodyBytes = nil
	slot.message.rawMessage = nil
	slot.message.inbound = nil
	slot.raw = bytes.Buffer{}
}
