package quickfix

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type shutdownStore struct {
	MessageStore
	closed atomic.Int32
}

func (s *shutdownStore) Close() error { s.closed.Add(1); return s.MessageStore.Close() }

type shutdownStoreFactory struct {
	dynamic *shutdownStore
	created atomic.Int32
}

func (f *shutdownStoreFactory) Create(id SessionID) (MessageStore, error) {
	if id.TargetCompID == "DEALER" {
		f.created.Add(1)
		return f.dynamic, nil
	}
	return NewMemoryStoreFactory().Create(id)
}

type shutdownApp struct {
	starvationApp
	entered, release chan struct{}
}

func (a *shutdownApp) OnCreate(id SessionID) {
	if id.TargetCompID == "DEALER" && a.entered != nil {
		close(a.entered)
		<-a.release
	}
}

type shutdownLog struct {
	nullLog
	panics chan string
}

func (l *shutdownLog) OnEventf(format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	if strings.Contains(message, "Panic") {
		l.panics <- message
	}
}

type shutdownLogFactory struct {
	LogFactory
	log *shutdownLog
}

func (f shutdownLogFactory) Create() (Log, error) { return f.log, nil }

type shutdownValidator struct{ entered, release chan struct{} }

func (v shutdownValidator) Validate(net.Conn, SessionID) error {
	close(v.entered)
	<-v.release
	return nil
}

func newShutdownAcceptor(t *testing.T, app Application) (*Acceptor, *shutdownStoreFactory, *shutdownLog, string) {
	t.Helper()
	settings := NewSettings()
	for key, value := range map[string]string{
		config.BeginString: "FIX.4.4", config.SenderCompID: "VENUE", config.SocketAcceptHost: "127.0.0.1", config.SocketAcceptPort: "0",
		config.DynamicSessions: "Y", config.LogoutTimeout: "1", config.ResetOnLogon: "N", config.ResetOnLogout: "N", config.ResetOnDisconnect: "N",
	} {
		settings.GlobalSettings().Set(key, value)
	}
	template := NewSessionSettings()
	template.Set(config.TargetCompID, "INTERNAL")
	_, err := settings.AddSession(template)
	require.NoError(t, err)
	memory, err := NewMemoryStoreFactory().Create(SessionID{})
	require.NoError(t, err)
	factory := &shutdownStoreFactory{dynamic: &shutdownStore{MessageStore: memory}}
	log := &shutdownLog{panics: make(chan string, 10)}
	a, err := NewAcceptor(app, factory, settings, shutdownLogFactory{NewNullLogFactory(), log})
	require.NoError(t, err)
	require.NoError(t, a.Start())
	// Ensure the static session has entered its event loop before racing Stop.
	for _, s := range a.sessions {
		s.waitForInSessionTime()
	}
	var address string
	for _, listener := range a.listeners {
		address = listener.Addr().String()
	}
	return a, factory, log, address
}

func TestAcceptorShutdownDuringDynamicSessionCreation(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	app := &shutdownApp{entered: make(chan struct{}), release: release}
	a, factory, log, address := newShutdownAcceptor(t, app)
	stopped := make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { go func() { a.Stop(); close(stopped) }() }) }
	client, err := net.Dial("tcp", address)
	require.NoError(t, err)
	t.Cleanup(func() { unblock(); client.Close(); stop(); awaitReplaySignal(t, stopped) })
	m := replayWireMessage("A", 1)
	m.Body.SetInt(tagEncryptMethod, 0)
	m.Body.SetInt(tagHeartBtInt, 30)
	_, err = client.Write(m.build())
	require.NoError(t, err)
	awaitReplaySignal(t, app.entered)
	stop()
	select {
	case <-stopped:
		t.Error("Stop returned while a dynamic session was still being registered")
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))
	// Drain any Logon/Logout output until shutdown closes the socket.
	buffer := make([]byte, 4096)
	for {
		_, err = client.Read(buffer)
		if err != nil {
			break
		}
	}
	if timeout, ok := err.(net.Error); ok {
		require.False(t, timeout.Timeout(), "connection handler leaked during shutdown")
	}
	awaitReplaySignal(t, stopped)
	assert.Empty(t, log.panics, "shutdown must not recover from a send-on-closed-channel panic")
	assert.EqualValues(t, 1, factory.dynamic.closed.Load(), "created session store must be closed exactly once")
	id := SessionID{BeginString: "FIX.4.4", SenderCompID: "VENUE", TargetCompID: "DEALER"}
	_, exists := lookupSession(id)
	assert.False(t, exists, "retired dynamic session remained registered")
	if exists {
		_ = UnregisterSession(id)
		_ = factory.dynamic.Close()
	}
}

func TestAcceptorRejectsDynamicRegistrationAfterShutdown(t *testing.T) {
	a, factory, log, address := newShutdownAcceptor(t, &shutdownApp{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	validator := shutdownValidator{entered: make(chan struct{}), release: release}
	a.SetConnectionValidator(validator)
	client, err := net.Dial("tcp", address)
	require.NoError(t, err)
	t.Cleanup(func() { unblock(); client.Close() })
	m := replayWireMessage("A", 1)
	m.Body.SetInt(tagEncryptMethod, 0)
	m.Body.SetInt(tagHeartBtInt, 30)
	_, err = client.Write(m.build())
	require.NoError(t, err)
	awaitReplaySignal(t, validator.entered)
	a.Stop()
	unblock()
	require.NoError(t, client.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, err = client.Read(make([]byte, 4096))
	require.Error(t, err)
	if timeout, ok := err.(net.Error); ok {
		require.False(t, timeout.Timeout(), "late connection handler leaked")
	}
	assert.Zero(t, factory.created.Load(), "shutdown must reject registration before opening a store")
	assert.Empty(t, log.panics, "late registration must not send on a closed channel")
	id := SessionID{BeginString: "FIX.4.4", SenderCompID: "VENUE", TargetCompID: "DEALER"}
	if _, exists := lookupSession(id); exists {
		t.Error("late session remained registered")
		_ = UnregisterSession(id)
		_ = factory.dynamic.Close()
	}
}
