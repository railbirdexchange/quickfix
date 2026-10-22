package quickfix

import (
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/config"
	"github.com/stretchr/testify/require"
)

type applicationBoundaryObservation struct {
	message       *Message
	bytes         []byte
	wire          []byte
	value         string
	raw           string
	tag           Tag
	borrowed      bool
	frameAttached bool
}

type applicationBoundaryApplication struct {
	borrowed bool
	response *Message
	logons   chan SessionID
	logouts  chan SessionID
	apps     chan applicationBoundaryObservation
	admins   chan applicationBoundaryObservation
	errors   chan error
}

func newApplicationBoundaryApplication(borrowed, respond bool) *applicationBoundaryApplication {
	application := &applicationBoundaryApplication{
		borrowed: borrowed, logons: make(chan SessionID, 4), logouts: make(chan SessionID, 4),
		apps: make(chan applicationBoundaryObservation, 4), admins: make(chan applicationBoundaryObservation, 8),
		errors: make(chan error, 4),
	}
	if respond {
		application.response = newBoundedFullResponse(false)
	}
	return application
}

func (*applicationBoundaryApplication) OnCreate(SessionID) {}
func (application *applicationBoundaryApplication) OnLogon(id SessionID) {
	application.logons <- id
}
func (application *applicationBoundaryApplication) OnLogout(id SessionID) {
	application.logouts <- id
}
func (*applicationBoundaryApplication) ToAdmin(*Message, SessionID)     {}
func (*applicationBoundaryApplication) ToApp(*Message, SessionID) error { return nil }

func (application *applicationBoundaryApplication) observe(message *Message, tag Tag) (applicationBoundaryObservation, MessageRejectError) {
	fields := &message.Body.FieldMap
	if tag == tagMsgType {
		fields = &message.Header.FieldMap
	}
	value, reject := fields.GetBytes(tag)
	if reject != nil {
		return applicationBoundaryObservation{}, reject
	}
	observation := applicationBoundaryObservation{
		value: string(value), tag: tag, borrowed: message.parseSlot != nil,
		frameAttached: message.inbound != nil,
	}
	if !application.borrowed {
		// Keep the actual callback pointers and byte views. No CopyInto or clone
		// can hide a delivery-lifetime regression in the default application.
		observation.message, observation.bytes = message, value
		observation.wire = message.Bytes()
		observation.raw = string(observation.wire)
	}
	return observation, nil
}

func (application *applicationBoundaryApplication) FromAdmin(message *Message, _ SessionID) MessageRejectError {
	observation, reject := application.observe(message, tagMsgType)
	if reject != nil {
		return reject
	}
	if observation.value == "A" || observation.value == "5" {
		application.admins <- observation
	} else if observation.value == "3" {
		application.errors <- fmt.Errorf("received FIX reject: %s", message.String())
	}
	return nil
}

func (application *applicationBoundaryApplication) FromApp(message *Message, id SessionID) MessageRejectError {
	observation, reject := application.observe(message, 11)
	if reject != nil {
		return reject
	}
	application.apps <- observation
	if application.response == nil {
		return nil
	}
	value, _ := message.Body.GetBytes(11)
	application.response.Body.SetBytes(11, value)
	application.response.Body.SetBytes(37, value)
	application.response.Body.SetBytes(17, value)
	application.response.Body.SetInt(38, 1)
	application.response.Body.SetInt(151, 1)
	if err := SendToTarget(application.response, id); err != nil {
		application.errors <- err
		return NewMessageRejectError(err.Error(), 0, nil)
	}
	return nil
}

// The public listener callback installs only a real loopback TCP listener.
// Keeping accepted connections lets the test exercise an actual disconnect;
// no session state, reader, writer or dispatch hook is replaced.
type applicationBoundaryListener struct {
	net.Listener
	accepted chan net.Conn
}

func (listener *applicationBoundaryListener) Accept() (net.Conn, error) {
	connection, err := listener.Listener.Accept()
	if err == nil {
		listener.accepted <- connection
	}
	return connection, err
}

func applicationBoundarySettings(t *testing.T, sender, target, port string, initiate bool) (*Settings, SessionID) {
	t.Helper()
	settings := NewSettings()
	global := settings.GlobalSettings()
	global.Set(config.DataDictionary, "spec/FIX44.xml")
	global.Set(config.HeartBtInt, "60")
	global.Set(config.TimeStampPrecision, "MILLIS")
	if initiate {
		global.Set(config.SocketConnectHost, "127.0.0.1")
		global.Set(config.SocketConnectPort, port)
		global.Set(config.ReconnectInterval, "100ms")
	} else {
		global.Set(config.SocketAcceptHost, "127.0.0.1")
		global.Set(config.SocketAcceptPort, port)
	}
	sessionSettings := NewSessionSettings()
	sessionSettings.Set(config.BeginString, BeginStringFIX44)
	sessionSettings.Set(config.SenderCompID, sender)
	sessionSettings.Set(config.TargetCompID, target)
	id, err := settings.AddSession(sessionSettings)
	require.NoError(t, err)
	return settings, id
}

func waitApplicationBoundaryLifecycle(t *testing.T, events <-chan SessionID, errors <-chan error, want SessionID) {
	t.Helper()
	select {
	case id := <-events:
		require.Equal(t, want, id)
	case err := <-errors:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for public session lifecycle callback")
	}
}

func waitApplicationBoundaryMessage(t *testing.T, events <-chan applicationBoundaryObservation, errors <-chan error, value string, borrowed bool) applicationBoundaryObservation {
	t.Helper()
	select {
	case observation := <-events:
		require.Equal(t, value, observation.value)
		require.Equal(t, borrowed, observation.borrowed)
		require.Equal(t, borrowed, observation.frameAttached)
		return observation
	case err := <-errors:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for peer-validated public session message")
	}
	return applicationBoundaryObservation{}
}

func assertApplicationBoundaryRetained(t *testing.T, observations []applicationBoundaryObservation) {
	t.Helper()
	seen := make(map[*Message]bool, len(observations))
	for _, observation := range observations {
		require.NotNil(t, observation.message)
		require.False(t, seen[observation.message], "owned callbacks reused a message identity")
		seen[observation.message] = true
		require.Equal(t, observation.value, string(observation.bytes))
		require.Equal(t, observation.raw, string(observation.wire))
		require.Equal(t, observation.raw, string(observation.message.Bytes()))
		require.Nil(t, observation.message.inbound)
		require.Nil(t, observation.message.parseSlot)
		fields := &observation.message.Body.FieldMap
		if observation.tag == tagMsgType {
			fields = &observation.message.Header.FieldMap
		}
		value, reject := fields.GetBytes(observation.tag)
		require.Nil(t, reject)
		require.Equal(t, observation.value, string(value))
	}
}

// This is a correctness proof of the public constructors and default wiring.
// Both delivery contracts use the same configured engine, real TCP, dictionary
// validation and persistence. It makes no allocation or latency assertion.
func TestApplicationBoundaryPublicConstructors(t *testing.T) {
	for _, borrowed := range []bool{false, true} {
		name := "owned"
		if borrowed {
			name = "borrowed"
		}
		t.Run(name, func(t *testing.T) {
			underlying, err := net.Listen("tcp4", "127.0.0.1:0")
			require.NoError(t, err)
			listener := &applicationBoundaryListener{Listener: underlying, accepted: make(chan net.Conn, 4)}
			t.Cleanup(func() { _ = listener.Close() })
			port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
			serverSettings, serverID := applicationBoundarySettings(t, "BOUNDARY-SERVER-"+name, "BOUNDARY-CLIENT-"+name, port, false)
			clientSettings, clientID := applicationBoundarySettings(t, serverID.TargetCompID, serverID.SenderCompID, port, true)
			serverApplication := newApplicationBoundaryApplication(borrowed, true)
			clientApplication := newApplicationBoundaryApplication(borrowed, false)
			var serverCallback, clientCallback Application = serverApplication, clientApplication
			if borrowed {
				serverCallback, clientCallback = BorrowApplication(serverApplication), BorrowApplication(clientApplication)
			}
			acceptor, err := NewAcceptor(serverCallback, NewBoundedStoreFactory(256, 16384), serverSettings, NewNullLogFactory())
			require.NoError(t, err)
			acceptor.SetNewListenerCallback(func(string, *tls.Config) (net.Listener, error) { return listener, nil })
			require.NoError(t, acceptor.Start())
			serverStopped := false
			t.Cleanup(func() {
				if !serverStopped {
					acceptor.Stop()
				}
			})
			initiator, err := NewInitiator(clientCallback, NewBoundedStoreFactory(256, 16384), clientSettings, NewNullLogFactory())
			require.NoError(t, err)
			require.NoError(t, initiator.Start())
			clientStopped := false
			t.Cleanup(func() {
				if !clientStopped {
					initiator.Stop()
				}
			})

			request := NewMessage()
			request.Header.SetString(tagMsgType, "D")
			request.Body.SetString(21, "1")
			request.Body.SetString(55, "SYMABC")
			request.Body.SetString(54, "1")
			request.Body.SetInt(38, 1)
			request.Body.SetString(40, "2")
			request.Body.SetString(44, "12.50")
			var retained []applicationBoundaryObservation
			for cycle := 0; cycle < 2; cycle++ {
				waitApplicationBoundaryLifecycle(t, serverApplication.logons, serverApplication.errors, serverID)
				waitApplicationBoundaryLifecycle(t, clientApplication.logons, clientApplication.errors, clientID)
				serverLogon := waitApplicationBoundaryMessage(t, serverApplication.admins, serverApplication.errors, "A", borrowed)
				clientLogon := waitApplicationBoundaryMessage(t, clientApplication.admins, clientApplication.errors, "A", borrowed)
				if !borrowed {
					retained = append(retained, serverLogon, clientLogon)
				}
				var connection net.Conn
				select {
				case connection = <-listener.accepted:
				case <-time.After(10 * time.Second):
					t.Fatal("public acceptor did not accept the TCP connection")
				}
				// More than the 35 default inbound slots forces frame reuse on
				// both peers before retained callback values are inspected.
				for order := 0; order < 48; order++ {
					identifier := strconv.Itoa(cycle*48 + order + 1)
					request.Body.SetString(11, identifier)
					request.Body.SetString(60, time.Now().UTC().Format("20060102-15:04:05.000"))
					require.NoError(t, SendToTarget(request, clientID))
					incoming := waitApplicationBoundaryMessage(t, serverApplication.apps, serverApplication.errors, identifier, borrowed)
					response := waitApplicationBoundaryMessage(t, clientApplication.apps, clientApplication.errors, identifier, borrowed)
					if !borrowed {
						retained = append(retained, incoming, response)
					}
				}
				if cycle == 0 {
					require.NoError(t, connection.Close())
					waitApplicationBoundaryLifecycle(t, serverApplication.logouts, serverApplication.errors, serverID)
					waitApplicationBoundaryLifecycle(t, clientApplication.logouts, clientApplication.errors, clientID)
				}
			}

			// Stop performs the normal logout exchange and joins the real writer
			// and reader. Receipt alone is not used as proof of slot return.
			initiator.Stop()
			clientStopped = true
			waitApplicationBoundaryLifecycle(t, clientApplication.logouts, clientApplication.errors, clientID)
			waitApplicationBoundaryLifecycle(t, serverApplication.logouts, serverApplication.errors, serverID)
			acceptor.Stop()
			serverStopped = true
			for _, current := range []*session{acceptor.sessions[serverID], initiator.sessions[clientID]} {
				require.NotNil(t, current.boundedIn)
				require.NotNil(t, current.outbound)
				require.Equal(t, DefaultParseLimits(), current.boundedIn.limits)
				require.Len(t, current.boundedIn.ring.vacant, len(current.boundedIn.ring.slots))
				require.Len(t, current.outbound.vacant, len(current.outbound.slots))
				require.Nil(t, current.readConnection)
				require.Nil(t, current.writeConnection)
			}
			require.Empty(t, serverApplication.errors)
			require.Empty(t, clientApplication.errors)
			if !borrowed {
				assertApplicationBoundaryRetained(t, retained)
			}
		})
	}
}
