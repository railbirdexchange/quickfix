package quickfix

import (
	"strconv"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/config"
	"github.com/quickfixgo/quickfix/internal"
	"github.com/stretchr/testify/require"
)

// The test goroutine is the session owner. A separate writer owns each output
// until completion; the fixed sink is inspected only after joining that writer.
func newBoundedRecoverySession(t *testing.T, boundedStore bool) (*session, *inboundRecordingApplication) {
	t.Helper()
	s, recorder, _, _ := newInboundIntegrationSession(t, 1, 3)
	if boundedStore {
		store, err := NewBoundedStore(64, 1024)
		require.NoError(t, err)
		s.store = store
	}
	settings := NewSessionSettings()
	settings.Set(config.OutboundFrameQueueCapacity, "4")
	require.NoError(t, s.configureOutbound(settings))
	connection := &scopedOutboundBenchmarkConnection{wire: make([]byte, 64*1024)}
	s.writeConnection = newOutboundWriter(connection, 4, time.Second, nil)
	go s.writeConnection.run()
	return s, recorder
}

func TestBoundedRecoveryPointerTimeoutAndSecondRecovery(t *testing.T) {
	for _, boundedOutput := range []bool{false, true} {
		t.Run("bounded_output="+strconv.FormatBool(boundedOutput), func(t *testing.T) {
			s, application := newBoundedRecoverySession(t, boundedOutput)
			s.ResendRequestChunkSize = 2
			inboundIntegrationDeliver(t, s, inboundIntegrationOrder(5, "5"))
			recovery, ok := s.State.(*resendState)
			require.True(t, ok)
			require.Same(t, &s.boundedIn.recovery, recovery)
			require.Equal(t, 4, recovery.resendRangeEnd)
			require.Equal(t, 2, recovery.currentResendRangeEnd)
			for range 2 {
				s.Timeout(s, internal.NeedHeartbeat)
				require.Same(t, recovery, s.State)
			}
			require.Equal(t, 4, s.store.NextSenderMsgSeqNum())
			s.Timeout(s, internal.PeerTimeout)
			pending, ok := s.State.(*pendingTimeout)
			require.True(t, ok)
			require.Same(t, &s.boundedIn.pending, pending)
			require.Same(t, recovery, pending.sessionState)
			for _, event := range []internal.Event{internal.NeedHeartbeat, internal.NeedHeartbeat, internal.LogonTimeout, internal.LogoutTimeout} {
				s.Timeout(s, event)
				require.Same(t, pending, s.State)
				require.Same(t, recovery, pending.sessionState, "the reusable wrapper must never embed itself")
			}
			require.Equal(t, 5, s.store.NextSenderMsgSeqNum(), "pending timeout must not send another heartbeat")
			require.Equal(t, 1, s.store.NextTargetMsgSeqNum())

			// A second high frame clears the pending wrapper while preserving the
			// original recovery range and both owned stash slots.
			inboundIntegrationDeliver(t, s, inboundIntegrationOrder(6, "6"))
			require.Same(t, recovery, s.State)
			require.Equal(t, [][2]int{{1, 2}}, application.resends)
			s.Timeout(s, internal.PeerTimeout)
			require.Same(t, pending, s.State)
			require.Same(t, recovery, pending.sessionState)
			for sequence := 1; sequence <= 4; sequence++ {
				inboundIntegrationDeliver(t, s, inboundIntegrationOrder(sequence, strconv.Itoa(sequence)))
				if sequence < 4 {
					require.Same(t, recovery, s.State)
				}
			}
			require.IsType(t, inSession{}, s.State)
			require.Equal(t, [][2]int{{1, 2}, {3, 0}}, application.resends)
			require.Equal(t, 7, s.store.NextTargetMsgSeqNum())
			require.Equal(t, 7, s.store.NextSenderMsgSeqNum())
			assertInboundRingVacant(t, s.boundedIn.ring)
			for index, record := range application.borrowed {
				require.Equal(t, inboundCallbackRecord{index + 1, strconv.Itoa(index + 1)}, record)
			}
			require.Len(t, application.borrowed, 6)

			// Re-enter recovery using the same cells, with a different range.
			// A gap fill discards one stashed frame while preserving the later one.
			inboundIntegrationDeliver(t, s, inboundIntegrationOrder(10, "10"))
			require.Same(t, recovery, s.State)
			require.Equal(t, 9, recovery.resendRangeEnd)
			require.Equal(t, 8, recovery.currentResendRangeEnd)
			s.Timeout(s, internal.PeerTimeout)
			require.Same(t, pending, s.State)
			require.Same(t, recovery, pending.sessionState)
			inboundIntegrationDeliver(t, s, inboundIntegrationOrder(8, "discard-eight"))
			require.Same(t, recovery, s.State)
			factory := MessageFactory{}
			factory.SetNextSeqNum(7)
			gapFill := factory.SequenceReset(9)
			gapFill.Body.SetBool(tagGapFillFlag, true)
			inboundIntegrationDeliver(t, s, gapFill)
			require.Same(t, recovery, s.State)
			require.Equal(t, 9, s.store.NextTargetMsgSeqNum())
			require.Equal(t, [][2]int{{1, 2}, {3, 0}, {7, 8}, {9, 0}}, application.resends)
			require.Len(t, s.boundedIn.ring.vacant, len(s.boundedIn.ring.slots)-1)
			inboundIntegrationDeliver(t, s, inboundIntegrationOrder(9, "9"))
			require.IsType(t, inSession{}, s.State)
			require.Equal(t, 11, s.store.NextTargetMsgSeqNum())
			require.Equal(t, []int{7}, application.borrowedAdmin)
			require.Equal(t, []inboundCallbackRecord{{9, "9"}, {10, "10"}}, application.borrowed[6:])
			assertInboundRingVacant(t, s.boundedIn.ring)
		})
	}
}

func TestBoundedRecoveryPendingPeerTimeoutReleasesStash(t *testing.T) {
	for _, boundedOutput := range []bool{false, true} {
		t.Run("bounded_output="+strconv.FormatBool(boundedOutput), func(t *testing.T) {
			s, application := newBoundedRecoverySession(t, boundedOutput)
			inboundIntegrationDeliver(t, s, inboundIntegrationOrder(3, "three"))
			inboundIntegrationDeliver(t, s, inboundIntegrationOrder(4, "four"))
			recovery := s.State.(*resendState)
			s.Timeout(s, internal.PeerTimeout)
			pending := s.State.(*pendingTimeout)
			require.Same(t, recovery, pending.sessionState)
			beforeSender, beforeTarget := s.store.NextSenderMsgSeqNum(), s.store.NextTargetMsgSeqNum()
			for range 2 {
				s.Timeout(s, internal.NeedHeartbeat)
				require.Same(t, pending, s.State)
			}
			s.Timeout(s, internal.PeerTimeout)
			require.IsType(t, latentState{}, s.State)
			require.Equal(t, beforeSender, s.store.NextSenderMsgSeqNum())
			require.Equal(t, beforeTarget, s.store.NextTargetMsgSeqNum())
			require.Equal(t, 1, application.logouts)
			require.Empty(t, application.borrowed)
			assertInboundRingVacant(t, s.boundedIn.ring)
			assertScopedOutboundVacant(t, s)
		})
	}
}

func TestBoundedRecoveryInSessionPendingCellReuse(t *testing.T) {
	s, application := newBoundedRecoverySession(t, true)
	s.Timeout(s, internal.PeerTimeout)
	pending, ok := s.State.(*pendingTimeout)
	require.True(t, ok)
	require.Same(t, &s.boundedIn.pending, pending)
	require.IsType(t, inSession{}, pending.sessionState)
	s.Timeout(s, internal.NeedHeartbeat)
	require.Same(t, pending, s.State)
	require.Equal(t, 2, s.store.NextSenderMsgSeqNum())
	inboundIntegrationDeliver(t, s, inboundIntegrationOrder(1, "first"))
	require.IsType(t, inSession{}, s.State)
	require.Equal(t, 2, s.store.NextTargetMsgSeqNum())
	s.Timeout(s, internal.PeerTimeout)
	require.Same(t, pending, s.State)
	require.IsType(t, inSession{}, pending.sessionState)
	require.Equal(t, 3, s.store.NextSenderMsgSeqNum())
	s.Timeout(s, internal.PeerTimeout)
	require.IsType(t, latentState{}, s.State)
	require.Equal(t, 3, s.store.NextSenderMsgSeqNum())
	require.Equal(t, 2, s.store.NextTargetMsgSeqNum())
	require.Equal(t, 1, application.logouts)
	assertInboundRingVacant(t, s.boundedIn.ring)
	assertScopedOutboundVacant(t, s)
}
