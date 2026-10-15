package quickfix

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/datadictionary"
)

func TestMessageAppendToWireFixtures(t *testing.T) {
	dictionary, err := datadictionary.Parse("spec/FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	// These independent expected wires are the existing rebuild fixtures in
	// message_test.go, including their original BodyLength and checksum values.
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "two reordered groups",
			input:    "8=FIX.4.4\x019=210\x0135=D\x0134=2\x01347=UTF-8\x0152=20231231-20:19:41\x0149=01001\x0150=01001a\x0156=TEST\x0144=12\x0111=13976\x011=10100400\x0121=1\x01386=1\x01336=NOPL\x0155=SYMABC\x0154=1\x0160=20231231-20:19:41\x0138=1\x0140=2\x0159=0\x01453=1\x01448=4501\x01447=D\x01452=28\x01354=6\x01355=Public\x0110=104\x01",
			expected: "8=FIX.4.4\x019=210\x0135=D\x0134=2\x0149=01001\x0150=01001a\x0152=20231231-20:19:41\x0156=TEST\x01347=UTF-8\x011=10100400\x0111=13976\x0121=1\x0138=1\x0140=2\x0144=12\x0154=1\x0155=SYMABC\x0159=0\x0160=20231231-20:19:41\x01354=6\x01355=Public\x01386=1\x01336=NOPL\x01453=1\x01448=4501\x01447=D\x01452=28\x0110=104\x01",
		},
		{
			name:     "two members",
			input:    "8=FIX.4.4\x019=187\x0135=D\x0134=2\x0149=01001\x0150=01001a\x0152=20231231-20:19:41\x0156=TEST\x011=acct1\x0111=13976\x0121=1\x0138=1\x0140=2\x0144=12\x0154=1\x0155=SYMABC\x0159=0\x0160=20231231-20:19:41\x01453=2\x01448=4501\x01447=D\x01452=28\x01448=4502\x01447=D\x01452=28\x0110=044\x01",
			expected: "8=FIX.4.4\x019=187\x0135=D\x0134=2\x0149=01001\x0150=01001a\x0152=20231231-20:19:41\x0156=TEST\x011=acct1\x0111=13976\x0121=1\x0138=1\x0140=2\x0144=12\x0154=1\x0155=SYMABC\x0159=0\x0160=20231231-20:19:41\x01453=2\x01448=4501\x01447=D\x01452=28\x01448=4502\x01447=D\x01452=28\x0110=044\x01",
		},
		{
			name:     "double nested group and following field",
			input:    "8=FIX.4.4\x019=202\x0135=D\x0134=2\x0149=01001\x0150=01001a\x0152=20231231-20:19:41\x0156=TEST\x011=acct1\x0111=13976\x0121=1\x0138=1\x0140=2\x0144=12\x0154=1\x0155=SYMABC\x0159=0\x0160=20231231-20:19:41\x0178=1\x0179=acct1\x01539=1\x01524=nestedid\x01804=1\x01545=doublenestedid\x0180=100\x0110=117\x01",
			expected: "8=FIX.4.4\x019=202\x0135=D\x0134=2\x0149=01001\x0150=01001a\x0152=20231231-20:19:41\x0156=TEST\x011=acct1\x0111=13976\x0121=1\x0138=1\x0140=2\x0144=12\x0154=1\x0155=SYMABC\x0159=0\x0160=20231231-20:19:41\x0178=1\x0179=acct1\x01539=1\x01524=nestedid\x01804=1\x01545=doublenestedid\x0180=100\x0110=117\x01",
		},
	}
	for _, fixture := range cases {
		t.Run(fixture.name, func(t *testing.T) {
			message := NewMessage()
			if err := ParseMessageWithDataDictionary(message, bytes.NewBufferString(fixture.input), dictionary, dictionary); err != nil {
				t.Fatal(err)
			}
			assertBoundedMessageAppend(t, message, message.AppendTo, fixture.expected)
		})
	}
}

func TestMessageAppendToConstructedMessage(t *testing.T) {
	message := NewMessage()
	message.Header.SetString(tagBeginString, BeginStringFIX44)
	message.Header.SetString(tagMsgType, "A")
	message.Header.SetField(tagSendingTime, FIXUTCTimestamp{
		Time: time.Date(2014, time.June, 15, 19, 49, 56, 999999999, time.UTC), Precision: Seconds,
	})
	message.Body.SetString(Tag(554), "secret")
	message.Body.SetString(Tag(553), "my_user")
	expected := "8=FIX.4.4\x019=49\x0135=A\x0152=20140615-19:49:56\x01553=my_user\x01554=secret\x0110=072\x01"
	assertBoundedMessageAppend(t, message, message.AppendTo, expected)
	if message.Header.Has(tagBodyLength) || message.Trailer.Has(tagCheckSum) {
		t.Fatal("AppendTo inserted generated BodyLength or CheckSum into the message")
	}

	message.Header.SetInt(tagBodyLength, 99999)
	message.Trailer.SetString(tagCheckSum, "999")
	assertBoundedMessageAppend(t, message, message.AppendTo, expected)
}

func TestMessageAppendToUsesCurrentFields(t *testing.T) {
	const original = "8=FIX.4.2\x019=104\x0135=D\x0134=2\x0149=TW\x0152=20140515-19:49:56.659\x0156=ISLD\x0111=100\x0121=1\x0140=1\x0154=1\x0155=TSLA\x0160=00010101-00:00:00.000\x0110=039\x01"
	message := NewMessage()
	if err := ParseMessage(message, bytes.NewBufferString(original)); err != nil {
		t.Fatal(err)
	}
	message.Header.SetString(tagOrigSendingTime, "20140515-19:49:56.659")
	message.Header.SetString(tagSendingTime, "20140615-19:49:56")
	message.Header.SetBool(tagPossDupFlag, true)
	expected := "8=FIX.4.2\x019=131\x0135=D\x0134=2\x0143=Y\x0149=TW\x0152=20140615-19:49:56\x0156=ISLD\x01122=20140515-19:49:56.659\x0111=100\x0121=1\x0140=1\x0154=1\x0155=TSLA\x0160=00010101-00:00:00.000\x0110=122\x01"
	assertBoundedMessageAppend(t, message, message.AppendTo, expected)
	if string(message.Bytes()) != original {
		t.Fatal("AppendTo changed legacy cached Bytes behavior")
	}
}

func TestMessageAppendWithBodyBytesToResend(t *testing.T) {
	const originalHeader = "8=FIXT.1.1\x019=161\x0135=8\x0134=3\x0149=ISLD\x0152=20240415-03:43:17.923\x0156=TW\x01"
	const originalBody = "6=1.00\x0111=1\x0114=1.00\x0117=1\x0131=1.00\x0132=1.00\x0137=1\x0138=1.00\x0139=2\x0154=1\x0155=1\x01150=2\x01151=0.00\x01453=1\x01448=xyzzy\x01447=D\x01452=1\x01"
	message := NewMessage()
	if err := ParseMessage(message, bytes.NewBufferString(originalHeader+originalBody+"10=014\x01")); err != nil {
		t.Fatal(err)
	}
	message.Header.SetString(tagOrigSendingTime, "20240415-03:43:17.923")
	message.Header.SetString(tagSendingTime, "20240415-14:41:23.456")
	message.Header.SetBool(tagPossDupFlag, true)
	// The saved body must remain authoritative even if the parsed map differs.
	message.Body.Remove(Tag(453))
	message.Body.SetString(Tag(55), "different symbol")
	expected := "8=FIXT.1.1\x019=192\x0135=8\x0134=3\x0143=Y\x0149=ISLD\x0152=20240415-14:41:23.456\x0156=TW\x01122=20240415-03:43:17.923\x01" + originalBody + "10=018\x01"
	assertBoundedMessageAppend(t, message, func(destination []byte) ([]byte, error) {
		return message.AppendWithBodyBytesTo(destination, message.bodyBytes)
	}, expected)
}

func TestMessageAppendToRemoveAndReAddDoesNotGrow(t *testing.T) {
	message := NewMessage()
	message.Header.SetString(tagBeginString, BeginStringFIX44)
	message.Header.SetString(tagMsgType, "D")
	message.Header.SetInt(tagMsgSeqNum, 1)
	message.Header.Remove(tagMsgSeqNum)
	message.Header.SetInt(tagMsgSeqNum, 2)
	message.Body.SetString(Tag(11), "original")
	message.Body.Remove(Tag(11))
	message.Body.SetString(Tag(11), "replacement")
	for _, rawBody := range []bool{false, true} {
		name := "field maps"
		appendMessage := message.AppendTo
		if rawBody {
			name = "raw body"
			bodyBytes := []byte("11=replacement\x01")
			appendMessage = func(destination []byte) ([]byte, error) {
				return message.AppendWithBodyBytesTo(destination, bodyBytes)
			}
		}
		t.Run(name, func(t *testing.T) {
			// Remove/re-add can retain duplicate private tag indexes. This test
			// asserts capacity accounting, without defining their wire semantics.
			reference, err := appendMessage(make([]byte, 0, 1024))
			if err != nil {
				t.Fatal(err)
			}
			assertBoundedMessageAppend(t, message, appendMessage, string(reference))
		})
	}
}

func TestMessageAppendToOversizedTagIndexDoesNotAllocateOrMutate(t *testing.T) {
	message := NewMessage()
	message.Header.SetString(tagBeginString, BeginStringFIX44)
	message.Header.SetString(tagMsgType, "D")
	message.Body.SetBytes(Tag(58), bytes.Repeat([]byte{'x'}, 1<<20))
	// This is the retained index shape of repeated Remove/Set operations.
	// Its logical length exceeds MaxInt32 while its actual storage stays small.
	message.Body.tags = make([]Tag, 2048)
	for index := range message.Body.tags {
		message.Body.tags[index] = Tag(58)
	}
	storage := bytes.Repeat([]byte{0xa5}, 128)
	beforeStorage := bytes.Clone(storage)
	beforeHeaderOrder := append([]Tag(nil), message.Header.tags...)
	allocations := testing.AllocsPerRun(100, func() {
		result, err := message.AppendTo(storage[:3])
		if err != ErrBufferTooSmall || len(result) != 3 || cap(result) != len(storage) {
			t.Fatalf("oversized append = len %d cap %d error %v", len(result), cap(result), err)
		}
	})
	if allocations != 0 {
		t.Fatalf("capacity failure allocated %g times per call", allocations)
	}
	if !bytes.Equal(storage, beforeStorage) || !reflect.DeepEqual(message.Header.tags, beforeHeaderOrder) {
		t.Fatal("capacity failure changed output storage or message ordering")
	}
}

func assertBoundedMessageAppend(t *testing.T, message *Message, appendMessage func([]byte) ([]byte, error), expected string) {
	t.Helper()
	const prefix = "already written:"
	for _, capacity := range []int{0, len(expected) - 1} {
		storage := bytes.Repeat([]byte{0xa5}, len(prefix)+capacity)
		copy(storage, prefix)
		beforeStorage := bytes.Clone(storage)
		beforeMessage := snapshotAppendMessage(message)
		result, err := appendMessage(storage[:len(prefix)])
		if err != ErrBufferTooSmall {
			t.Fatalf("AppendTo with %d bytes available: error = %v, want ErrBufferTooSmall", capacity, err)
		}
		if len(result) != len(prefix) || cap(result) != len(storage) || &result[0] != &storage[0] {
			t.Fatal("failed append did not return the original destination")
		}
		if !bytes.Equal(storage, beforeStorage) {
			t.Fatal("failed append changed destination backing storage")
		}
		if !reflect.DeepEqual(snapshotAppendMessage(message), beforeMessage) {
			t.Fatal("failed append changed message state")
		}
	}
	result, err := appendMessage(nil)
	if err != ErrBufferTooSmall || result != nil {
		t.Fatalf("append(nil) = %v, %v; want nil, ErrBufferTooSmall", result, err)
	}

	storage := make([]byte, len(prefix)+len(expected))
	copy(storage, prefix)
	beforeMessage := snapshotAppendMessage(message)
	result, err = appendMessage(storage[:len(prefix)])
	if err != nil {
		t.Fatalf("append with exact capacity: %v", err)
	}
	if string(result) != prefix+expected {
		t.Fatalf("wire mismatch:\n got %q\nwant %q", result, prefix+expected)
	}
	if &result[0] != &storage[0] || cap(result) != cap(storage) {
		t.Fatal("append did not retain caller-owned storage")
	}
	// Mutating the owned output cannot alter message fields or retained raw bytes.
	for index := len(prefix); index < len(result); index++ {
		result[index] = 0
	}
	afterMessage := snapshotAppendMessage(message)
	// Successful encoding may sort the private tag-order caches.
	beforeMessage.order = afterMessage.order
	if !reflect.DeepEqual(afterMessage, beforeMessage) {
		t.Fatal("append changed message field values or output aliases source bytes")
	}
}

type appendMessageSnapshot struct {
	order      [3][]Tag
	fields     [3]map[Tag][]string
	rawMessage string
	bodyBytes  string
	parsed     []string
}

func snapshotAppendMessage(message *Message) appendMessageSnapshot {
	var snapshot appendMessageSnapshot
	for section, fields := range []*FieldMap{&message.Header.FieldMap, &message.Body.FieldMap, &message.Trailer.FieldMap} {
		snapshot.order[section] = append([]Tag(nil), fields.tags...)
		snapshot.fields[section] = make(map[Tag][]string, len(fields.tagLookup))
		for tag, values := range fields.tagLookup {
			for _, value := range values {
				snapshot.fields[section][tag] = append(snapshot.fields[section][tag], string(value.bytes), string(value.value))
			}
		}
	}
	if message.rawMessage != nil {
		snapshot.rawMessage = message.rawMessage.String()
	}
	snapshot.bodyBytes = string(message.bodyBytes)
	for _, value := range message.fields {
		snapshot.parsed = append(snapshot.parsed, string(value.bytes), string(value.value))
	}
	return snapshot
}
