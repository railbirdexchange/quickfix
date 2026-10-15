package quickfix

import (
	"time"

	"github.com/quickfixgo/quickfix/internal"
)

// resendRange runs on the session owner. Journal bytes remain borrowed only
// while this call is active; replay never reserves or changes journal entries.
// The separate replay builder preserves accepted callback header changes while
// the ordinary builder emits any preceding gap fill.
func (out *boundedOutboundSession) resendRange(begin, end int, inReplyTo *Message) error {
	if err := out.store.retainedRange(begin, end); err != nil {
		return err
	}
	defer out.replay.Reset()
	defer func() {
		out.replayBuilder.Reset()
		out.replayBuilder.active = false
	}()

	nextSequence := begin
	searchSequence := begin
	for {
		index := out.store.nextEntry(searchSequence, end)
		if index < 0 {
			break
		}
		sequence := out.store.entries[index].sequence
		searchSequence = sequence + 1 // retainedRange excludes maxInt.
		wire, found := out.store.message(sequence)
		if !found {
			return ErrJournalRangeUnavailable
		}
		if err := out.replay.Parse(wire); err != nil {
			return err
		}
		message := out.replay.Message()
		messageType, err := message.Header.GetBytes(tagMsgType)
		if err != nil {
			return err
		}
		storedSequence, err := message.Header.GetInt(tagMsgSeqNum)
		if err != nil {
			return err
		}
		if storedSequence != sequence {
			return ErrInvalidJournalSequence
		}
		if isAdminMessageType(messageType) {
			continue
		}

		builder := out.replayBuilder
		builder.active = true
		builder.Reset()
		if err := builder.CopyFrom(message); err != nil {
			return err
		}
		builder.Header().SetBool(tagPossDupFlag, true)
		if original, err := message.Header.GetBytes(tagSendingTime); err == nil {
			builder.Header().SetBytes(tagOrigSendingTime, original)
		}
		builder.Header().SetTime(tagSendingTime, boundedSendingTime(out.session))
		if err := builder.Err(); err != nil {
			return err
		}
		callbackErr := out.application.ToAppBorrowed(builder, out.session.sessionID)
		if err := builder.Err(); err != nil {
			return err
		}
		if callbackErr != nil {
			// Legacy resend treats a ToApp error as a veto and gap-fills it.
			builder.active = false
			continue
		}
		if nextSequence != sequence {
			if err := out.gapFill(nextSequence, sequence, inReplyTo); err != nil {
				return err
			}
		}
		out.session.log.OnEventf("Resending Message: %v", sequence)
		// Legacy resend also preserves the stored body regardless of ToApp
		// body mutations, including its original repeating-group field order.
		body := message.bodyBytes
		if body == nil {
			// A non-nil empty body selects the raw-body encoding path too.
			body = wire[:0]
		}
		if err := out.sendMessage(builder, inReplyTo, body, sequence, true); err != nil {
			return err
		}
		builder.active = false
		nextSequence = sequence + 1
	}
	if nextSequence <= end {
		return out.gapFill(nextSequence, end+1, inReplyTo)
	}
	return nil
}

func boundedSendingTime(session *session) FIXUTCTimestamp {
	precision := session.timestampPrecision
	if session.sessionID.BeginString < BeginStringFIX42 {
		precision = Seconds
	}
	return FIXUTCTimestamp{Time: time.Now().UTC(), Precision: precision}
}

func (out *boundedOutboundSession) gapFill(begin, end int, inReplyTo *Message) error {
	builder, err := out.begin(msgTypeSequenceReset)
	if err != nil {
		return err
	}
	defer out.abort()
	out.fillHeader(builder, inReplyTo)
	builder.Header().SetInt(tagMsgSeqNum, begin)
	builder.Header().SetBool(tagPossDupFlag, true)
	builder.Body().SetInt(tagNewSeqNo, end)
	builder.Body().SetBool(tagGapFillFlag, true)
	if sendingTime, err := builder.Message().Header.GetBytes(tagSendingTime); err == nil {
		builder.Header().SetBytes(tagOrigSendingTime, sendingTime)
	}
	if err := builder.Err(); err != nil {
		return err
	}
	if err := out.sendBuilder(inReplyTo, nil, begin); err != nil {
		return err
	}
	out.session.log.OnEventf("Sent SequenceReset TO: %v", end)
	return nil
}

func (state inSession) boundedTimeout(session *session, event internal.Event) sessionState {
	switch event {
	case internal.NeedHeartbeat:
		builder, err := session.boundedOut.begin(msgTypeHeartbeat)
		if err != nil {
			return handleStateError(session, err)
		}
		defer session.boundedOut.abort()
		if err := session.boundedOut.sendMessage(builder, nil, nil, 0, false); err != nil {
			return handleStateError(session, err)
		}
	case internal.PeerTimeout:
		builder, err := session.boundedOut.begin(msgTypeTestRequest)
		if err != nil {
			return handleStateError(session, err)
		}
		defer session.boundedOut.abort()
		builder.Body().SetString(tagTestReqID, "TEST")
		if err := session.boundedOut.sendMessage(builder, nil, nil, 0, false); err != nil {
			return handleStateError(session, err)
		}
		session.log.OnEvent("Sent test request TEST")
		session.peerTimer.Reset(time.Duration(float64(1.2) * float64(session.HeartBtInt)))
		return pendingTimeout{state}
	}
	return state
}

// Verification has already run before entering this bounded branch.
func (state inSession) boundedHandleTestRequest(session *session, message *Message) sessionState {
	testRequestID, err := message.Body.GetBytes(tagTestReqID)
	if err != nil {
		session.log.OnEvent("Test Request with no testRequestID")
	} else {
		builder, err := session.boundedOut.begin(msgTypeHeartbeat)
		if err != nil {
			return handleStateError(session, err)
		}
		defer session.boundedOut.abort()
		builder.Body().SetBytes(tagTestReqID, testRequestID)
		if err := session.boundedOut.sendMessage(builder, message, nil, 0, false); err != nil {
			return handleStateError(session, err)
		}
	}
	if err := session.store.IncrNextTargetMsgSeqNum(); err != nil {
		return handleStateError(session, err)
	}
	return state
}
