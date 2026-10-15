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
	"fmt"
	"math"
	"time"

	"github.com/quickfixgo/quickfix/datadictionary"
)

// Header is first section of a FIX Message.
type Header struct{ FieldMap }

// msgparser contains message parsing vars needed to parse a string into a message.
type msgParser struct {
	msg                     *Message
	transportDataDictionary *datadictionary.DataDictionary
	appDataDictionary       *datadictionary.DataDictionary
	rawBytes                []byte
	fieldIndex              int
	parsedFieldBytes        *TagValue
	trailerBytes            []byte
	foundBody               bool
	foundTrailer            bool
	messageDef              *datadictionary.MessageDef
	limits                  *ParseLimits
	declaredGroupEntries    int
	actualGroupEntries      int
}

// in the message header, the first 3 tags in the message header must be 8,9,35.
func headerFieldOrdering(i, j Tag) bool {
	var ordering = func(t Tag) uint32 {
		switch t {
		case tagBeginString:
			return 1
		case tagBodyLength:
			return 2
		case tagMsgType:
			return 3
		}

		return math.MaxUint32
	}

	orderi := ordering(i)
	orderj := ordering(j)

	switch {
	case orderi < orderj:
		return true
	case orderi > orderj:
		return false
	}

	return i < j
}

// Init initializes the Header instance.
func (h *Header) Init() {
	h.initWithOrdering(headerFieldOrdering)
}

// Body is the primary application section of a FIX message.
type Body struct{ FieldMap }

// Init initializes the FIX message.
func (b *Body) Init() {
	b.init()
}

// Trailer is the last section of a FIX message.
type Trailer struct{ FieldMap }

// In the trailer, CheckSum (tag 10) must be last.
func trailerFieldOrdering(i, j Tag) bool {
	switch {
	case i == tagCheckSum:
		return false
	case j == tagCheckSum:
		return true
	}

	return i > j
}

// Init initializes the FIX message.
func (t *Trailer) Init() {
	t.initWithOrdering(trailerFieldOrdering)
}

// Message is a FIX Message abstraction.
type Message struct {
	Header  Header
	Trailer Trailer
	Body    Body

	// ReceiveTime is the time that this message was read from the socket connection.
	ReceiveTime time.Time

	rawMessage *bytes.Buffer

	// Slice of Bytes corresponding to the message body.
	bodyBytes []byte

	// Field bytes as they appear in the raw message.
	fields     []TagValue
	groupStack []*datadictionary.FieldDef
}

// ToMessage returns the message itself.
func (m *Message) ToMessage() *Message { return m }

// parseError is returned when bytes cannot be parsed as a FIX message.
type parseError struct {
	OrigError string
}

func (e parseError) Error() string { return fmt.Sprintf("error parsing message: %s", e.OrigError) }

// NewMessage returns a newly initialized Message instance.
func NewMessage() *Message {
	m := new(Message)
	m.Header.Init()
	m.Body.Init()
	m.Trailer.Init()

	return m
}

// CopyInto erases the dest messages and copies the currency message content
// into it.
func (m *Message) CopyInto(to *Message) {
	m.Header.CopyInto(&to.Header.FieldMap)
	m.Body.CopyInto(&to.Body.FieldMap)
	m.Trailer.CopyInto(&to.Trailer.FieldMap)

	to.ReceiveTime = m.ReceiveTime
	to.bodyBytes = make([]byte, len(m.bodyBytes))
	copy(to.bodyBytes, m.bodyBytes)
	to.fields = make([]TagValue, len(m.fields))
	for i := range to.fields {
		to.fields[i].init(m.fields[i].tag, m.fields[i].value)
	}
}

// ParseMessage constructs a Message from a byte slice wrapping a FIX message.
func ParseMessage(msg *Message, rawMessage *bytes.Buffer) (err error) {
	return ParseMessageWithDataDictionary(msg, rawMessage, nil, nil)
}

// ParseMessageWithDataDictionary constructs a Message from a byte slice wrapping a FIX message using an optional session and application DataDictionary for reference.
func ParseMessageWithDataDictionary(
	msg *Message,
	rawMessage *bytes.Buffer,
	transportDataDictionary *datadictionary.DataDictionary,
	appDataDictionary *datadictionary.DataDictionary,
) (err error) {
	// Create msgparser before we go any further.
	mp := &msgParser{
		msg:                     msg,
		transportDataDictionary: transportDataDictionary,
		appDataDictionary:       appDataDictionary,
	}
	mp.msg.rawMessage = rawMessage
	mp.rawBytes = rawMessage.Bytes()

	return doParsing(mp)
}

// doParsing executes the message parsing process.
func doParsing(mp *msgParser) (err error) {
	mp.msg.Header.lock()
	defer mp.msg.Header.unlock()
	mp.msg.Body.lock()
	defer mp.msg.Body.unlock()
	mp.msg.Trailer.lock()
	defer mp.msg.Trailer.unlock()

	// Initialize for parsing.
	mp.msg.Header.clearNoLock()
	mp.msg.Body.clearNoLock()
	mp.msg.Trailer.clearNoLock()
	mp.msg.bodyBytes = nil
	mp.msg.groupStack = mp.msg.groupStack[:0]

	// Allocate expected message fields in one chunk.
	fieldCount := bytes.Count(mp.rawBytes, []byte{'\001'})
	if mp.limits != nil {
		// A binary XMLData value may itself contain SOH bytes. Bound actual
		// extracted fields instead of treating those data bytes as fields.
		fieldCount = cap(mp.msg.fields)
	}
	if fieldCount == 0 {
		return parseError{OrigError: fmt.Sprintf("No Fields detected in %s", string(mp.rawBytes))}
	}
	if fieldCount < 4 {
		return parseError{OrigError: "Incomplete message"}
	}
	if cap(mp.msg.fields) < fieldCount {
		if mp.limits != nil {
			return ErrFieldCapacity
		}
		mp.msg.fields = make([]TagValue, fieldCount)
	} else {
		mp.msg.fields = mp.msg.fields[0:fieldCount]
	}

	// Message must start with begin string, body length, msg type.
	// Get begin string.
	if mp.rawBytes, err = extractSpecificField(&mp.msg.fields[mp.fieldIndex], tagBeginString, mp.rawBytes); err != nil {
		return
	}
	mp.msg.Header.add(mp.msg.fields[mp.fieldIndex : mp.fieldIndex+1])

	// Get body length.
	mp.fieldIndex++
	mp.parsedFieldBytes = &mp.msg.fields[mp.fieldIndex]
	if mp.rawBytes, err = extractSpecificField(mp.parsedFieldBytes, tagBodyLength, mp.rawBytes); err != nil {
		return
	}
	mp.msg.Header.add(mp.msg.fields[mp.fieldIndex : mp.fieldIndex+1])

	// Get msg type.
	mp.fieldIndex++
	mp.parsedFieldBytes = &mp.msg.fields[mp.fieldIndex]
	if mp.rawBytes, err = extractSpecificField(mp.parsedFieldBytes, tagMsgType, mp.rawBytes); err != nil {
		return
	}
	mp.msg.Header.add(mp.msg.fields[mp.fieldIndex : mp.fieldIndex+1])

	// Start parsing.
	dictionary := mp.appDataDictionary
	if mp.limits != nil {
		dictionary = boundedMessageDictionary(mp.transportDataDictionary, dictionary, mp.parsedFieldBytes.value)
	}
	if dictionary != nil {
		// A transient byte-to-string map key does not copy the frame's bytes.
		mp.messageDef = dictionary.Messages[string(mp.parsedFieldBytes.value)]
	}
	mp.msg.bodyBytes = mp.rawBytes
	mp.fieldIndex++
	xmlDataLen := 0
	xmlDataMsg := false
	mp.trailerBytes = []byte{}
	mp.foundBody = false
	mp.foundTrailer = false
	for {
		if mp.fieldIndex >= len(mp.msg.fields) {
			if mp.limits != nil && len(mp.rawBytes) > 0 {
				return ErrFieldCapacity
			}
			return parseError{OrigError: "Missing CheckSum field"}
		}
		mp.parsedFieldBytes = &mp.msg.fields[mp.fieldIndex]
		if xmlDataLen > 0 {
			mp.rawBytes, err = extractXMLDataField(mp.parsedFieldBytes, mp.rawBytes, xmlDataLen)
			xmlDataLen = 0
			xmlDataMsg = true
		} else {
			mp.rawBytes, err = extractField(mp.parsedFieldBytes, mp.rawBytes)
		}
		if err != nil {
			return
		}

		switch {
		case isHeaderField(mp.parsedFieldBytes.tag, mp.transportDataDictionary):
			if group := mp.sectionGroup(mp.parsedFieldBytes.tag, true); group != nil {
				if err := parseGroup(mp, group, &mp.msg.Header.FieldMap); err != nil {
					return err
				}
			} else {
				mp.msg.Header.add(mp.msg.fields[mp.fieldIndex : mp.fieldIndex+1])
			}
		case isTrailerField(mp.parsedFieldBytes.tag, mp.transportDataDictionary):
			if group := mp.sectionGroup(mp.parsedFieldBytes.tag, false); group != nil {
				if err := parseGroup(mp, group, &mp.msg.Trailer.FieldMap); err != nil {
					return err
				}
			} else {
				mp.msg.Trailer.add(mp.msg.fields[mp.fieldIndex : mp.fieldIndex+1])
			}
			mp.foundTrailer = true
		case mp.groupDefinition(mp.parsedFieldBytes.tag) != nil:
			if err = parseGroup(mp, mp.groupDefinition(mp.parsedFieldBytes.tag), &mp.msg.Body.FieldMap); err != nil {
				return err
			}
		default:
			mp.foundBody = true
			mp.trailerBytes = mp.rawBytes
			mp.msg.Body.add(mp.msg.fields[mp.fieldIndex : mp.fieldIndex+1])
		}
		if mp.parsedFieldBytes.tag == tagCheckSum {
			break
		}

		if !mp.foundBody {
			mp.msg.bodyBytes = mp.rawBytes
		}

		if mp.parsedFieldBytes.tag == tagXMLDataLen {
			if mp.limits != nil {
				xmlDataLen, err = boundedUnsigned(mp.parsedFieldBytes.value, len(mp.rawBytes), ErrFrameCapacity)
				if err != nil {
					return err
				}
			} else {
				xmlDataLen, _ = mp.msg.Header.getIntNoLock(tagXMLDataLen)
			}
		}
		mp.fieldIndex++
	}
	if mp.limits != nil {
		if len(mp.rawBytes) != 0 {
			return parseError{OrigError: "Fields after CheckSum"}
		}
		mp.msg.fields = mp.msg.fields[:mp.fieldIndex+1]
	}

	// This will happen if there are no fields in the body
	if mp.foundTrailer && !mp.foundBody {
		mp.trailerBytes = mp.rawBytes
		mp.msg.bodyBytes = nil
	}

	// Body length would only be larger than trailer if fields out of order.
	if len(mp.msg.bodyBytes) > len(mp.trailerBytes) {
		mp.msg.bodyBytes = mp.msg.bodyBytes[:len(mp.msg.bodyBytes)-len(mp.trailerBytes)]
	}

	length := 0
	for _, field := range mp.msg.fields {
		switch field.tag {
		case tagBeginString, tagBodyLength, tagCheckSum: // Tags do not contribute to length.
		default:
			length += field.length()
		}
	}

	var bodyLength int
	if mp.limits != nil {
		bodyLength, err = boundedUnsigned(mp.msg.fields[1].value, mp.limits.MaxFrameBytes, ErrFrameCapacity)
	} else {
		bodyLength, err = mp.msg.Header.getIntNoLock(tagBodyLength)
	}
	if err != nil {
		if mp.limits == nil {
			return parseError{OrigError: err.Error()}
		}
		return err
	} else if length != bodyLength && !xmlDataMsg {
		err = parseError{OrigError: fmt.Sprintf("Incorrect Message Length, expected %d, got %d", bodyLength, length)}
	}

	return
}

// parseGroup iterates through a repeating group to maintain correct order of those fields.
func parseGroup(mp *msgParser, definition *datadictionary.FieldDef, section *FieldMap) error {
	bodyGroup := section == &mp.msg.Body.FieldMap
	if bodyGroup {
		mp.foundBody = true
	}
	start := mp.fieldIndex
	mp.msg.groupStack = mp.msg.groupStack[:0]
	if err := mp.pushGroup(definition); err != nil {
		return err
	}

	for {
		mp.fieldIndex++
		if mp.fieldIndex >= len(mp.msg.fields) {
			if mp.limits != nil && len(mp.rawBytes) > 0 {
				return ErrFieldCapacity
			}
			return parseError{OrigError: "Missing CheckSum field"}
		}
		mp.parsedFieldBytes = &mp.msg.fields[mp.fieldIndex]
		preExtractBytes := mp.rawBytes
		var err error
		mp.rawBytes, err = extractField(mp.parsedFieldBytes, mp.rawBytes)
		if err != nil {
			return err
		}
		if bodyGroup {
			mp.trailerBytes = mp.rawBytes
		}

		// Search the innermost group first, then its ancestors. Each group
		// keeps its original contiguous range in the message's field slab.
		var child *datadictionary.FieldDef
		for depth := len(mp.msg.groupStack); depth > 0; depth-- {
			parent := mp.msg.groupStack[depth-1]
			if child = parent.Child(int(mp.parsedFieldBytes.tag)); child != nil {
				mp.msg.groupStack = mp.msg.groupStack[:depth]
				if mp.limits != nil && parent.Fields[0].Tag() == int(mp.parsedFieldBytes.tag) {
					mp.actualGroupEntries++
					if mp.actualGroupEntries > mp.limits.MaxGroupEntries {
						return ErrGroupCapacity
					}
				}
				break
			}
		}
		if child != nil {
			if child.IsGroup() {
				if err := mp.pushGroup(child); err != nil {
					return err
				}
			}
			continue
		}

		section.add(mp.msg.fields[start:mp.fieldIndex])
		if !bodyGroup || mp.limits != nil {
			// The normal loop classifies the first field outside this header or
			// trailer group. Rewind that field without moving/copying its bytes.
			mp.fieldIndex--
			mp.parsedFieldBytes = &mp.msg.fields[mp.fieldIndex]
			mp.rawBytes = preExtractBytes
			if bodyGroup {
				mp.trailerBytes = preExtractBytes
			}
			return nil
		}
		if isHeaderField(mp.parsedFieldBytes.tag, mp.transportDataDictionary) {
			mp.msg.Header.add(mp.msg.fields[mp.fieldIndex : mp.fieldIndex+1])
		} else if isTrailerField(mp.parsedFieldBytes.tag, mp.transportDataDictionary) {
			mp.msg.Trailer.add(mp.msg.fields[mp.fieldIndex : mp.fieldIndex+1])
			mp.foundTrailer = true
			// The original body ends before the trailer, including on resend.
			mp.trailerBytes = preExtractBytes
		} else if next := mp.groupDefinition(mp.parsedFieldBytes.tag); next != nil {
			start = mp.fieldIndex
			mp.msg.groupStack = mp.msg.groupStack[:0]
			if err := mp.pushGroup(next); err != nil {
				return err
			}
			continue
		} else {
			mp.msg.Body.add(mp.msg.fields[mp.fieldIndex : mp.fieldIndex+1])
		}
		return nil
	}
}

func (mp *msgParser) groupDefinition(tag Tag) *datadictionary.FieldDef {
	if mp.messageDef != nil {
		if field := mp.messageDef.Fields[int(tag)]; field != nil && field.IsGroup() {
			return field
		}
	}
	return nil
}

func (mp *msgParser) sectionGroup(tag Tag, header bool) *datadictionary.FieldDef {
	if mp.limits == nil || mp.transportDataDictionary == nil {
		return nil
	}
	section := mp.transportDataDictionary.Trailer
	if header {
		section = mp.transportDataDictionary.Header
	}
	if section != nil {
		if definition := section.Fields[int(tag)]; definition != nil && definition.IsGroup() {
			return definition
		}
	}
	return nil
}

func (mp *msgParser) pushGroup(definition *datadictionary.FieldDef) error {
	if mp.limits != nil {
		if len(mp.msg.groupStack) == cap(mp.msg.groupStack) {
			return ErrGroupDepth
		}
		count, err := boundedUnsigned(mp.parsedFieldBytes.value, mp.limits.MaxGroupEntries-mp.declaredGroupEntries, ErrGroupCapacity)
		if err != nil && err != ErrInvalidUnsignedValue {
			return err
		}
		// Invalid count syntax belongs to dictionary validation, which supplies
		// the existing FIX reject reason/tag. Actual fields and delimiters are
		// still bounded even when the declared count is malformed.
		mp.declaredGroupEntries += count
	}
	mp.msg.groupStack = append(mp.msg.groupStack, definition)
	return nil
}

func isHeaderField(tag Tag, dataDict *datadictionary.DataDictionary) bool {
	if tag.IsHeader() {
		return true
	}

	if dataDict == nil || dataDict.Header == nil {
		return false
	}

	_, ok := dataDict.Header.Fields[int(tag)]
	return ok
}

func isTrailerField(tag Tag, dataDict *datadictionary.DataDictionary) bool {
	if tag.IsTrailer() {
		return true
	}

	if dataDict == nil || dataDict.Trailer == nil {
		return false
	}

	_, ok := dataDict.Trailer.Fields[int(tag)]
	return ok
}

// MsgType returns MsgType (tag 35) field's value.
func (m *Message) MsgType() (string, MessageRejectError) {
	return m.Header.GetString(tagMsgType)
}

func (m *Message) msgTypeNoLock() (string, MessageRejectError) {
	return m.Header.getStringNoLock(tagMsgType)
}

// IsMsgTypeOf returns true if the Header contains MsgType (tag 35) field and its value is the specified one.
func (m *Message) IsMsgTypeOf(msgType string) bool {
	if v, err := m.MsgType(); err == nil {
		return v == msgType
	}
	return false
}

// reverseRoute returns a message builder with routing header fields initialized as the reverse of this message.
func (m *Message) reverseRoute() *Message {
	reverseMsg := NewMessage()

	copyFunc := func(src Tag, dest Tag) {
		var field FIXString
		if m.Header.GetField(src, &field) == nil {
			if len(field) != 0 {
				reverseMsg.Header.SetField(dest, field)
			}
		}
	}

	copyFunc(tagSenderCompID, tagTargetCompID)
	copyFunc(tagSenderSubID, tagTargetSubID)
	copyFunc(tagSenderLocationID, tagTargetLocationID)

	copyFunc(tagTargetCompID, tagSenderCompID)
	copyFunc(tagTargetSubID, tagSenderSubID)
	copyFunc(tagTargetLocationID, tagSenderLocationID)

	copyFunc(tagOnBehalfOfCompID, tagDeliverToCompID)
	copyFunc(tagOnBehalfOfSubID, tagDeliverToSubID)
	copyFunc(tagDeliverToCompID, tagOnBehalfOfCompID)
	copyFunc(tagDeliverToSubID, tagOnBehalfOfSubID)

	// Tags added in 4.1.
	var beginString FIXString
	if m.Header.GetField(tagBeginString, &beginString) == nil {
		if string(beginString) != BeginStringFIX40 {
			copyFunc(tagOnBehalfOfLocationID, tagDeliverToLocationID)
			copyFunc(tagDeliverToLocationID, tagOnBehalfOfLocationID)
		}
	}

	return reverseMsg
}

func extractSpecificField(field *TagValue, expectedTag Tag, buffer []byte) (remBuffer []byte, err error) {
	remBuffer, err = extractField(field, buffer)
	switch {
	case err != nil:
		return
	case field.tag != expectedTag:
		err = parseError{OrigError: fmt.Sprintf("extractSpecificField: Fields out of order, expected %d, got %d", expectedTag, field.tag)}
		return
	}

	return
}

func extractXMLDataField(parsedFieldBytes *TagValue, buffer []byte, dataLen int) (remBytes []byte, err error) {
	endIndex := bytes.IndexByte(buffer, '=')
	if endIndex == -1 {
		err = parseError{OrigError: "extractField: No Trailing Delim in " + string(buffer)}
		remBytes = buffer
		return
	}
	if dataLen < 0 || dataLen >= len(buffer)-endIndex-1 {
		return buffer, parseError{OrigError: "Incomplete XMLData field"}
	}
	endIndex += dataLen + 1

	err = parsedFieldBytes.parse(buffer[:endIndex+1])
	return buffer[(endIndex + 1):], err
}

func extractField(parsedFieldBytes *TagValue, buffer []byte) (remBytes []byte, err error) {
	endIndex := bytes.IndexByte(buffer, '\001')
	if endIndex == -1 {
		err = parseError{OrigError: "extractField: No Trailing Delim in " + string(buffer)}
		remBytes = buffer
		return
	}

	err = parsedFieldBytes.parse(buffer[:endIndex+1])
	return buffer[(endIndex + 1):], err
}

func (m *Message) Bytes() []byte {
	if m.rawMessage != nil {
		return m.rawMessage.Bytes()
	}

	return m.build()
}

func (m *Message) String() string {
	if m.rawMessage != nil {
		return m.rawMessage.String()
	}

	return string(m.build())
}

func formatCheckSum(value int) string {
	if value < 0 || value > 255 {
		return fmt.Sprintf("%03d", value)
	}
	return string([]byte{byte(value/100) + '0', byte(value/10%10) + '0', byte(value%10) + '0'})
}

// Build constructs a []byte from a Message instance.
func (m *Message) build() []byte {
	m.cook(m.Body.length(), m.Body.total())

	dst := make([]byte, 0, m.Header.serializedLength()+m.Body.serializedLength()+m.Trailer.serializedLength())
	dst = m.Header.appendLegacy(dst)
	dst = m.Body.appendLegacy(dst)
	return m.Trailer.appendLegacy(dst)
}

// Constructs a []byte from a Message instance, using the given bodyBytes.
// This is a workaround for the fact that we currently rely on the generated Message types to properly serialize/deserialize RepeatingGroups.
// In other words, we cannot go from bytes to a Message then back to bytes, which is exactly what we need to do in the case of a Resend.
// This func lets us pull the Message from the Store, parse it, update the Header, and then build it back into bytes using the original Body.
// Note: The only standard non-Body group is NoHops.  If that is used in the Header, this workaround may fail.
func (m *Message) buildWithBodyBytes(bodyBytes []byte) []byte {
	m.cook(len(bodyBytes), bytesTotal(bodyBytes))

	dst := make([]byte, 0, m.Header.serializedLength()+len(bodyBytes)+m.Trailer.serializedLength())
	dst = m.Header.appendLegacy(dst)
	dst = append(dst, bodyBytes...)
	return m.Trailer.appendLegacy(dst)
}

func (m *Message) cook(bodyLen, bodyTotal int) {
	bodyLength := m.Header.length() + bodyLen + m.Trailer.length()
	m.Header.SetInt(tagBodyLength, bodyLength)
	checkSum := (m.Header.total() + bodyTotal + m.Trailer.total()) % 256
	m.Trailer.SetString(tagCheckSum, formatCheckSum(checkSum))
}
