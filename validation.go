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

import "github.com/quickfixgo/quickfix/datadictionary"

const (
	UserDefinedTagMin int = 5000
)

// Validator validates a FIX message.
type Validator interface {
	Validate(*Message) MessageRejectError
}

// ValidatorSettings describe validation behavior.
type ValidatorSettings struct {
	CheckFieldsOutOfOrder     bool
	RejectInvalidMessage      bool
	AllowUnknownMessageFields bool
	CheckUserDefinedFields    bool
	CheckFieldsHaveValues     bool
}

// Default configuration for message validation.
// See http://www.quickfixengine.org/quickfix/doc/html/configuration.html.
var defaultValidatorSettings = ValidatorSettings{
	CheckFieldsOutOfOrder:     true,
	CheckFieldsHaveValues:     true,
	RejectInvalidMessage:      true,
	AllowUnknownMessageFields: false,
	CheckUserDefinedFields:    true,
}

type fixValidator struct {
	dataDictionary *datadictionary.DataDictionary
	settings       ValidatorSettings
}

type fixtValidator struct {
	transportDataDictionary *datadictionary.DataDictionary
	appDataDictionary       *datadictionary.DataDictionary
	settings                ValidatorSettings
}

// NewValidator creates a FIX message validator from the given data dictionaries.
func NewValidator(settings ValidatorSettings, appDataDictionary, transportDataDictionary *datadictionary.DataDictionary) Validator {
	if transportDataDictionary != nil {
		return &fixtValidator{
			transportDataDictionary: transportDataDictionary,
			appDataDictionary:       appDataDictionary,
			settings:                settings,
		}
	}
	return &fixValidator{
		dataDictionary: appDataDictionary,
		settings:       settings,
	}
}

// Validate tests the message against the provided data dictionary.
func (v *fixValidator) Validate(msg *Message) MessageRejectError {
	if !msg.Header.Has(tagMsgType) {
		return RequiredTagMissing(tagMsgType)
	}
	msgType, err := msg.Header.GetBytes(tagMsgType)
	if err != nil {
		return err
	}

	return validateFIX(v.dataDictionary, v.settings, string(msgType), msg)
}

// Validate tests the message against the provided transport and app data dictionaries.
// If the message is an admin message, it will be validated against the transport data dictionary.
func (v *fixtValidator) Validate(msg *Message) MessageRejectError {
	if !msg.Header.Has(tagMsgType) {
		return RequiredTagMissing(tagMsgType)
	}
	msgType, err := msg.Header.GetBytes(tagMsgType)
	if err != nil {
		return err
	}

	if isAdminMessageType(msgType) {
		return validateFIX(v.transportDataDictionary, v.settings, string(msgType), msg)
	}
	return validateFIXT(v.transportDataDictionary, v.appDataDictionary, v.settings, string(msgType), msg)
}

func validateFIX(d *datadictionary.DataDictionary, settings ValidatorSettings, msgType string, msg *Message) MessageRejectError {
	return validateFIXWithScratch(d, settings, msgType, msg, nil, nil)
}

func validateFIXWithScratch(d *datadictionary.DataDictionary, settings ValidatorSettings, msgType string, msg *Message, scratch *validationTagSet, required *validationRequiredMetadata) MessageRejectError {
	if d != nil {
		if err := validateMsgType(d, msgType, msg); err != nil {
			return err
		}

		if err := validateRequired(d, d, msgType, msg, required); err != nil {
			return err
		}
	}

	if err := validateFieldContent(msg, settings.CheckFieldsHaveValues, settings.CheckFieldsOutOfOrder, required); err != nil {
		return err
	}

	if settings.RejectInvalidMessage && d != nil {
		if err := validateFields(d, d, settings, msg, required); err != nil {
			return err
		}

		if err := validateWalkWithScratch(d, d, settings, msgType, msg, scratch, required); err != nil {
			return err
		}
	}

	return nil
}

func validateFIXT(transportDD, appDD *datadictionary.DataDictionary, settings ValidatorSettings, msgType string, msg *Message) MessageRejectError {
	return validateFIXTWithScratch(transportDD, appDD, settings, msgType, msg, nil, nil)
}

func validateFIXTWithScratch(transportDD, appDD *datadictionary.DataDictionary, settings ValidatorSettings, msgType string, msg *Message, scratch *validationTagSet, required *validationRequiredMetadata) MessageRejectError {
	if appDD != nil && transportDD != nil {
		if err := validateMsgType(appDD, msgType, msg); err != nil {
			return err
		}

		if err := validateRequired(transportDD, appDD, msgType, msg, required); err != nil {
			return err
		}
	}

	if err := validateFieldContent(msg, settings.CheckFieldsHaveValues, settings.CheckFieldsOutOfOrder, required); err != nil {
		return err
	}

	if settings.RejectInvalidMessage && appDD != nil && transportDD != nil {
		if err := validateFields(transportDD, appDD, settings, msg, required); err != nil {
			return err
		}

		if err := validateWalkWithScratch(transportDD, appDD, settings, msgType, msg, scratch, required); err != nil {
			return err
		}
	}

	return nil
}

func validateMsgType(d *datadictionary.DataDictionary, msgType string, _ *Message) MessageRejectError {
	if _, validMsgType := d.Messages[msgType]; !validMsgType {
		return InvalidMessageType()
	}
	return nil
}

const validationInlineTagCapacity = 512

// validationTagSet has one owner. Public validators use a call-local table;
// sessions reuse fixed storage and clear only the entries touched by a call.
// Keep either table at most half full. Larger legacy messages spill into a
// private map without imposing a new validation limit.
type validationTagSet struct {
	tags     []Tag
	limit    int
	count    int
	hasZero  bool
	overflow datadictionary.TagSet
	touched  []int
	used     int
}

func (seen *validationTagSet) initFixed(capacity int) {
	size := 2
	for size < 2*capacity {
		size *= 2
	}
	seen.tags = make([]Tag, size)
	seen.limit = capacity
	seen.touched = make([]int, capacity)
}

func (seen *validationTagSet) reset() {
	for _, index := range seen.touched[:seen.used] {
		seen.tags[index] = 0
	}
	seen.used = 0
	seen.count = 0
	seen.hasZero = false
}

// add returns false for an already visited top-level tag. Tag zero needs a
// separate marker because zero also identifies an unoccupied table entry.
func (seen *validationTagSet) add(tag Tag) bool {
	if seen.overflow != nil {
		if _, exists := seen.overflow[int(tag)]; exists {
			return false
		}
		seen.overflow.Add(int(tag))
		return true
	}
	if tag == 0 {
		if seen.hasZero {
			return false
		}
		if seen.count == seen.limit {
			seen.spill(tag)
		} else {
			seen.hasZero = true
			seen.count++
		}
		return true
	}
	hash := uint64(tag) * 11400714819323198485
	hash ^= hash >> 32
	mask := len(seen.tags) - 1
	index := int(hash & uint64(mask))
	for {
		switch seen.tags[index] {
		case tag:
			return false
		case 0:
			if seen.count == seen.limit {
				seen.spill(tag)
			} else {
				seen.tags[index] = tag
				if seen.touched != nil {
					seen.touched[seen.used] = index
					seen.used++
				}
				seen.count++
			}
			return true
		}
		index = (index + 1) & mask
	}
}

func (seen *validationTagSet) spill(tag Tag) {
	if seen.touched != nil {
		// The session parser and BoundedValidator check the complete field
		// count first. Only top-level tags enter this table.
		panic(ErrValidationCapacity)
	}
	seen.overflow = make(datadictionary.TagSet, seen.count+1)
	for _, previous := range seen.tags {
		if previous != 0 {
			seen.overflow.Add(int(previous))
		}
	}
	if seen.hasZero {
		seen.overflow.Add(0)
	}
	seen.overflow.Add(int(tag))
}

func validateWalkWithScratch(transportDD *datadictionary.DataDictionary, appDD *datadictionary.DataDictionary, settings ValidatorSettings, msgType string, msg *Message, scratch *validationTagSet, metadata *validationRequiredMetadata) MessageRejectError {
	if scratch == nil {
		return validateWalkWithLocalScratch(transportDD, appDD, settings, msgType, msg)
	}
	return validateWalkWithTagSet(transportDD, appDD, settings, msgType, msg, scratch, metadata)
}

// Keep the public validator's stack table in its own call frame. Session
// validation must not initialize an unused 8 KiB table on every message.
func validateWalkWithLocalScratch(transportDD, appDD *datadictionary.DataDictionary, settings ValidatorSettings, msgType string, msg *Message) MessageRejectError {
	var tags [2 * validationInlineTagCapacity]Tag
	seen := validationTagSet{tags: tags[:], limit: validationInlineTagCapacity}
	return validateWalkWithTagSet(transportDD, appDD, settings, msgType, msg, &seen, nil)
}

func validateWalkWithTagSet(transportDD, appDD *datadictionary.DataDictionary, settings ValidatorSettings, msgType string, msg *Message, seen *validationTagSet, metadata *validationRequiredMetadata) MessageRejectError {
	remainingFields := msg.fields
	bodyDef := appDD.Messages[msgType]

	var messageDef *datadictionary.MessageDef
	var fieldDef *datadictionary.FieldDef
	var err MessageRejectError
	var ok bool

	for len(remainingFields) > 0 {
		tag := remainingFields[0].tag

		switch {
		case tag.IsHeader():
			messageDef = transportDD.Header
		case tag.IsTrailer():
			messageDef = transportDD.Trailer
		default: // is body
			messageDef = bodyDef
		}

		if !seen.add(tag) {
			return metadata.reject(tag, rejectReasonTagAppearsMoreThanOnce)
		}

		if fieldDef, ok = messageDef.Fields[int(tag)]; !ok {
			if !checkFieldNotDefined(settings, tag) {
				return metadata.reject(tag, rejectReasonTagNotDefinedForThisMessageType)
			}
			remainingFields = remainingFields[1:]
			continue
		}

		if remainingFields, err = validateVisitFieldWithMetadata(fieldDef, remainingFields, metadata); err != nil {
			return err
		}
	}

	if len(remainingFields) != 0 {
		return metadata.reject(remainingFields[0].tag, rejectReasonTagNotDefinedForThisMessageType)
	}

	return nil
}

func validateVisitField(fieldDef *datadictionary.FieldDef, fields []TagValue) ([]TagValue, MessageRejectError) {
	return validateVisitFieldWithMetadata(fieldDef, fields, nil)
}

func validateVisitFieldWithMetadata(fieldDef *datadictionary.FieldDef, fields []TagValue, metadata *validationRequiredMetadata) ([]TagValue, MessageRejectError) {
	if len(fieldDef.Fields) > 0 {
		var err MessageRejectError
		if fields, err = validateVisitGroupField(fieldDef, fields, metadata); err != nil {
			return nil, err
		}
		return fields, nil
	}

	return fields[1:], nil
}

func validateVisitGroupField(fieldDef *datadictionary.FieldDef, fieldStack []TagValue, metadata *validationRequiredMetadata) ([]TagValue, MessageRejectError) {
	numInGroupTag := fieldStack[0].tag
	var numInGroup FIXInt

	if err := numInGroup.Read(fieldStack[0].value); err != nil {
		return nil, metadata.reject(numInGroupTag, rejectReasonIncorrectDataFormatForValue)
	}

	fieldStack = fieldStack[1:]

	var childDefs []*datadictionary.FieldDef
	groupCount := 0

	for len(fieldStack) > 0 {

		// Start of repeating group.
		if int(fieldStack[0].tag) == fieldDef.Fields[0].Tag() {
			childDefs = fieldDef.Fields
			groupCount++
		}

		// Group complete.
		if len(childDefs) == 0 {
			break
		}

		if int(fieldStack[0].tag) == childDefs[0].Tag() {
			var err MessageRejectError
			if fieldStack, err = validateVisitFieldWithMetadata(childDefs[0], fieldStack, metadata); err != nil {
				return fieldStack, err
			}
		} else {
			if childDefs[0].Required() {
				return fieldStack, metadata.missing(Tag(childDefs[0].Tag()))
			}
		}

		childDefs = childDefs[1:]
	}

	if groupCount != int(numInGroup) {
		return fieldStack, metadata.reject(numInGroupTag, rejectReasonIncorrectNumInGroupCountForRepeatingGroup)
	}

	return fieldStack, nil
}

func validateFieldContent(msg *Message, checkFieldsHaveValues, checkFieldsOutOfOrder bool, metadata *validationRequiredMetadata) MessageRejectError {
	if !checkFieldsHaveValues && !checkFieldsOutOfOrder {
		return nil
	}
	inHeader := true
	inTrailer := false
	for _, field := range msg.fields {
		t := field.tag
		if checkFieldsHaveValues && len(field.value) == 0 {
			return metadata.reject(t, rejectReasonTagSpecifiedWithoutAValue)
		}
		switch {
		case inHeader && t.IsHeader():
		case inHeader && !t.IsHeader():
			inHeader = false
		case !inHeader && t.IsHeader() && checkFieldsOutOfOrder:
			return metadata.reject(t, rejectReasonTagSpecifiedOutOfRequiredOrder)
		case t.IsTrailer():
			inTrailer = true
		case inTrailer && !t.IsTrailer() && checkFieldsOutOfOrder:
			return metadata.reject(t, rejectReasonTagSpecifiedOutOfRequiredOrder)
		}
	}

	return nil
}

func validateRequired(transportDD *datadictionary.DataDictionary, appDD *datadictionary.DataDictionary, msgType string, message *Message, compiled *validationRequiredMetadata) MessageRejectError {
	if compiled != nil && compiled.allPresent(msgType, message) {
		return nil
	}
	// On a missing field, retain the original traversal and reject selection.
	// Compiled order is used only for successful session validation.
	if err := validateRequiredFieldMap(compiled, transportDD.Header.RequiredTags, message.Header.FieldMap); err != nil {
		return err
	}

	if err := validateRequiredFieldMap(compiled, appDD.Messages[msgType].RequiredTags, message.Body.FieldMap); err != nil {
		return err
	}

	if err := validateRequiredFieldMap(compiled, transportDD.Trailer.RequiredTags, message.Trailer.FieldMap); err != nil {
		return err
	}

	return nil
}

func validateRequiredFieldMap(compiled *validationRequiredMetadata, requiredTags map[int]struct{}, fieldMap FieldMap) MessageRejectError {
	for required := range requiredTags {
		requiredTag := Tag(required)
		if !fieldMap.Has(requiredTag) {
			return compiled.missing(requiredTag)
		}
	}

	return nil
}

func validateFields(transportDD *datadictionary.DataDictionary,
	appDD *datadictionary.DataDictionary,
	settings ValidatorSettings,
	message *Message,
	metadata *validationRequiredMetadata,
) MessageRejectError {
	for index := range message.fields {
		field := &message.fields[index]
		switch {
		case field.tag.IsHeader():
			if err := validateField(transportDD, settings, field, metadata); err != nil {
				return err
			}
		case field.tag.IsTrailer():
			if err := validateField(transportDD, settings, field, metadata); err != nil {
				return err
			}
		default:
			if err := validateField(appDD, settings, field, metadata); err != nil {
				return err
			}
		}
	}

	return nil
}

func getFieldType(d *datadictionary.DataDictionary, field int) (*datadictionary.FieldType, bool) {
	fieldType, isMessageField := d.FieldTypeByTag[field]
	return fieldType, isMessageField
}

func checkFieldNotDefined(settings ValidatorSettings, field Tag) bool {
	fail := false
	if int(field) < UserDefinedTagMin {
		fail = !settings.AllowUnknownMessageFields
	} else {
		fail = settings.CheckUserDefinedFields
	}
	return !fail
}

func validateField(d *datadictionary.DataDictionary,
	settings ValidatorSettings,
	field *TagValue,
	metadata *validationRequiredMetadata,
) MessageRejectError {
	if len(field.value) == 0 {
		return metadata.reject(field.tag, rejectReasonTagSpecifiedWithoutAValue)
	}

	fieldType, isMessageField := getFieldType(d, int(field.tag))
	if !isMessageField && !checkFieldNotDefined(settings, field.tag) {
		return InvalidTagNumber(field.tag)
	}

	if !isMessageField {
		return nil
	}

	if !validEnumValues(fieldType, field.value) {
		return metadata.reject(field.tag, rejectReasonValueIsIncorrect)
	}

	if !validFieldValue(fieldType.Type, field.value) {
		return metadata.reject(field.tag, rejectReasonIncorrectDataFormatForValue)
	}

	return nil
}
