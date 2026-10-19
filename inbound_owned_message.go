package quickfix

import "bytes"

// ownedInboundMessage keeps the raw-buffer descriptor with the message exposed
// to an ordinary Application. Neither object belongs to a reusable engine slot.
type ownedInboundMessage struct {
	message Message
	raw     bytes.Buffer
}

// materializeInboundMessage is used only immediately after a successful fixed
// parse, before any callback can replace fields. The parsed fields are contiguous
// ranges of the original frame. Copy those ranges and their section/group indexes
// instead of parsing the owned frame a second time.
func materializeInboundMessage(parsed *Message) *Message {
	frame := bytes.Clone(parsed.rawMessage.Bytes())
	owned := &ownedInboundMessage{raw: *bytes.NewBuffer(frame)}
	message := &owned.message
	message.Header.Init()
	message.Body.Init()
	message.Trailer.Init()
	message.rawMessage = &owned.raw
	message.fields = make([]TagValue, len(parsed.fields))
	sections := [...]struct{ source, destination *FieldMap }{
		{&parsed.Header.FieldMap, &message.Header.FieldMap},
		{&parsed.Body.FieldMap, &message.Body.FieldMap},
		{&parsed.Trailer.FieldMap, &message.Trailer.FieldMap},
	}
	// Each owned section gets an exact-capacity portion of one tag allocation.
	// Legacy setters may subsequently replace or enlarge their own section.
	tags := make([]Tag, parsed.Header.fieldCount()+parsed.Body.fieldCount()+parsed.Trailer.fieldCount())
	for _, section := range sections {
		section.destination.missingRejects = section.source.missingRejects
		count := section.source.fieldCount()
		section.destination.tags = tags[:0:count]
		tags = tags[count:]
	}
	offset := 0
	for index := range parsed.fields {
		source := &parsed.fields[index]
		end := offset + len(source.bytes)
		message.fields[index] = TagValue{
			tag: source.tag, bytes: frame[offset:end:end],
			value: frame[end-1-len(source.value) : end-1 : end-1],
		}
		if len(parsed.bodyBytes) != 0 && &parsed.bodyBytes[0] == &source.bytes[0] {
			message.bodyBytes = frame[offset : offset+len(parsed.bodyBytes)]
		}
		for _, section := range sections {
			values, found := section.source.lookup(source.tag)
			if found && &values[0] == source {
				section.destination.add(message.fields[index : index+len(values)])
			}
		}
		offset = end
	}
	return message
}
