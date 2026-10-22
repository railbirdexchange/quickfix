package quickfix

import "github.com/quickfixgo/quickfix/datadictionary"

// configuredValidationReject owns only immutable scalar data. Results live in
// one setup-owned block, never in a reusable message or scratch slot.
// The same compact value identifies an applicable tag/reason in the setup table.
type configuredValidationReject struct {
	tag    Tag
	reason int
}

func (reject configuredValidationReject) Error() string {
	switch reject.reason {
	case rejectReasonTagNotDefinedForThisMessageType:
		return rejectTextTagNotDefined
	case rejectReasonTagSpecifiedWithoutAValue:
		return rejectTextEmptyValue
	case rejectReasonValueIsIncorrect:
		return rejectTextValueIsIncorrect
	case rejectReasonIncorrectDataFormatForValue:
		return rejectTextIncorrectDataFormat
	case rejectReasonTagAppearsMoreThanOnce:
		return rejectTextDuplicateTag
	case rejectReasonTagSpecifiedOutOfRequiredOrder:
		return rejectTextFieldOrder
	case rejectReasonIncorrectNumInGroupCountForRepeatingGroup:
		return rejectTextIncorrectGroupCount
	}
	panic("unsupported configured validation reject reason")
}

func (reject configuredValidationReject) RejectReason() int     { return reject.reason }
func (reject configuredValidationReject) RefTagID() (Tag, bool) { return reject.tag, true }
func (configuredValidationReject) BusinessRejectRefID() string  { return "" }
func (configuredValidationReject) IsBusinessReject() bool       { return false }

func (metadata *validationRequiredMetadata) reject(tag Tag, reason int) MessageRejectError {
	key := configuredValidationReject{tag: tag, reason: reason}
	if metadata != nil {
		if prepared := metadata.configured[key]; prepared != nil {
			return prepared
		}
	}
	// Unknown tags and public validators retain the existing owned constructor.
	// They never insert into or otherwise mutate session metadata.
	return NewMessageRejectError(key.Error(), reason, &tag)
}

func (metadata *validationRequiredMetadata) prepareConfiguredReject(tag Tag, reason int) {
	key := configuredValidationReject{tag: tag, reason: reason}
	// Collect the finite set first. finishConfiguredRejects publishes immutable
	// pointers only after all dictionary and group traversal has finished.
	metadata.configured[key] = nil
}

func (metadata *validationRequiredMetadata) finishConfiguredRejects() {
	rejects := make([]configuredValidationReject, len(metadata.configured))
	index := 0
	for key := range metadata.configured {
		rejects[index] = key
		metadata.configured[key] = &rejects[index]
		index++
	}
}

func (metadata *validationRequiredMetadata) prepareConfiguredFields(dictionary *datadictionary.DataDictionary) {
	for number, field := range dictionary.FieldTypeByTag {
		tag := Tag(number)
		// Any configured field can be empty, duplicated, placed after a trailer,
		// or used in a message where it is not defined.
		metadata.prepareConfiguredReject(tag, rejectReasonTagSpecifiedWithoutAValue)
		metadata.prepareConfiguredReject(tag, rejectReasonTagAppearsMoreThanOnce)
		metadata.prepareConfiguredReject(tag, rejectReasonTagSpecifiedOutOfRequiredOrder)
		metadata.prepareConfiguredReject(tag, rejectReasonTagNotDefinedForThisMessageType)
		if field == nil || len(field.Enums) != 0 {
			metadata.prepareConfiguredReject(tag, rejectReasonValueIsIncorrect)
		}
		if field != nil && validationTypeCanRejectFormat(field.Type) {
			metadata.prepareConfiguredReject(tag, rejectReasonIncorrectDataFormatForValue)
		}
	}
}

// This mirrors only the always-accepted types in validFieldValue. All other
// types, including an unrecognized dictionary type, may reject their format.
func validationTypeCanRejectFormat(kind string) bool {
	switch kind {
	case "MULTIPLESTRINGVALUE", "MULTIPLEVALUESTRING", "MULTIPLECHARVALUE",
		"CHAR", "CURRENCY", "DATA", "MONTHYEAR", "LOCALMKTDATE", "DATE",
		"EXCHANGE", "LANGUAGE", "XMLDATA", "COUNTRY", "UTCTIMEONLY",
		"UTCDATEONLY", "UTCDATE", "TZTIMEONLY", "TZTIMESTAMP", "STRING":
		return false
	}
	return true
}

func (metadata *validationRequiredMetadata) prepareConfiguredGroups(definition *datadictionary.MessageDef) {
	for _, field := range definition.Fields {
		metadata.prepareConfiguredGroup(field)
	}
}

func (metadata *validationRequiredMetadata) prepareConfiguredGroup(group *datadictionary.FieldDef) {
	if len(group.Fields) == 0 {
		return
	}
	// The walker always reads group counts as FIXInt, even when a custom
	// dictionary declares another field type for the root.
	metadata.prepareConfiguredReject(Tag(group.Tag()), rejectReasonIncorrectDataFormatForValue)
	metadata.prepareConfiguredReject(Tag(group.Tag()), rejectReasonIncorrectNumInGroupCountForRepeatingGroup)
	for _, child := range group.Fields {
		if child.Required() {
			metadata.prepareRejects([]Tag{Tag(child.Tag())})
		}
		metadata.prepareConfiguredGroup(child)
	}
}
