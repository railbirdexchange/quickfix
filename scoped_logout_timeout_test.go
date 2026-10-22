package quickfix

import (
	"net"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/internal"
	"github.com/stretchr/testify/require"
)

type scopedLogoutObservationApplication struct {
	Application
	logout    chan struct{}
	fromAdmin chan struct{}
}

func (application *scopedLogoutObservationApplication) OnLogout(SessionID) {
	close(application.logout)
}

func (application *scopedLogoutObservationApplication) FromAdmin(*Message, SessionID) MessageRejectError {
	close(application.fromAdmin)
	return nil
}

type scopedLogoutObservationLog struct {
	Log
	timeout chan struct{}
}

func (log scopedLogoutObservationLog) OnEvent(event string) {
	if event == "Timed out waiting for logout response" {
		close(log.timeout)
	}
	log.Log.OnEvent(event)
}

// A response timeout starts after writer admission, not completed socket write.
// An OnLogout callback without FromAdmin(35=5) therefore does not by itself prove
// that the peer closed the connection or dropped its response.
func TestScopedLogoutTimeoutBeforeCompletedRequestWrite(t *testing.T) {
	current, _, store := newScopedOutboundLifecycleSession(t)
	application := &scopedLogoutObservationApplication{
		Application: current.application,
		logout:      make(chan struct{}),
		fromAdmin:   make(chan struct{}),
	}
	observed := scopedLogoutObservationLog{Log: current.log, timeout: make(chan struct{})}
	current.application, current.log = application, observed
	// Drive the production timeout transition explicitly after observing writer
	// admission. This avoids a wall-clock race while retaining the real timer
	// creation and the real connection/state-machine teardown.
	current.LogoutTimeout = time.Hour

	local, peer := net.Pipe()
	connection := &scopedOutboundGracefulConnection{
		Conn: local, writeStarted: make(chan struct{}), allowWrite: make(chan struct{}),
		readInterrupted: make(chan struct{}), closed: make(chan struct{}),
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		if current.logoutTimeoutTimer != nil {
			current.logoutTimeoutTimer.Stop()
		}
		_ = connection.Close()
		_ = peer.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("timed-out Logout teardown did not join")
		}
	})
	incoming := make(chan fixIn, 1)
	reader, err := startBoundedRead(connection, newParser(connection), current.boundedIn, incoming, nil, time.Time{})
	require.NoError(t, err)
	current.messageIn, current.readConnection = incoming, reader
	writer := newOutboundWriter(connection, 0, time.Second, current.messageEvent)
	current.writeConnection = writer
	go writer.run()

	current.stateMachine.Stop(current)
	require.IsType(t, logoutState{}, current.State)
	require.NotNil(t, current.logoutTimeoutTimer, "response timer must be armed while request write is blocked")
	select {
	case <-connection.writeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Logout request did not reach the blocked writer")
	}
	go func() {
		current.stateMachine.Timeout(current, internal.LogoutTimeout)
		close(done)
	}()
	for _, reached := range []<-chan struct{}{observed.timeout, application.logout, connection.readInterrupted} {
		select {
		case <-reached:
		case <-time.After(2 * time.Second):
			t.Fatal("response timeout did not reach log, callback and read cancellation")
		}
	}
	select {
	case <-application.fromAdmin:
		t.Fatal("observed an incoming admin response despite the blocked request")
	case <-connection.closed:
		t.Fatal("timeout closed the socket before the accepted write completed")
	case <-done:
		t.Fatal("timeout teardown returned before the accepted write completed")
	default:
	}

	close(connection.allowWrite)
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(2*time.Second)))
	wire, err := newParser(peer).ReadMessage()
	require.NoError(t, err)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("completed Logout request did not release teardown")
	}
	message := NewMessage()
	require.NoError(t, ParseMessage(message, wire))
	messageType, reject := message.Header.GetString(tagMsgType)
	require.Nil(t, reject)
	require.Equal(t, "5", messageType)
	require.NoError(t, writer.err)
	require.Equal(t, 2, store.NextSenderMsgSeqNum())
	require.Equal(t, 1, store.NextTargetMsgSeqNum(), "no peer response was processed")
	assertScopedOutboundVacant(t, current)
	assertInboundRingVacant(t, current.boundedIn.ring)
	select {
	case <-connection.closed:
	default:
		t.Fatal("completed timeout teardown did not close the socket")
	}
}
