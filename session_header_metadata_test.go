package quickfix

import (
	"sync"
	"testing"
)

func TestSessionHeaderMetadataIsImmutableAndSessionLocal(t *testing.T) {
	firstID, secondID := sessionHeaderStringID("metadata-first"), sessionHeaderStringID("metadata-second")
	first := newSessionHeaderStringSession(t, firstID)
	second := newSessionHeaderStringSession(t, secondID)
	if first.boundedIn.metadata == second.boundedIn.metadata {
		t.Fatal("different sessions share mutable setup metadata")
	}
	for _, current := range []*session{first, second} {
		for _, message := range []*Message{&current.boundedIn.parsed.message, &current.outbound.replay.message, current.controls.message} {
			for _, fields := range []*FieldMap{&message.Header.FieldMap, &message.Body.FieldMap, &message.Trailer.FieldMap} {
				if fields.metadata != current.boundedIn.metadata {
					t.Fatal("session slot did not bind its immutable metadata before use")
				}
			}
		}
	}
	// Sharing immutable dictionary errors must not make the identities shared.
	sharedRejects := first.boundedIn.metadata.missingRejects
	firstMetadata := newSessionFieldMetadata(firstID, sharedRejects)
	secondMetadata := newSessionFieldMetadata(secondID, sharedRejects)
	var workers sync.WaitGroup
	for index := range 4 {
		metadata, id := firstMetadata, firstID
		if index%2 != 0 {
			metadata, id = secondMetadata, secondID
		}
		message := NewMessage()
		message.useFieldMetadata(metadata)
		message.Header.SetString(tagSenderCompID, id.SenderCompID)
		workers.Add(1)
		go func(message *Message, want string) {
			defer workers.Done()
			for range 1000 {
				if value, reject := message.Header.GetString(tagSenderCompID); reject != nil || value != want {
					t.Errorf("shared metadata changed identity: %q, %v", value, reject)
					return
				}
				if _, reject := message.Body.GetString(526); reject != sharedRejects[526] {
					t.Error("shared metadata changed dictionary reject")
					return
				}
			}
		}(message, id.SenderCompID)
	}
	workers.Wait()
	if firstMetadata.senderCompID != firstID.SenderCompID || secondMetadata.senderCompID != secondID.SenderCompID {
		t.Fatal("identity metadata was mutated after setup")
	}
}
