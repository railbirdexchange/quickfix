package quickfix

import "github.com/quickfixgo/quickfix/datadictionary"

// preparedMissingRejects is immutable after construction. Messages may share
// its errors, including after their frame or session storage has been reused.
// Traffic never inserts an unknown tag into this table.
type preparedMissingRejects map[Tag]*missingFieldReject

func prepareMissingRejects(transport, application *datadictionary.DataDictionary) preparedMissingRejects {
	capacity := 0
	if transport != nil {
		capacity += len(transport.FieldTypeByTag)
	}
	if application != nil && application != transport {
		capacity += len(application.FieldTypeByTag)
	}
	if capacity == 0 {
		return nil
	}
	rejects := make(preparedMissingRejects, capacity)
	entries := make([]missingFieldReject, capacity)
	for _, dictionary := range [...]*datadictionary.DataDictionary{transport, application} {
		if dictionary == nil {
			continue
		}
		for number := range dictionary.FieldTypeByTag {
			tag := Tag(number)
			if _, prepared := rejects[tag]; prepared {
				continue
			}
			entry := &entries[len(rejects)]
			*entry = newMissingFieldReject(tag)
			rejects[tag] = entry
		}
	}
	return rejects
}

func (fields FieldMap) missingFieldReject(tag Tag) MessageRejectError {
	if fields.metadata != nil {
		if reject := fields.metadata.missingRejects[tag]; reject != nil {
			return reject
		}
	}
	return ConditionallyRequiredFieldMissing(tag)
}
