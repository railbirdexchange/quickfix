package quickfix

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBorrowedOutboundFirstSendCopiesOwnAllSections(t *testing.T) {
	for _, admin := range []bool{false, true} {
		name := "ToApp"
		if admin {
			name = "ToAdmin"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newScopedOutboundBenchmarkWithDelivery(t, true, "", true)
			defer fixture.close()
			message := fixture.message
			if admin {
				message.Header.SetString(tagMsgType, "0")
			}
			header, body, trailer, party := []byte("HEADER"), []byte("123.45"), []byte("SIGNED"), []byte("PARTY")
			message.Header.SetBytes(115, header)
			message.Body.SetBytes(44, body)
			message.Trailer.SetBytes(89, trailer)
			group := NewRepeatingGroup(453, GroupTemplate{GroupElement(448), GroupElement(447), GroupElement(452)})
			entry := group.Add()
			entry.SetBytes(448, party)
			entry.SetString(447, "D")
			entry.SetInt(452, 1)
			message.Body.SetGroup(group)
			retained, sections := NewMessage(), NewMessage()
			retainedGroup := NewRepeatingGroup(453, GroupTemplate{GroupElement(448), GroupElement(447), GroupElement(452)})
			calls := 0
			capture := func(delivered *Message) {
				calls++
				require.Same(t, message, delivered)
				require.Nil(t, delivered.arena, "first send must keep its cold construction representation")
				delivered.CopyInto(retained)
				delivered.Header.CopyInto(&sections.Header.FieldMap)
				delivered.Body.CopyInto(&sections.Body.FieldMap)
				delivered.Trailer.CopyInto(&sections.Trailer.FieldMap)
				require.NoError(t, delivered.Body.GetGroup(retainedGroup))
			}
			fixture.session.application = BorrowApplication(&scopedOutboundLifecycleApplication{
				toApp:   func(delivered *Message) error { capture(delivered); return nil },
				toAdmin: capture,
			})
			require.NoError(t, fixture.process())
			require.Equal(t, 1, calls)
			for _, input := range [][]byte{header, body, trailer, party} {
				for index := range input {
					input[index] = 'X'
				}
			}
			for _, snapshot := range []*Message{retained, sections} {
				assertArenaValue(t, &snapshot.Header.FieldMap, 115, "HEADER")
				assertArenaValue(t, &snapshot.Body.FieldMap, 44, "123.45")
				assertArenaValue(t, &snapshot.Trailer.FieldMap, 89, "SIGNED")
			}
			assertArenaValue(t, &retainedGroup.Get(0).FieldMap, 448, "PARTY")
			assertArenaValue(t, &message.Body.FieldMap, 44, "XXXXXX")
			require.Nil(t, message.arena)
			for _, fields := range []*FieldMap{&message.Header.FieldMap, &message.Body.FieldMap, &message.Trailer.FieldMap} {
				require.False(t, fields.reusable, "callback marker escaped its synchronous scope")
			}
		})
	}
}

func TestBorrowedOutboundCopyScopeRestoresAfterErrorAndPanic(t *testing.T) {
	for _, mode := range []string{"error", "panic_app", "panic_admin"} {
		t.Run(mode, func(t *testing.T) {
			message := NewMessage()
			message.Body.SetString(11, "original")
			failure := errors.New("callback failure")
			callback := func(delivered *Message) error {
				require.True(t, delivered.Header.reusable)
				require.True(t, delivered.Body.reusable)
				require.True(t, delivered.Trailer.reusable)
				if mode != "error" {
					panic(failure)
				}
				return failure
			}
			application := BorrowApplication(&scopedOutboundLifecycleApplication{
				toApp:   callback,
				toAdmin: func(delivered *Message) { _ = callback(delivered) },
			})
			if mode == "error" {
				require.ErrorIs(t, application.ToApp(message, SessionID{}), failure)
			} else {
				require.PanicsWithValue(t, failure, func() {
					if mode == "panic_admin" {
						application.ToAdmin(message, SessionID{})
					} else {
						_ = application.ToApp(message, SessionID{})
					}
				})
			}
			require.False(t, message.Header.reusable)
			require.False(t, message.Body.reusable)
			require.False(t, message.Trailer.reusable)
			assertArenaValue(t, &message.Body.FieldMap, 11, "original")
		})
	}
}

func TestBorrowedOutboundCopyScopePreservesReplacementAndExistingFlags(t *testing.T) {
	for _, warmed := range []bool{false, true} {
		message := NewMessage()
		if warmed {
			message = newArenaTestMessage(t, 1024, 32)
		}
		var replacement Body
		replacement.Init()
		replacement.SetString(11, "replacement")
		retained := NewMessage()
		application := BorrowApplication(&scopedOutboundLifecycleApplication{toApp: func(delivered *Message) error {
			delivered.Body = replacement
			delivered.CopyInto(retained)
			return nil
		}})
		require.NoError(t, application.ToApp(message, SessionID{}))
		require.False(t, message.Body.reusable, "replaced section inherited an unrelated callback marker")
		require.Equal(t, warmed, message.Header.reusable)
		require.Equal(t, warmed, message.Trailer.reusable)
		assertArenaValue(t, &retained.Body.FieldMap, 11, "replacement")
	}
}
