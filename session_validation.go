package quickfix

import "github.com/quickfixgo/quickfix/datadictionary"

// validationRequiredMetadata is built once for the private session validator.
// Session dictionaries are immutable. Public validators retain direct dictionary
// traversal, including their existing concurrent-use behavior.
type validationRequiredMetadata struct {
	header   []Tag
	trailer  []Tag
	messages map[string][]Tag
	rejects  map[Tag]MessageRejectError
}

func newValidationRequiredMetadata(transport, application *datadictionary.DataDictionary) *validationRequiredMetadata {
	if transport == nil || application == nil {
		return nil
	}
	metadata := &validationRequiredMetadata{
		header:   copyValidationRequiredTags(transport.Header.RequiredTags),
		trailer:  copyValidationRequiredTags(transport.Trailer.RequiredTags),
		messages: make(map[string][]Tag, len(application.Messages)),
		rejects:  make(map[Tag]MessageRejectError),
	}
	metadata.prepareRejects(metadata.header)
	metadata.prepareRejects(metadata.trailer)
	for messageType, definition := range application.Messages {
		tags := copyValidationRequiredTags(definition.RequiredTags)
		metadata.messages[messageType] = tags
		metadata.prepareRejects(tags)
	}
	return metadata
}

// Required-tag errors are immutable and may outlive the message or session.
// Prepare only the finite dictionary union, before the validator is published.
func (metadata *validationRequiredMetadata) prepareRejects(tags []Tag) {
	for _, tag := range tags {
		if _, exists := metadata.rejects[tag]; !exists {
			metadata.rejects[tag] = RequiredTagMissing(tag)
		}
	}
}

func (metadata *validationRequiredMetadata) missing(tag Tag) MessageRejectError {
	if metadata != nil {
		if reject := metadata.rejects[tag]; reject != nil {
			return reject
		}
	}
	return RequiredTagMissing(tag)
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
	if fields.fixed != nil {
		// Fixed fields and this private validator share the session owner.
		// Avoid the public value-receiver copy and legacy lock/defer path.
		for _, tag := range required {
			if _, present := fields.fixed.find(tag); !present {
				return false
			}
		}
		return true
	}
	for _, tag := range required {
		if !fields.Has(tag) {
			return false
		}
	}
	return true
}
