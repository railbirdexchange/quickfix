package quickfix

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

func assertCachedAppendMatchesOwnedCopy(t *testing.T, message *Message, body []byte, useBody bool) []byte {
	t.Helper()
	owned := NewMessage()
	message.CopyInto(owned)
	if owned.arena != nil {
		t.Fatal("reference copy retained the construction arena")
	}
	appendMessage, appendOwned := message.AppendTo, owned.AppendTo
	if useBody {
		appendMessage = func(dst []byte) ([]byte, error) { return message.AppendWithBodyBytesTo(dst, body) }
		appendOwned = func(dst []byte) ([]byte, error) { return owned.AppendWithBodyBytesTo(dst, body) }
	}
	want, err := appendOwned(make([]byte, 0, 8192))
	if err != nil {
		t.Fatal(err)
	}
	// Includes exact capacity, one byte short, and an existing destination
	// prefix. Every failure must leave the entire backing array unchanged.
	assertBoundedMessageAppend(t, message, appendMessage, string(want))
	got, err := appendMessage(make([]byte, 0, len(want)))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("append after boundary checks = %q, %v; want %q", got, err, want)
	}
	return want
}

func TestMessageAppendCachedLengthsTrackScalarMutations(t *testing.T) {
	message := newArenaTestMessage(t, 4096, 64)
	steps := []struct {
		name   string
		mutate func()
	}{
		{"all_sections", func() {
			message.Header.SetString(50, "sender")
			message.Body.SetString(55, "SYMBOL")
			message.Trailer.SetString(89, "signature")
			message.Header.SetInt(tagBodyLength, 123456)
			message.Trailer.SetString(tagCheckSum, "ignored checksum")
		}},
		{"grow", func() {
			message.Header.SetString(tagBeginString, "FIXT.1.1")
			message.Header.SetString(50, "longer sender")
			message.Body.SetString(55, strings.Repeat("S", 100))
			message.Trailer.SetString(89, "longer signature")
		}},
		{"shrink", func() {
			message.Header.SetString(tagBeginString, "FIX.4.2")
			message.Header.SetString(50, "")
			message.Body.SetString(55, "S")
			message.Trailer.SetString(89, "")
		}},
		{"remove", func() {
			message.Header.Remove(tagBeginString)
			message.Header.Remove(50)
			message.Body.Remove(55)
			message.Trailer.Remove(89)
		}},
		{"reinsert_and_unusual_sections", func() {
			message.Header.SetString(tagBeginString, "FIX.4.4")
			message.Header.SetString(tagCheckSum, "header checksum is ordinary data")
			message.Body.SetString(tagBeginString, "body begin")
			message.Body.SetString(tagBodyLength, "body length")
			message.Body.SetString(tagCheckSum, "body checksum")
			message.Trailer.SetString(tagBeginString, "trailer begin")
			message.Trailer.SetString(tagBodyLength, "trailer length")
		}},
		{"clear_body", func() { message.Body.Clear() }},
		{"clear_header", func() { message.Header.Clear() }},
		{"clear_trailer", func() { message.Trailer.Clear() }},
		{"reuse_after_clear", func() {
			message.Header.SetString(tagBeginString, "FIX.4.4")
			message.Header.SetString(tagMsgType, "D")
			message.Body.SetString(55, "restored")
		}},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			step.mutate()
			if message.arena == nil || message.arena.owner != message || !message.arena.intact() {
				t.Fatal("scalar mutation left the cached-length path")
			}
			assertCachedAppendMatchesOwnedCopy(t, message, nil, false)
		})
	}

	before := assertCachedAppendMatchesOwnedCopy(t, message, nil, false)
	getter, reject := message.Body.GetBytes(55)
	if reject != nil {
		t.Fatal(reject)
	}
	getter[0] = 'X'
	if after := assertCachedAppendMatchesOwnedCopy(t, message, nil, false); !bytes.Equal(after, before) {
		t.Fatal("mutable getter changed the encoded snapshot")
	}
}

func TestMessageAppendCachedLengthsBodyLengthDigits(t *testing.T) {
	message := newArenaTestMessage(t, 4096, 16)
	message.Header.SetInt(tagBodyLength, 99999)
	message.Trailer.SetString(tagCheckSum, "999")
	for _, length := range []int{9, 10, 99, 100, 999, 1000, 100, 99, 10, 9} {
		t.Run(strconv.Itoa(length), func(t *testing.T) {
			// 35=D<SOH> plus 55=<value><SOH> contributes nine fixed bytes.
			message.Body.SetString(55, strings.Repeat("x", length-9))
			wire := assertCachedAppendMatchesOwnedCopy(t, message, nil, false)
			prefix := "8=FIX.4.4\x019=" + strconv.Itoa(length) + "\x01"
			if !bytes.HasPrefix(wire, []byte(prefix)) {
				t.Fatalf("BodyLength %d: wire %q", length, wire)
			}
		})
	}
}

func TestMessageAppendCachedLengthsTrackGroups(t *testing.T) {
	makeGroup := func(tag, child Tag, value string) *RepeatingGroup {
		group := NewRepeatingGroup(tag, GroupTemplate{GroupElement(child)})
		group.Add().SetString(child, value)
		return group
	}
	message := NewMessage()
	message.Header.SetString(tagBeginString, "FIX.4.4")
	message.Header.SetString(tagMsgType, "D")
	message.Body.SetGroup(makeGroup(453, 448, "initial party"))
	if err := message.attachMessageArena(4096, 64); err != nil {
		t.Fatal(err)
	}
	assertCachedAppendMatchesOwnedCopy(t, message, nil, false)

	for _, section := range []struct {
		name   string
		fields *FieldMap
		tag    Tag
	}{
		{"header", &message.Header.FieldMap, 627},
		{"body", &message.Body.FieldMap, 453},
		{"trailer", &message.Trailer.FieldMap, 9000},
	} {
		t.Run(section.name, func(t *testing.T) {
			nested := makeGroup(9001, 9002, "nested value")
			group := NewRepeatingGroup(section.tag, GroupTemplate{GroupElement(8), GroupElement(9), GroupElement(10), nested})
			entry := group.Add()
			entry.SetString(8, "child begin")
			entry.SetString(9, "child length")
			entry.SetString(10, "child checksum")
			entry.SetGroup(nested)
			section.fields.SetGroup(group)
			assertCachedAppendMatchesOwnedCopy(t, message, nil, false)
			section.fields.SetString(section.tag, "group replaced by scalar")
			assertCachedAppendMatchesOwnedCopy(t, message, nil, false)
			section.fields.Remove(section.tag)
			section.fields.SetGroup(makeGroup(section.tag, 9003, "reinserted"))
			assertCachedAppendMatchesOwnedCopy(t, message, nil, false)
		})
	}
}

func TestMessageAppendCachedLengthsFallbacks(t *testing.T) {
	for _, operation := range []string{"raw_body", "empty_raw_body", "shallow_copy", "replace_header", "replace_body", "replace_trailer", "copy_section", "init_section", "copy_message"} {
		t.Run(operation, func(t *testing.T) {
			message := newArenaTestMessage(t, 4096, 32)
			message.Body.SetString(11, "original body")
			message.Trailer.SetString(89, "original signature")
			replacement := NewMessage()
			replacement.Header.SetString(tagBeginString, "FIXT.1.1")
			replacement.Header.SetString(tagMsgType, "8")
			replacement.Header.SetString(49, "replacement sender")
			replacement.Body.SetString(55, "a differently sized replacement body")
			replacement.Trailer.SetString(89, "new")
			var body []byte
			useBody := false
			switch operation {
			case "raw_body":
				body, useBody = []byte("453=1\x01448=party\x01447=D\x0155=resend symbol\x01"), true
			case "empty_raw_body":
				useBody = true
			case "shallow_copy":
				original := message
				copied := *original
				copied.Body = replacement.Body
				message = &copied
				if !original.arena.intact() || message.arena.owner == message {
					t.Fatal("fixture must isolate owner identity from arena integrity")
				}
			case "replace_header":
				message.Header = replacement.Header
			case "replace_body":
				message.Body = replacement.Body
			case "replace_trailer":
				message.Trailer = replacement.Trailer
			case "copy_section":
				replacement.Body.CopyInto(&message.Body.FieldMap)
			case "init_section":
				message.Header.Init()
			case "copy_message":
				replacement.CopyInto(message)
			}
			// No setter intervenes after direct section replacement: encoding
			// itself must detect that the original arena totals are inapplicable.
			assertCachedAppendMatchesOwnedCopy(t, message, body, useBody)
		})
	}
}

func TestMessageAppendCachedLengthsPreserveConstructionError(t *testing.T) {
	message := newArenaTestMessage(t, 128, 16)
	message.Body.SetString(55, strings.Repeat("x", 128))
	storage := bytes.Repeat([]byte{0xa5}, 256)
	before := bytes.Clone(storage)
	result, err := message.AppendTo(storage[:3])
	if err != ErrFrameCapacity || len(result) != 3 || cap(result) != len(storage) || &result[0] != &storage[0] || !bytes.Equal(storage, before) {
		t.Fatalf("latched construction failure changed output: len %d cap %d error %v", len(result), cap(result), err)
	}
}
