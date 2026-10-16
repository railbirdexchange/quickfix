package quickfix

import "github.com/quickfixgo/quickfix/datadictionary"

// validationRequiredMetadata is built once for the private session validator.
// Session dictionaries are immutable. Public validators retain direct dictionary
// traversal, including their existing concurrent-use behavior.
type validationRequiredMetadata struct {
	header   []Tag
	trailer  []Tag
	messages map[string][]Tag
}

func newValidationRequiredMetadata(transport, application *datadictionary.DataDictionary) *validationRequiredMetadata {
	if transport == nil || application == nil {
		return nil
	}
	metadata := &validationRequiredMetadata{
		header:   copyValidationRequiredTags(transport.Header.RequiredTags),
		trailer:  copyValidationRequiredTags(transport.Trailer.RequiredTags),
		messages: make(map[string][]Tag, len(application.Messages)),
	}
	for messageType, definition := range application.Messages {
		metadata.messages[messageType] = copyValidationRequiredTags(definition.RequiredTags)
	}
	return metadata
}

func copyValidationRequiredTags(required datadictionary.TagSet) []Tag {
	tags := make([]Tag, len(required))
	index := 0
	for tag := range required {
		tags[index] = Tag(tag)
		index++
	}
	return tags
}

func (metadata *validationRequiredMetadata) allPresent(messageType string, message *Message) bool {
	return validationHasRequiredTags(metadata.header, &message.Header.FieldMap) &&
		validationHasRequiredTags(metadata.messages[messageType], &message.Body.FieldMap) &&
		validationHasRequiredTags(metadata.trailer, &message.Trailer.FieldMap)
}

func validationHasRequiredTags(required []Tag, fields *FieldMap) bool {
	for _, tag := range required {
		if !fields.Has(tag) {
			return false
		}
	}
	return true
}
