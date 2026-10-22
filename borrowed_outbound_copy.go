package quickfix

// borrowedOutboundCopyScope makes the existing owned-copy APIs consistent for
// cold and warmed outgoing messages. A first send has no construction arena,
// but its caller may still reuse external SetBytes input after SendToTarget.
// The temporary markers only affect snapshot APIs; setters keep their original
// representation and the caller retains the same Message and byte aliases.
type borrowedOutboundCopyScope struct {
	message  *Message
	marked   uint8
	original [3]FieldMap
}

func beginBorrowedOutboundCopy(message *Message) borrowedOutboundCopyScope {
	scope := borrowedOutboundCopyScope{message: message}
	for index, fields := range [...]*FieldMap{&message.Header.FieldMap, &message.Body.FieldMap, &message.Trailer.FieldMap} {
		if fields.scoped || fields.reusable {
			continue
		}
		scope.marked |= 1 << index
		scope.original[index] = *fields
		fields.reusable = true
	}
	return scope
}

func (scope *borrowedOutboundCopyScope) release() {
	if scope.marked == 0 {
		return
	}
	for index, fields := range [...]*FieldMap{&scope.message.Header.FieldMap, &scope.message.Body.FieldMap, &scope.message.Trailer.FieldMap} {
		original := &scope.original[index]
		if scope.marked&(1<<index) != 0 && fields.fixed == original.fixed && fields.rwLock == original.rwLock {
			fields.reusable = false
		}
	}
}
