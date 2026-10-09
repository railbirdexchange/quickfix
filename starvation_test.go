package quickfix

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const starvationSentAtTag Tag = 5001

type starvationApp struct {
	toAppCost time.Duration
	loggedOn  chan SessionID
	mu        sync.Mutex
	latencies []time.Duration
}

func (a *starvationApp) OnCreate(SessionID) {}

func (a *starvationApp) OnLogon(sessionID SessionID) {
	select {
	case a.loggedOn <- sessionID:
	default:
	}
}

func (a *starvationApp) OnLogout(SessionID) {}

func (a *starvationApp) ToAdmin(*Message, SessionID) {}

func (a *starvationApp) ToApp(*Message, SessionID) error {
	if a.toAppCost > 0 {
		started := time.Now()
		for time.Since(started) < a.toAppCost {
		}
	}
	return nil
}

func (a *starvationApp) FromAdmin(*Message, SessionID) MessageRejectError { return nil }

func (a *starvationApp) FromApp(msg *Message, _ SessionID) MessageRejectError {
	sentAt, rejectErr := msg.Body.GetString(starvationSentAtTag)
	if rejectErr != nil {
		return nil
	}
	nanos, parseErr := strconv.ParseInt(sentAt, 10, 64)
	if parseErr != nil {
		return nil
	}
	a.mu.Lock()
	a.latencies = append(a.latencies, time.Since(time.Unix(0, nanos)))
	a.mu.Unlock()
	return nil
}

func (a *starvationApp) takeLatencies() []time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	taken := a.latencies
	a.latencies = nil
	return taken
}

func starvationSettings(t *testing.T, port int, sender, target, role string) *Settings {
	text := strings.Join([]string{
		"[DEFAULT]",
		"BeginString=FIX.4.4",
		"HeartBtInt=30",
		"ResetOnLogon=Y",
		"UseDataDictionary=N",
		"ValidateFieldsOutOfOrder=N",
		"ReconnectInterval=1",
		"StartTime=00:00:00",
		"EndTime=00:00:00",
		fmt.Sprintf("SocketAcceptPort=%d", port),
		"SocketConnectHost=127.0.0.1",
		fmt.Sprintf("SocketConnectPort=%d", port),
		"[SESSION]",
		"SenderCompID=" + sender,
		"TargetCompID=" + target,
		"",
	}, "\n")
	settings, err := ParseSettings(strings.NewReader(text))
	require.NoError(t, err, role)
	return settings
}

func freeTCPPort(t *testing.T) int {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func starvationMessage(sentAt time.Time) *Message {
	msg := NewMessage()
	msg.Header.SetField(tagMsgType, FIXString("B"))
	msg.Body.SetField(Tag(148), FIXString("starvation"))
	msg.Body.SetField(starvationSentAtTag, FIXString(strconv.FormatInt(sentAt.UnixNano(), 10)))
	return msg
}

func percentile(values []time.Duration, fraction float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(fraction * float64(len(sorted)-1))
	return sorted[index]
}

func TestInboundIsNotStarvedByOutboundFlood(t *testing.T) {
	if os.Getenv("QUICKFIX_STARVATION_TEST") != "1" {
		t.Skip("set QUICKFIX_STARVATION_TEST=1 to run the inbound starvation reproduction")
	}

	previousProcs := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(previousProcs)

	port := freeTCPPort(t)
	toAppCost := time.Millisecond
	if value, err := strconv.Atoi(os.Getenv("QUICKFIX_STARVATION_TOAPP_US")); err == nil {
		toAppCost = time.Duration(value) * time.Microsecond
	}
	senders := 4
	if value, err := strconv.Atoi(os.Getenv("QUICKFIX_STARVATION_SENDERS")); err == nil && value > 0 {
		senders = value
	}
	acceptorApp := &starvationApp{toAppCost: toAppCost, loggedOn: make(chan SessionID, 1)}
	initiatorApp := &starvationApp{loggedOn: make(chan SessionID, 1)}

	acceptor, err := NewAcceptor(acceptorApp, NewMemoryStoreFactory(), starvationSettings(t, port, "VENUE", "DEALER", "acceptor"), NewNullLogFactory())
	require.NoError(t, err)
	require.NoError(t, acceptor.Start())
	defer acceptor.Stop()

	initiator, err := NewInitiator(initiatorApp, NewMemoryStoreFactory(), starvationSettings(t, port, "DEALER", "VENUE", "initiator"), NewNullLogFactory())
	require.NoError(t, err)
	require.NoError(t, initiator.Start())
	defer initiator.Stop()

	var acceptorSession, initiatorSession SessionID
	select {
	case acceptorSession = <-acceptorApp.loggedOn:
	case <-time.After(10 * time.Second):
		t.Fatal("acceptor did not log on")
	}
	select {
	case initiatorSession = <-initiatorApp.loggedOn:
	case <-time.After(10 * time.Second):
		t.Fatal("initiator did not log on")
	}

	measure := func(flood bool) []time.Duration {
		acceptorApp.takeLatencies()
		var stop atomic.Bool
		var flooding sync.WaitGroup
		if flood {
			for range senders {
				flooding.Add(1)
				go func() {
					defer flooding.Done()
					for !stop.Load() {
						_ = SendToTarget(starvationMessage(time.Now()), acceptorSession)
					}
				}()
			}
		}
		ticker := time.NewTicker(2 * time.Millisecond)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			<-ticker.C
			require.NoError(t, SendToTarget(starvationMessage(time.Now()), initiatorSession))
		}
		ticker.Stop()
		time.Sleep(time.Second)
		stop.Store(true)
		flooding.Wait()
		time.Sleep(2 * time.Second)
		return acceptorApp.takeLatencies()
	}

	quiet := measure(false)
	flooded := measure(true)
	t.Logf("senders %d, ToApp cost %v", senders, toAppCost)
	t.Logf("inbound without flood: %d messages, p50 %v, p99 %v", len(quiet), percentile(quiet, 0.5), percentile(quiet, 0.99))
	t.Logf("inbound with flood:    %d messages, p50 %v, p99 %v", len(flooded), percentile(flooded, 0.5), percentile(flooded, 0.99))

	require.GreaterOrEqual(t, len(flooded), len(quiet)*9/10)
	require.Less(t, percentile(flooded, 0.99), 100*time.Millisecond)
}
