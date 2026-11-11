package quickfix

import (
	"bufio"
	"bytes"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitiatorSocketOutboundBufferSize(t *testing.T) {
	sessionID := SessionID{BeginString: BeginStringFIX42, SenderCompID: "sender", TargetCompID: "target"}
	sessionSettings := NewSessionSettings()
	sessionSettings.Set(config.BeginString, sessionID.BeginString)
	sessionSettings.Set(config.SenderCompID, sessionID.SenderCompID)
	sessionSettings.Set(config.TargetCompID, sessionID.TargetCompID)

	settings := NewSettings()
	settings.GlobalSettings().Set(config.SocketOutboundBufferSize, "128")
	_, err := settings.AddSession(sessionSettings)
	require.NoError(t, err)

	initiator := &Initiator{settings: settings}
	got, err := initiator.socketOutboundBufferSize(sessionID)
	require.NoError(t, err)
	assert.Equal(t, 128, got)

	sessionSettings.Set(config.SocketOutboundBufferSize, "256")
	got, err = initiator.socketOutboundBufferSize(sessionID)
	require.NoError(t, err)
	assert.Equal(t, 256, got)
}

type initiatorReplayApp struct {
	replayTestApp
	logons chan struct{}
}

func (a *initiatorReplayApp) OnLogon(SessionID) { a.logons <- struct{}{} }

func TestInitiatorDisconnectCancelsReplayAndReconnects(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	memory, err := NewMemoryStoreFactory().Create(SessionID{})
	require.NoError(t, err)
	store := &blockingReplayStore{MessageStore: memory, entered: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
	defer close(store.release)
	app := &initiatorReplayApp{replayTestApp: replayTestApp{logout: make(chan struct{}, 2)}, logons: make(chan struct{}, 2)}
	settings := NewSettings()
	for key, value := range map[string]string{
		config.BeginString: "FIX.4.4", config.SenderCompID: "VENUE", config.TargetCompID: "DEALER",
		config.SocketConnectHost: "127.0.0.1", config.SocketConnectPort: strconv.Itoa(listener.Addr().(*net.TCPAddr).Port),
		config.HeartBtInt: "30", config.ReconnectInterval: "1", config.LogoutTimeout: "1",
		config.ResetOnLogon: "N", config.ResetOnDisconnect: "N", config.ResetOnLogout: "N",
	} {
		settings.GlobalSettings().Set(key, value)
	}
	id, err := settings.AddSession(NewSessionSettings())
	require.NoError(t, err)
	initiator, err := NewInitiator(app, replayStoreFactory{store}, settings, NewNullLogFactory())
	require.NoError(t, err)
	require.NoError(t, initiator.Start())
	defer initiator.Stop()
	accept := func() net.Conn {
		require.NoError(t, listener.(*net.TCPListener).SetDeadline(time.Now().Add(5*time.Second)))
		conn, err := listener.Accept()
		require.NoError(t, err)
		require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
		return conn
	}
	read := func(p *parser) *Message {
		raw, err := p.ReadMessage()
		require.NoError(t, err)
		m := NewMessage()
		require.NoError(t, ParseMessage(m, bytes.NewBuffer(raw.Bytes())))
		return m
	}
	logon := func(conn net.Conn, seq int) {
		m := replayWireMessage("A", seq)
		m.Body.SetInt(tagEncryptMethod, 0)
		m.Body.SetInt(tagHeartBtInt, 30)
		_, err := conn.Write(m.build())
		require.NoError(t, err)
		awaitReplaySignal(t, app.logons)
	}
	first := accept()
	defer first.Close()
	p := newParser(bufio.NewReader(first))
	require.Equal(t, "A", messageKind(t, read(p)))
	logon(first, 1)
	require.NoError(t, SendToTarget(starvationMessage(time.Now()), id))
	require.Equal(t, "B", messageKind(t, read(p)))
	_, err = first.Write(replayRequestMessage(2).build())
	require.NoError(t, err)
	awaitReplaySignal(t, store.entered)
	require.NoError(t, first.Close())
	awaitReplaySignal(t, store.exited)
	awaitReplaySignal(t, app.logout)
	second := accept()
	defer second.Close()
	p2 := newParser(bufio.NewReader(second))
	response := read(p2)
	require.Equal(t, "A", messageKind(t, response))
	seq, reject := response.Header.GetInt(tagMsgSeqNum)
	require.Nil(t, reject)
	require.Equal(t, 3, seq)
	logon(second, 3) // The received ResendRequest consumed inbound sequence 2.
	require.NoError(t, SendToTarget(starvationMessage(time.Now()), id))
	live := read(p2)
	require.Equal(t, "B", messageKind(t, live))
	require.False(t, live.Header.Has(tagPossDupFlag))
	require.Empty(t, app.logout, "old connection cleaned up the replacement")
}

func TestInitiatorSocketOutboundBufferSizeRejectsNegativeValue(t *testing.T) {
	settings := NewSettings()
	settings.GlobalSettings().Set(config.SocketOutboundBufferSize, "-1")

	initiator := &Initiator{settings: settings}
	_, err := initiator.socketOutboundBufferSize(SessionID{})
	require.Error(t, err)
}
