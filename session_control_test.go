package quickfix

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type sessionControlApp struct {
	starvationApp
	logout chan SessionID
}

func (a *sessionControlApp) OnLogout(id SessionID) { a.logout <- id }

type dealerStoreFactory struct{ dealer MessageStore }

func (f dealerStoreFactory) Create(id SessionID) (MessageStore, error) {
	if f.dealer != nil && id.TargetCompID == "DEALER" {
		return f.dealer, nil
	}
	return NewMemoryStoreFactory().Create(id)
}

func startSessionControlAcceptor(t *testing.T, store MessageStore) (*sessionControlApp, int) {
	t.Helper()
	port := freeTCPPort(t)
	settings, err := ParseSettings(strings.NewReader(strings.Join([]string{
		"[DEFAULT]",
		"BeginString=FIX.4.4",
		"SenderCompID=VENUE",
		"DynamicSessions=Y",
		"HeartBtInt=30",
		"UseDataDictionary=N",
		"StartTime=00:00:00",
		"EndTime=00:00:00",
		fmt.Sprintf("SocketAcceptPort=%d", port),
		"[SESSION]",
		"TargetCompID=PATRIOTS",
		"",
	}, "\n")))
	require.NoError(t, err)
	app := &sessionControlApp{starvationApp: starvationApp{loggedOn: make(chan SessionID, 1)}, logout: make(chan SessionID, 2)}
	acceptor, err := NewAcceptor(app, dealerStoreFactory{store}, settings, NewNullLogFactory())
	require.NoError(t, err)
	require.NoError(t, acceptor.Start())
	t.Cleanup(acceptor.Stop)
	return app, port
}

func dialAndLogon(t *testing.T, port int, app *sessionControlApp) (net.Conn, *parser) {
	t.Helper()
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	logon := replayWireMessage("A", 1)
	logon.Body.SetInt(tagEncryptMethod, 0)
	logon.Body.SetInt(tagHeartBtInt, 30)
	logon.Body.SetBool(tagResetSeqNumFlag, true)
	_, err = conn.Write(logon.build())
	require.NoError(t, err)
	p := newParser(bufio.NewReader(conn))
	reply := readWireMessage(t, conn, p)
	require.Equal(t, "A", messageKind(t, reply))
	select {
	case <-app.loggedOn:
	case <-time.After(2 * time.Second):
		t.Fatal("acceptor did not log on")
	}
	return conn, p
}

func readWireMessage(t *testing.T, conn net.Conn, p *parser) *Message {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	raw, err := p.ReadMessage()
	require.NoError(t, err)
	m := NewMessage()
	require.NoError(t, ParseMessage(m, raw))
	return m
}

func outboundNews() *Message {
	m := NewMessage()
	m.Header.SetString(tagMsgType, "B")
	m.Header.SetString(tagBeginString, "FIX.4.4")
	m.Header.SetString(tagSenderCompID, "VENUE")
	m.Header.SetString(tagTargetCompID, "DEALER")
	m.Body.SetString(Tag(148), "Bruins")
	return m
}

func TestTerminateStopsSessionDuringResend(t *testing.T) {
	store := &blockingReplayStore{MessageStore: newRetirementMemoryStore(), entered: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
	app, port := startSessionControlAcceptor(t, store)
	conn, p := dialAndLogon(t, port, app)
	id := SessionID{BeginString: "FIX.4.4", SenderCompID: "VENUE", TargetCompID: "DEALER"}

	_, err := conn.Write(replayRequestMessage(2).build())
	require.NoError(t, err)
	awaitReplaySignal(t, store.entered)

	sent := make(chan error, 1)
	go func() { sent <- SendToTarget(outboundNews(), id) }()
	select {
	case <-sent:
		t.Fatal("send during resend should wait for the resend")
	case <-time.After(200 * time.Millisecond):
	}

	handle, ok := LookupSessionConnection(id)
	require.True(t, ok)
	require.True(t, handle.Registered())
	handle.Terminate()

	awaitReplaySignal(t, store.exited)
	select {
	case got := <-app.logout:
		require.Equal(t, id, got)
	case <-time.After(2 * time.Second):
		t.Fatal("OnLogout did not run after Terminate")
	}
	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked send was not released")
	}
	require.Eventually(t, func() bool { return !handle.Registered() }, 2*time.Second, 10*time.Millisecond)
	_, ok = LookupSessionConnection(id)
	require.False(t, ok)

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	for {
		if _, err := p.ReadMessage(); err != nil {
			require.NotContains(t, err.Error(), "timeout")
			break
		}
	}
}

func TestStaleHandleDoesNotTerminateReplacement(t *testing.T) {
	app, port := startSessionControlAcceptor(t, nil)
	id := SessionID{BeginString: "FIX.4.4", SenderCompID: "VENUE", TargetCompID: "DEALER"}

	dialAndLogon(t, port, app)
	stale, ok := LookupSessionConnection(id)
	require.True(t, ok)
	stale.Terminate()
	<-app.logout
	require.Eventually(t, func() bool { return !stale.Registered() }, 2*time.Second, 10*time.Millisecond)

	dialAndLogon(t, port, app)
	current, ok := LookupSessionConnection(id)
	require.True(t, ok)
	require.True(t, current.Registered())
	require.False(t, stale.Registered())

	stale.Terminate()
	select {
	case <-app.logout:
		t.Fatal("stale handle terminated the replacement connection")
	case <-time.After(200 * time.Millisecond):
	}
	require.True(t, current.Registered())
}

func TestLookupSessionConnectionUnknownSession(t *testing.T) {
	_, ok := LookupSessionConnection(SessionID{BeginString: "FIX.4.4", SenderCompID: "VENUE", TargetCompID: t.Name()})
	require.False(t, ok)
	SessionConnection{}.Terminate()
	require.False(t, SessionConnection{}.Registered())
}
