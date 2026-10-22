package quickfix

import "github.com/quickfixgo/quickfix/datadictionary"

// fieldMetadata is immutable after setup. Sharing it never retains a session,
// message or mutable frame. Each session owns a separate metadata object, while
// its slots and owned snapshots may retain the same immutable values.
type fieldMetadata struct {
	missingRejects                                           preparedMissingRejects
	messageTypes                                             map[string]string
	beginString, senderCompID, senderSubID, senderLocationID string
	targetCompID, targetSubID, targetLocationID              string
}

func newSessionFieldMetadata(id SessionID, rejects preparedMissingRejects, dictionaries ...*datadictionary.DataDictionary) *fieldMetadata {
	metadata := &fieldMetadata{
		missingRejects: rejects,
		beginString:    id.BeginString, senderCompID: id.SenderCompID,
		senderSubID: id.SenderSubID, senderLocationID: id.SenderLocationID,
		targetCompID: id.TargetCompID, targetSubID: id.TargetSubID, targetLocationID: id.TargetLocationID,
	}
	typeCount := 0
	for _, dictionary := range dictionaries {
		if dictionary != nil {
			typeCount += len(dictionary.Messages)
		}
	}
	if typeCount != 0 {
		metadata.messageTypes = make(map[string]string, typeCount)
		for _, dictionary := range dictionaries {
			if dictionary != nil {
				for messageType := range dictionary.Messages {
					metadata.messageTypes[messageType] = messageType
				}
			}
		}
	}
	return metadata
}

// ownedString may reuse only an exact immutable configured value. Sender and
// target identities can occur in either direction, including retained replay.
// The caller has already looked up the actual field, so absent optional fields
// cannot acquire a value from configuration. Comparisons borrow value only for
// the comparison; the fallback keeps the ordinary owned-string conversion.
func (metadata *fieldMetadata) ownedString(tag Tag, value []byte) string {
	if metadata != nil {
		var first, second string
		switch tag {
		case tagMsgType:
			if configured, ok := metadata.messageTypes[string(value)]; ok {
				return configured
			}
			return string(value)
		case tagBeginString:
			first = metadata.beginString
		case tagSenderCompID, tagTargetCompID:
			first, second = metadata.senderCompID, metadata.targetCompID
		case tagSenderSubID, tagTargetSubID:
			first, second = metadata.senderSubID, metadata.targetSubID
		case tagSenderLocationID, tagTargetLocationID:
			first, second = metadata.senderLocationID, metadata.targetLocationID
		default:
			return string(value)
		}
		if string(value) == first {
			return first
		}
		if string(value) == second {
			return second
		}
	}
	return string(value)
}

func (message *Message) useFieldMetadata(metadata *fieldMetadata) {
	message.Header.metadata = metadata
	message.Body.metadata = metadata
	message.Trailer.metadata = metadata
}
