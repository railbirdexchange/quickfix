package quickfix

// fieldMetadata is immutable after setup. Sharing it never retains a session,
// message or mutable frame. Each session owns a separate metadata object, while
// its slots and owned snapshots may retain the same immutable values.
type fieldMetadata struct {
	missingRejects                                           preparedMissingRejects
	beginString, senderCompID, senderSubID, senderLocationID string
	targetCompID, targetSubID, targetLocationID              string
}

func newSessionFieldMetadata(id SessionID, rejects preparedMissingRejects) *fieldMetadata {
	return &fieldMetadata{
		missingRejects: rejects,
		beginString:    id.BeginString, senderCompID: id.SenderCompID,
		senderSubID: id.SenderSubID, senderLocationID: id.SenderLocationID,
		targetCompID: id.TargetCompID, targetSubID: id.TargetSubID, targetLocationID: id.TargetLocationID,
	}
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
