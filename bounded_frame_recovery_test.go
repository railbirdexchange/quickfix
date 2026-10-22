package quickfix

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/internal"
	"github.com/stretchr/testify/require"
)

func TestBoundedFrameRecoveryLegacyBoundaries(t *testing.T) {
	for _, fixture := range []struct {
		name, malformed string
		framingError    error
	}{
		{"short_length", "8=FIX.4.4\x019=4\x0135=0\x0110=163\x01", errInvalidFrameChecksum},
		{"long_length_consumes_following_frame", "8=FIX.4.4\x019=6\x0135=0\x0110=163\x01" + boundedHeartbeatFrame, errInvalidFrameChecksum},
		{"nonnumeric_checksum", "8=FIX.4.4\x019=5\x0135=0\x0110=x63\x01", errInvalidFrameChecksum},
		{"short_checksum", "8=FIX.4.4\x019=5\x0135=0\x0110=0\x01", errInvalidFrameChecksum},
		{"long_checksum", "8=FIX.4.4\x019=5\x0135=0\x0110=0163\x01", errInvalidFrameChecksum},
		{"out_of_order_length", "8=FIX.4.4\x0135=0\x019=5\x0135=0\x0110=163\x01", errInvalidFrameLength},
	} {
		for _, chunk := range []int{1, 7, 0} {
			t.Run(fixture.name+"/chunk_"+strconv.Itoa(chunk), func(t *testing.T) {
				stream := fixture.malformed + boundedHeartbeatFrame
				legacy, err := newParser(strings.NewReader(stream)).ReadMessage()
				require.NoError(t, err)
				require.Equal(t, fixture.malformed, legacy.String(), "recovery must match legacy consumption")
				reader := &boundedFramingReader{remaining: []byte(stream), chunk: chunk, eofWithData: true}
				framer, err := newBoundedParser(reader, len(stream))
				require.NoError(t, err)
				scratch := &framer.bigBuffer[0]
				storage := bytes.Repeat([]byte{0xa5}, len(stream))
				before := bytes.Clone(storage)
				for range 2 {
					frame, err := framer.ReadMessageInto(storage)
					require.ErrorIs(t, err, fixture.framingError, "public strict framing must not recover implicitly")
					require.Empty(t, frame)
					require.Equal(t, before, storage)
				}
				frame, err := framer.recoverMalformedFrameInto(storage)
				require.NoError(t, err)
				require.Equal(t, fixture.malformed, string(frame))
				require.Same(t, &storage[0], &frame[0])
				frame, err = framer.ReadMessageInto(storage)
				require.NoError(t, err)
				require.Equal(t, boundedHeartbeatFrame, string(frame))
				_, err = framer.ReadMessageInto(storage)
				require.ErrorIs(t, err, io.EOF)
				require.Same(t, scratch, &framer.bigBuffer[0])
				require.Equal(t, len(stream), cap(framer.bigBuffer))
			})
		}
	}
}

func TestBoundedFrameRecoveryShortDestinationRetainsMalformedFrame(t *testing.T) {
	const malformed = "8=FIX.4.4\x019=5\x0135=0\x0110=0163\x01"
	framer, err := newBoundedParser(strings.NewReader(malformed+boundedHeartbeatFrame), 128)
	require.NoError(t, err)
	short := bytes.Repeat([]byte{0xa5}, len(malformed)-1)
	before := bytes.Clone(short)
	_, err = framer.ReadMessageInto(short)
	require.ErrorIs(t, err, errInvalidFrameChecksum)
	frame, err := framer.recoverMalformedFrameInto(short[:1])
	require.ErrorIs(t, err, ErrFrameCapacity)
	require.Empty(t, frame)
	require.Equal(t, before, short)
	frame, err = framer.recoverMalformedFrameInto(nil)
	require.ErrorIs(t, err, ErrFrameCapacity)
	require.Nil(t, frame)
	storage := make([]byte, len(malformed))
	frame, err = framer.recoverMalformedFrameInto(storage)
	require.NoError(t, err)
	require.Equal(t, malformed, string(frame))
	frame, err = framer.ReadMessageInto(storage)
	require.NoError(t, err)
	require.Equal(t, boundedHeartbeatFrame, string(frame))
}

func TestBoundedFrameRecoveryTerminalErrorsReturnSlot(t *testing.T) {
	for _, fixture := range []struct {
		name, wire string
		failure    error
	}{
		{"missing_length", "8=FIX.4.4\x0135=0\x01", io.ErrUnexpectedEOF},
		{"empty_length", "8=FIX.4.4\x019=\x01", errInvalidFrameLength},
		{"zero_length", "8=FIX.4.4\x019=0\x01", errInvalidFrameLength},
		{"negative_length", "8=FIX.4.4\x019=-1\x01", errInvalidFrameLength},
		{"overflow", "8=FIX.4.4\x019=999999999999999999999999999999\x01", ErrFrameCapacity},
		{"out_of_order_overflow", "8=FIX.4.4\x0135=0\x019=999999999999999999999999999999\x01", ErrFrameCapacity},
		{"out_of_order_exceeds_remaining_capacity", "8=FIX.4.4\x0135=0\x019=1010\x01", ErrFrameCapacity},
		{"no_checksum_within_capacity", "8=FIX.4.4\x019=1\x0135=0\x01" + strings.Repeat("x", 1024), ErrFrameCapacity},
		{"unterminated_checksum", "8=FIX.4.4\x019=5\x0135=0\x0110=0163", io.ErrUnexpectedEOF},
		{"short_checksum_at_eof", "8=FIX.4.4\x019=5\x0135=0\x0110=0\x01", io.ErrUnexpectedEOF},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			inbound := boundedConnectionTestInbound(t, 0)
			reader := &boundedFramingReader{remaining: []byte(fixture.wire), chunk: 3, eofWithData: true}
			control, incoming, stop := boundedConnectionTestStart(t, boundedConnectionTestPipe(t), newParser(reader), inbound, 0, nil, time.Time{})
			terminal := boundedConnectionTestReceive(t, incoming)
			require.Nil(t, terminal.slot, "unrecoverable reads must not transfer malformed raw as a message")
			require.ErrorIs(t, terminal.readError, fixture.failure)
			boundedConnectionTestWait(t, control.done, "terminal frame error")
			stop()
			assertInboundRingVacant(t, inbound.ring)
		})
	}
}

type boundedFrameRecoveryLog struct {
	inboundRecordingLog
	parseErrors []string
}

func (log *boundedFrameRecoveryLog) OnEventf(format string, args ...interface{}) {
	if format == "Msg Parse Error: %v" {
		log.parseErrors = append(log.parseErrors, args[0].(string))
		return
	}
	log.inboundRecordingLog.OnEventf(format, args...)
}

func TestBoundedFrameRecoveryDropsMalformedBeforeValidSessionMessage(t *testing.T) {
	for _, chunk := range []int{1, 0} {
		t.Run("chunk_"+strconv.Itoa(chunk), func(t *testing.T) {
			current, application, _, store := newInboundIntegrationSession(t, 0, 0)
			log := &boundedFrameRecoveryLog{}
			current.log, current.boundedIn.log = log, log
			current.application = BorrowApplication(application)
			timerFired := make(chan struct{}, 1)
			current.peerTimer = internal.NewEventTimer(func() {
				select {
				case timerFired <- struct{}{}:
				default:
				}
			})
			t.Cleanup(current.peerTimer.Stop)
			current.HeartBtInt = time.Millisecond
			good := inboundIntegrationOrder(1, "after-malformed").build()
			malformed := bytes.Clone(good)
			malformed[len(malformed)-4] = 'x'
			cycles := len(current.boundedIn.ring.slots) + 1
			stream := append(bytes.Repeat(malformed, cycles), good...)
			reader := &boundedFramingReader{remaining: stream, chunk: chunk, eofWithData: true}
			control, incoming, stop := boundedConnectionTestStart(t, boundedConnectionTestPipe(t), newParser(reader), current.boundedIn, 0, nil, time.Time{})
			seen := make(map[*inboundSlot]int)
			creation := store.CreationTime()
			for index := 0; index < cycles; index++ {
				message := boundedConnectionTestReceive(t, incoming)
				require.NotNil(t, message.slot)
				require.ErrorIs(t, message.readError, errInvalidFrameChecksum)
				require.Equal(t, malformed, message.slot.frame)
				require.False(t, message.receiveTime.IsZero())
				seen[message.slot]++
				current.Incoming(current, message)
				require.IsType(t, inSession{}, current.State)
				require.Equal(t, 1, store.NextSenderMsgSeqNum())
				require.Equal(t, 1, store.NextTargetMsgSeqNum())
				require.Equal(t, creation, store.CreationTime())
				require.Empty(t, store.messageMap)
				require.Empty(t, application.borrowed)
				require.Empty(t, application.borrowedAdmin)
				require.Empty(t, current.messageOut)
				require.Len(t, log.borrowed, index+1)
				require.Equal(t, string(malformed), log.borrowed[index])
				require.Len(t, log.parseErrors, index+1)
				require.Equal(t, errInvalidFrameChecksum.Error(), log.parseErrors[index])
				if index == 0 {
					boundedConnectionTestWait(t, timerFired, "peer timer reset by discarded frame")
				}
			}
			require.Less(t, len(seen), cycles, "fixed slots must be recycled across malformed frames")
			message := boundedConnectionTestReceive(t, incoming)
			require.Nil(t, message.readError)
			require.NotNil(t, message.slot)
			require.Equal(t, good, message.slot.frame)
			current.Incoming(current, message)
			require.Equal(t, []inboundCallbackRecord{{1, "after-malformed"}}, application.borrowed)
			require.Equal(t, 2, store.NextTargetMsgSeqNum())
			require.Equal(t, 1, store.NextSenderMsgSeqNum())
			require.Len(t, log.borrowed, cycles+1)
			require.Len(t, log.parseErrors, cycles)
			terminal := boundedConnectionTestReceive(t, incoming)
			require.Nil(t, terminal.slot)
			require.ErrorIs(t, terminal.readError, io.EOF)
			boundedConnectionTestWait(t, control.done, "recovered stream completion")
			stop()
			assertInboundRingVacant(t, current.boundedIn.ring)
		})
	}
}

func TestBoundedFrameRecoveryMarkerSkipsParseAndDispatch(t *testing.T) {
	current, application, _, store := newInboundIntegrationSession(t, 0, 0)
	log := &boundedFrameRecoveryLog{}
	current.log, current.boundedIn.log = log, log
	slot := inboundRingTestAcquire(t, current.boundedIn.ring)
	wire := inboundIntegrationOrder(1, "must-not-dispatch").build()
	slot.frame = append(slot.frame[:0], wire...)
	// A valid-looking prefix in a recovered frame cannot override its framing
	// error. This catches implementations that parse and dispatch before drop.
	current.Incoming(current, fixIn{slot: slot, receiveTime: time.Now(), readError: errInvalidFrameLength})
	require.IsType(t, inSession{}, current.State)
	require.Empty(t, application.borrowed)
	require.Equal(t, 1, store.NextTargetMsgSeqNum())
	require.Equal(t, 1, store.NextSenderMsgSeqNum())
	require.Equal(t, []string{string(wire)}, log.borrowed)
	require.Equal(t, []string{errInvalidFrameLength.Error()}, log.parseErrors)
	assertInboundRingVacant(t, current.boundedIn.ring)
}
