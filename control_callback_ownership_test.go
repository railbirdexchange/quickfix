package quickfix

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

type ownedControlCallbackRecord struct {
	message *Message
	shallow Message
	header  Header
	body    Body
	value   []byte
	seq     []byte
	want    string
	wantSeq string
}

func TestOrdinaryControlCallbacksRetainMessagesAndSections(t *testing.T) {
	for _, scenario := range []struct {
		name string
		send func(*session, *Message) error
	}{
		{"heartbeat", func(s *session, _ *Message) error { return s.sendHeartbeat(nil, nil) }},
		{"test_request", func(s *session, _ *Message) error { return s.sendTestRequest() }},
		{"resend_request", func(s *session, _ *Message) error { _, err := s.sendResendRequest(10, 20); return err }},
		{"gap_fill", func(s *session, message *Message) error { return s.generateSequenceReset(10, 20, message) }},
		{"logout", func(s *session, _ *Message) error { return s.sendLogout("Completed") }},
		{"logon", func(s *session, _ *Message) error { return s.sendLogonInReplyTo(false, nil) }},
		{"reject", func(s *session, message *Message) error {
			return s.doReject(message, NewMessageRejectError("incorrect value", 5, nil))
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture, application := newScopedControlFixture(t)
			defer fixture.close()
			var retained []ownedControlCallbackRecord
			application.hook = func(message *Message) {
				require.NotSame(t, fixture.session.controls.message, message, "ordinary ToAdmin received the reusable control")
				value := "retained-" + strconv.Itoa(len(retained))
				message.Body.SetString(tagTestReqID, value)
				message.Header.SetInt(tagMsgSeqNum, 700+len(retained))
				view, reject := message.Body.GetBytes(tagTestReqID)
				require.Nil(t, reject)
				sequence, reject := message.Header.GetBytes(tagMsgSeqNum)
				require.Nil(t, reject)
				// These are the actual callback value, byte views and shallow
				// copies. Deep copies would hide recycling of legacy delivery.
				retained = append(retained, ownedControlCallbackRecord{
					message: message, shallow: *message, header: message.Header, body: message.Body,
					value: view, seq: sequence, want: value, wantSeq: string(sequence),
				})
			}
			for iteration := 0; iteration < 3; iteration++ {
				sequence := fixture.store.NextSenderMsgSeqNum()
				require.NoError(t, scenario.send(fixture.session, fixture.message))
				require.NoError(t, completeScopedControl(fixture))
				peer := fixture.peer.Message()
				actual, reject := peer.Body.GetString(tagTestReqID)
				require.Nil(t, reject)
				require.Equal(t, retained[iteration].want, actual)
				actual, reject = peer.Header.GetString(tagMsgSeqNum)
				require.Nil(t, reject)
				require.Equal(t, retained[iteration].wantSeq, actual)
				if fixture.store.NextSenderMsgSeqNum() != sequence {
					persisted, present := fixture.store.message(sequence)
					require.True(t, present)
					require.Equal(t, fixture.connection.wire[:fixture.connection.used], persisted)
					fixture.floor++
					require.NoError(t, fixture.store.RetireBefore(fixture.floor))
				}
				fixture.connection.used = 0
			}
			require.Len(t, retained, 3)
			seen := make(map[*Message]bool)
			for _, record := range retained {
				require.False(t, seen[record.message], "ordinary callback identity was recycled")
				seen[record.message] = true
				require.Equal(t, record.want, string(record.value))
				require.Equal(t, record.wantSeq, string(record.seq))
				for _, body := range []*Body{&record.message.Body, &record.shallow.Body, &record.body} {
					value, reject := body.GetString(tagTestReqID)
					require.Nil(t, reject)
					require.Equal(t, record.want, value)
				}
				for _, header := range []*Header{&record.message.Header, &record.shallow.Header, &record.header} {
					value, reject := header.GetString(tagMsgSeqNum)
					require.Nil(t, reject)
					require.Equal(t, record.wantSeq, value)
				}
			}
		})
	}
}

func TestOrdinaryControlCallbackReplacementReachesCompletedWriter(t *testing.T) {
	for _, replacement := range []string{"fields", "body", "message"} {
		t.Run(replacement, func(t *testing.T) {
			fixture, application := newScopedControlFixture(t)
			defer fixture.close()
			var retained, template *Message
			application.hook = func(message *Message) {
				require.NotSame(t, fixture.session.controls.message, message)
				retained = message
				switch replacement {
				case "body":
					template = NewMessage()
					template.Body.SetString(tagTestReqID, "callback-body")
					message.Body = template.Body
				case "message":
					template = NewMessage()
					message.Header.CopyInto(&template.Header.FieldMap)
					template.Body.SetString(tagTestReqID, "callback-message")
					*message = *template
				default:
					message.Body.SetString(tagTestReqID, "callback-fields")
				}
				message.Header.SetInt(tagMsgSeqNum, 777)
			}
			sequence := fixture.store.NextSenderMsgSeqNum()
			require.NoError(t, fixture.session.sendHeartbeat(nil, nil))
			// Recycle the engine cell before observing completion. The writer
			// must encode the application's replacement, then own its wire.
			next, err := fixture.session.acquireControl("1")
			require.NoError(t, err)
			next.Body.SetString(tagTestReqID, "next-control")
			fixture.session.releaseControl()
			require.NoError(t, completeScopedControl(fixture))
			for _, message := range []*Message{retained, fixture.peer.Message()} {
				value, reject := message.Body.GetString(tagTestReqID)
				require.Nil(t, reject)
				require.Equal(t, "callback-"+replacement, value)
				wireSequence, reject := message.Header.GetInt(tagMsgSeqNum)
				require.Nil(t, reject)
				require.Equal(t, 777, wireSequence)
			}
			if template != nil {
				value, reject := template.Body.GetString(tagTestReqID)
				require.Nil(t, reject)
				require.Equal(t, "callback-"+replacement, value)
			}
			persisted, present := fixture.store.message(sequence)
			require.True(t, present)
			require.Equal(t, fixture.connection.wire[:fixture.connection.used], persisted)
			require.Equal(t, sequence+1, fixture.store.NextSenderMsgSeqNum())
		})
	}
}

func TestOrdinaryControlConstructionOverflowReturnsError(t *testing.T) {
	for _, gapFill := range []bool{false, true} {
		name := "heartbeat"
		if gapFill {
			name = "gap_fill"
		}
		t.Run(name, func(t *testing.T) {
			fixture, application := newScopedControlFixture(t)
			defer fixture.close()
			originalID := fixture.session.sessionID
			defer func() { fixture.session.sessionID = originalID }()
			fixture.session.sessionID.SenderCompID = string(bytes.Repeat([]byte{'X'}, fixture.session.boundedIn.limits.MaxFrameBytes))
			sender, target := fixture.store.NextSenderMsgSeqNum(), fixture.store.NextTargetMsgSeqNum()
			var sendError error
			require.NotPanics(t, func() {
				if gapFill {
					sendError = fixture.session.generateSequenceReset(10, 20, nil)
				} else {
					sendError = fixture.session.sendHeartbeat(nil, nil)
				}
			}, "the owned callback adapter must not CopyInto an invalid control")
			require.ErrorIs(t, sendError, ErrFrameCapacity)
			require.Zero(t, application.calls)
			require.False(t, fixture.session.controls.busy)
			require.Equal(t, sender, fixture.store.NextSenderMsgSeqNum())
			require.Equal(t, target, fixture.store.NextTargetMsgSeqNum())
			require.Len(t, fixture.session.outbound.vacant, len(fixture.session.outbound.slots))
			require.Zero(t, fixture.connection.used)
			fixture.session.sessionID = originalID
			require.NoError(t, fixture.session.sendHeartbeat(nil, nil))
			require.NoError(t, completeScopedControl(fixture))
		})
	}
}

func TestOrdinaryOutboundDetachesPreviouslyBorrowedStorage(t *testing.T) {
	for _, previouslyBorrowed := range []bool{false, true} {
		name := "always_owned"
		if previouslyBorrowed {
			name = "borrowed_then_owned"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newScopedOutboundBenchmark(t, false, "")
			defer fixture.close()
			fixture.session.application = fixture.application
			if previouslyBorrowed {
				fixture.session.application = BorrowApplication(fixture.application)
			}
			for range 3 {
				require.NoError(t, fixture.process())
			}
			if previouslyBorrowed {
				require.NotNil(t, fixture.message.arena, "fixture did not acquire the borrowed message arena")
			} else {
				require.Nil(t, fixture.message.arena, "ordinary sends attached a reusable message arena")
			}
			application := &scopedOutboundHookApplication{scopedOutboundBenchmarkApplication: fixture.application}
			fixture.session.application = application
			var retained *Message
			application.toApp = func(message *Message) error {
				require.Same(t, fixture.message, message, "ownership adaptation replaced the caller's Message pointer")
				require.Nil(t, message.arena, "ordinary callback still receives a reusable arena")
				retained = message
				return nil
			}
			fixture.message.Body.SetString(11, "first-owned-body")
			require.NoError(t, fixture.process())
			require.Same(t, fixture.message, retained)
			type retainedField struct {
				value []byte
				want  string
			}
			var fields []retainedField
			for _, field := range []struct {
				section *FieldMap
				tag     Tag
			}{
				{&retained.Header.FieldMap, tagMsgSeqNum},
				{&retained.Header.FieldMap, tagSendingTime},
				{&retained.Header.FieldMap, tagBodyLength},
				{&retained.Trailer.FieldMap, tagCheckSum},
				{&retained.Body.FieldMap, 11},
			} {
				value, reject := field.section.GetBytes(field.tag)
				require.Nil(t, reject)
				fields = append(fields, retainedField{value: value, want: string(value)})
			}
			for iteration := 0; iteration < 5; iteration++ {
				fixture.message.Body.SetString(11, "later-owned-body-with-different-length-"+strconv.Itoa(iteration))
				require.NoError(t, fixture.process())
				require.Nil(t, fixture.message.arena)
				for _, field := range fields {
					require.Equal(t, field.want, string(field.value), "a later ordinary mutation overwrote a retained getter")
				}
			}
		})
	}
}
