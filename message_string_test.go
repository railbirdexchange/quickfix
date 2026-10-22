package quickfix

import (
	"bytes"
	"strings"
	"testing"

	"github.com/quickfixgo/quickfix/datadictionary"
)

const messageStringLogonWire = "8=FIX.4.4\x019=49\x0135=A\x0152=20140615-19:49:56\x01553=my_user\x01554=secret\x0110=072\x01"

func newMessageStringFixture(t testing.TB, kind string) *Message {
	t.Helper()
	if kind == "new_order_single" {
		message := createFIX40NewOrderSingle()
		message.Header.SetString(tagSendingTime, "20261019-12:34:56.123")
		return message
	}
	if kind != "session_reject" {
		t.Fatalf("unknown String fixture %q", kind)
	}
	message := NewMessage()
	message.Header.SetString(tagBeginString, BeginStringFIX44)
	message.Header.SetString(tagMsgType, "3")
	message.Header.SetInt(tagMsgSeqNum, 17)
	message.Header.SetString(tagSenderCompID, "SERVER")
	message.Header.SetString(tagTargetCompID, "CLIENT")
	message.Header.SetString(tagSendingTime, "20261019-12:34:56.123")
	message.Body.SetInt(tagRefSeqNum, 12)
	message.Body.SetInt(tagRefTagID, 44)
	message.Body.SetString(tagRefMsgType, "D")
	message.Body.SetInt(tagSessionRejectReason, 1)
	message.Body.SetString(tagText, "Required tag missing")
	return message
}

func TestMessageStringConstructedWireFixtures(t *testing.T) {
	dictionary, err := datadictionary.Parse("spec/FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	// Independent golden wires from the existing rebuild/append fixtures. Clear
	// the raw shortcut to exercise the constructed serializer with parsed groups.
	fixtures := []struct {
		name, input, expected string
		resend                bool
	}{
		{
			name:     "logon",
			input:    messageStringLogonWire,
			expected: messageStringLogonWire,
		},
		{
			name:     "order_with_resend_headers",
			input:    "8=FIX.4.2\x019=104\x0135=D\x0134=2\x0149=TW\x0152=20140515-19:49:56.659\x0156=ISLD\x0111=100\x0121=1\x0140=1\x0154=1\x0155=TSLA\x0160=00010101-00:00:00.000\x0110=039\x01",
			expected: "8=FIX.4.2\x019=131\x0135=D\x0134=2\x0143=Y\x0149=TW\x0152=20140615-19:49:56\x0156=ISLD\x01122=20140515-19:49:56.659\x0111=100\x0121=1\x0140=1\x0154=1\x0155=TSLA\x0160=00010101-00:00:00.000\x0110=122\x01",
			resend:   true,
		},
		{
			name:     "two_reordered_groups",
			input:    "8=FIX.4.4\x019=210\x0135=D\x0134=2\x01347=UTF-8\x0152=20231231-20:19:41\x0149=01001\x0150=01001a\x0156=TEST\x0144=12\x0111=13976\x011=10100400\x0121=1\x01386=1\x01336=NOPL\x0155=SYMABC\x0154=1\x0160=20231231-20:19:41\x0138=1\x0140=2\x0159=0\x01453=1\x01448=4501\x01447=D\x01452=28\x01354=6\x01355=Public\x0110=104\x01",
			expected: "8=FIX.4.4\x019=210\x0135=D\x0134=2\x0149=01001\x0150=01001a\x0152=20231231-20:19:41\x0156=TEST\x01347=UTF-8\x011=10100400\x0111=13976\x0121=1\x0138=1\x0140=2\x0144=12\x0154=1\x0155=SYMABC\x0159=0\x0160=20231231-20:19:41\x01354=6\x01355=Public\x01386=1\x01336=NOPL\x01453=1\x01448=4501\x01447=D\x01452=28\x0110=104\x01",
		},
		{
			name:     "nested_groups_and_following_field",
			input:    "8=FIX.4.4\x019=202\x0135=D\x0134=2\x0149=01001\x0150=01001a\x0152=20231231-20:19:41\x0156=TEST\x011=acct1\x0111=13976\x0121=1\x0138=1\x0140=2\x0144=12\x0154=1\x0155=SYMABC\x0159=0\x0160=20231231-20:19:41\x0178=1\x0179=acct1\x01539=1\x01524=nestedid\x01804=1\x01545=doublenestedid\x0180=100\x0110=117\x01",
			expected: "8=FIX.4.4\x019=202\x0135=D\x0134=2\x0149=01001\x0150=01001a\x0152=20231231-20:19:41\x0156=TEST\x011=acct1\x0111=13976\x0121=1\x0138=1\x0140=2\x0144=12\x0154=1\x0155=SYMABC\x0159=0\x0160=20231231-20:19:41\x0178=1\x0179=acct1\x01539=1\x01524=nestedid\x01804=1\x01545=doublenestedid\x0180=100\x0110=117\x01",
		},
	}
	for _, fixture := range fixtures {
		for _, arena := range []bool{false, true} {
			name := fixture.name + "/ordinary"
			if arena {
				name = fixture.name + "/arena"
			}
			t.Run(name, func(t *testing.T) {
				message := NewMessage()
				if err := ParseMessageWithDataDictionary(message, bytes.NewBufferString(fixture.input), dictionary, dictionary); err != nil {
					t.Fatal(err)
				}
				message.rawMessage = nil
				if arena {
					if err := message.attachMessageArena(4096, 64); err != nil {
						t.Fatal(err)
					}
				}
				if fixture.resend {
					message.Header.SetString(tagOrigSendingTime, "20140515-19:49:56.659")
					message.Header.SetString(tagSendingTime, "20140615-19:49:56")
					message.Header.SetBool(tagPossDupFlag, true)
				}
				if got := message.String(); got != fixture.expected {
					t.Fatalf("String wire = %q, want %q", got, fixture.expected)
				}
			})
		}
	}
}

func TestMessageStringPreservesCookSideEffects(t *testing.T) {
	for _, arena := range []bool{false, true} {
		message := NewMessage()
		message.Header.SetString(tagBeginString, BeginStringFIX44)
		message.Header.SetString(tagMsgType, "A")
		message.Header.SetString(tagSendingTime, "20140615-19:49:56")
		message.Body.SetString(554, "secret")
		message.Body.SetString(553, "my_user")
		if arena {
			if err := message.attachMessageArena(4096, 64); err != nil {
				t.Fatal(err)
			}
		}
		for _, existingGeneratedFields := range []bool{false, true} {
			if existingGeneratedFields {
				message.Header.SetInt(tagBodyLength, 99999)
				message.Trailer.SetString(tagCheckSum, "999")
			}
			if got := message.String(); got != messageStringLogonWire {
				t.Fatalf("arena=%v existing=%v: wire = %q", arena, existingGeneratedFields, got)
			}
			if length, reject := message.Header.GetInt(tagBodyLength); reject != nil || length != 49 {
				t.Fatalf("stored BodyLength = %d, %v; want 49", length, reject)
			}
			if checksum, reject := message.Trailer.GetString(tagCheckSum); reject != nil || checksum != "072" {
				t.Fatalf("stored CheckSum = %q, %v; want 072", checksum, reject)
			}
		}
	}
}

func TestMessageStringRetainsConstructedSnapshot(t *testing.T) {
	for _, arena := range []bool{false, true} {
		message := newMessageStringFixture(t, "new_order_single")
		if arena {
			if err := message.attachMessageArena(4096, 64); err != nil {
				t.Fatal(err)
			}
		}
		retained := message.String()
		expected := strings.Clone(retained)
		message.Header.SetInt(tagMsgSeqNum, 999)
		message.Body.SetString(55, "A LONGER REPLACEMENT SYMBOL")
		if changed := message.String(); changed == retained {
			t.Fatal("constructed String ignored field changes")
		}
		message.Body.Clear()
		message.Body.SetString(11, "replacement order")
		_ = message.String()
		if retained != expected {
			t.Fatalf("arena=%v: retained String changed after message reuse", arena)
		}
	}
}

func TestMessageStringRetainsRawSnapshot(t *testing.T) {
	raw := []byte(messageStringLogonWire)
	message := NewMessage()
	if err := ParseMessage(message, bytes.NewBuffer(raw)); err != nil {
		t.Fatal(err)
	}
	retained := message.String()
	message.Header.SetString(tagSendingTime, "20261019-12:34:56")
	if got := message.String(); got != messageStringLogonWire {
		t.Fatal("raw String stopped using its cached wire after field mutation")
	}
	for index := range raw {
		raw[index] = 'x'
	}
	if err := ParseMessage(message, bytes.NewBufferString(messageStringLogonWire)); err != nil {
		t.Fatal(err)
	}
	if retained != messageStringLogonWire {
		t.Fatal("retained raw String changed after input-buffer and message reuse")
	}
}

func TestMessageStringPreservesCapacityFailure(t *testing.T) {
	t.Run("latched_arena_error", func(t *testing.T) {
		message := newArenaTestMessage(t, 64, 8)
		message.Body.SetString(11, strings.Repeat("x", 64))
		if message.constructionError() != ErrFrameCapacity {
			t.Fatal("fixture did not latch frame exhaustion")
		}
		defer func() {
			if got := recover(); got != ErrFrameCapacity {
				t.Fatalf("String panic = %v, want ErrFrameCapacity", got)
			}
		}()
		_ = message.String()
	})
	t.Run("strict_header_exhausted_during_cook", func(t *testing.T) {
		message := NewMessage()
		message.Header.initFixed(headerFieldOrdering, 2)
		message.Header.SetString(tagBeginString, BeginStringFIX44)
		message.Header.SetString(tagMsgType, "A")
		if err := message.constructionError(); err != nil {
			t.Fatalf("fixture failed before cook: %v", err)
		}
		defer func() {
			if got := recover(); got != ErrFieldCapacity {
				t.Fatalf("String panic = %v, want ErrFieldCapacity", got)
			}
			if message.Header.Has(tagBodyLength) {
				t.Fatal("failed cook inserted an extra header field")
			}
		}()
		_ = message.String()
	})
}
