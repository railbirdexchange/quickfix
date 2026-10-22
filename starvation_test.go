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

	"github.com/quickfixgo/quickfix/config"
	"github.com/stretchr/testify/require"
)

const starvationSentAtTag Tag = 5001

type starvationApp struct {
	toAppCost time.Duration
	loggedOn  chan SessionID
	mu        sync.Mutex
	latencies []time.Duration
	received  atomic.Uint64
	invalid   atomic.Uint64
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
		for {
			if time.Since(started) >= a.toAppCost {
				break
			}
		}
	}
	return nil
}

func (a *starvationApp) FromAdmin(*Message, SessionID) MessageRejectError { return nil }

func (a *starvationApp) FromApp(msg *Message, _ SessionID) MessageRejectError {
	sentAt, rejectErr := msg.Body.GetString(starvationSentAtTag)
	if rejectErr != nil {
		a.invalid.Add(1)
		return nil
	}
	nanos, parseErr := strconv.ParseInt(sentAt, 10, 64)
	if parseErr != nil {
		a.invalid.Add(1)
		return nil
	}
	a.mu.Lock()
	a.latencies = append(a.latencies, time.Since(time.Unix(0, nanos)))
	a.mu.Unlock()
	a.received.Add(1)
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
	runInboundStarvationFlood(t, false)
}

func TestBoundedInboundIsNotStarvedByOutboundFlood(t *testing.T) {
	runInboundStarvationFlood(t, true)
}

func runInboundStarvationFlood(t *testing.T, bounded bool) {
	t.Helper()
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
	acceptorSettings := starvationSettings(t, port, "VENUE", "DEALER", "acceptor")
	initiatorSettings := starvationSettings(t, port, "DEALER", "VENUE", "initiator")
	storeFactory := NewMemoryStoreFactory()
	if bounded {
		// Fixed retained storage covers the default four-second flood at one
		// millisecond per ToApp plus probes and session controls. Deliberately
		// faster overrides can exhaust it and must fail the no-drop assertions.
		const journalMessages, frameBytes = 16384, 1024
		storeFactory = NewBoundedStoreFactory(journalMessages, frameBytes)
		for _, settings := range []*Settings{acceptorSettings, initiatorSettings} {
			global := settings.GlobalSettings()
			global.Set(config.InChanCapacity, "32")
			global.Set(config.InboundFrameQueueCapacity, "32")
			global.Set(config.OutboundFrameQueueCapacity, "32")
			global.Set(config.SocketOutboundBufferSize, "32")
			global.Set(config.BoundedInboundStashCapacity, "32")
			global.Set(config.BoundedMaxFrameBytes, strconv.Itoa(frameBytes))
			global.Set(config.BoundedMaxFields, "64")
			global.Set(config.BoundedMaxGroupEntries, "8")
			global.Set(config.BoundedMaxGroupDepth, "4")
		}
		t.Logf("bounded frame_bytes=%d inbound_queue=32 outbound_queue=32 writer_queue=32 retained_messages=%d", frameBytes, journalMessages)
	}

	acceptor, err := NewAcceptor(acceptorApp, storeFactory, acceptorSettings, NewNullLogFactory())
	require.NoError(t, err)
	require.NoError(t, acceptor.Start())
	defer acceptor.Stop()

	initiator, err := NewInitiator(initiatorApp, storeFactory, initiatorSettings, NewNullLogFactory())
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
		outboundBefore := initiatorApp.received.Load()
		var stop atomic.Bool
		var outboundAccepted, outboundErrors atomic.Uint64
		firstOutboundError := make(chan error, 1)
		var flooding sync.WaitGroup
		defer func() { stop.Store(true); flooding.Wait() }()
		if flood {
			for range senders {
				flooding.Add(1)
				go func() {
					defer flooding.Done()
					for !stop.Load() {
						if err := SendToTarget(starvationMessage(time.Now()), acceptorSession); err != nil {
							outboundErrors.Add(1)
							select {
							case firstOutboundError <- err:
							default:
							}
						} else {
							outboundAccepted.Add(1)
						}
					}
				}()
			}
		}
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.Now().Add(3 * time.Second)
		inboundAccepted := 0
		for time.Now().Before(deadline) {
			<-ticker.C
			require.NoError(t, SendToTarget(starvationMessage(time.Now()), initiatorSession))
			inboundAccepted++
		}
		ticker.Stop()
		outboundDuringLoad := initiatorApp.received.Load() - outboundBefore
		time.Sleep(time.Second)
		stop.Store(true)
		flooding.Wait()
		time.Sleep(2 * time.Second)
		latencies := acceptorApp.takeLatencies()
		outboundReceived := initiatorApp.received.Load() - outboundBefore
		t.Logf("flood=%v inbound accepted=%d received=%d; outbound accepted=%d errors=%d received_during_probe_load=%d received_after_drain=%d",
			flood, inboundAccepted, len(latencies), outboundAccepted.Load(), outboundErrors.Load(), outboundDuringLoad, outboundReceived)
		select {
		case err := <-firstOutboundError:
			t.Errorf("outbound flood send failed: %v", err)
		default:
		}
		require.Zero(t, outboundErrors.Load(), "inbound responsiveness must not be bought by dropping outbound sends")
		require.Equal(t, inboundAccepted, len(latencies), "all accepted inbound probes must reach FromApp")
		require.Equal(t, outboundAccepted.Load(), outboundReceived, "all accepted flood messages must reach the peer")
		require.Zero(t, acceptorApp.invalid.Load())
		require.Zero(t, initiatorApp.invalid.Load())
		if flood {
			require.Positive(t, outboundDuringLoad, "outbound must make progress while inbound probes and flood senders are active")
		}
		return latencies
	}

	quiet := measure(false)
	flooded := measure(true)
	t.Logf("senders %d, ToApp cost %v", senders, toAppCost)
	t.Logf("inbound without flood: %d messages, p50 %v, p99 %v", len(quiet), percentile(quiet, 0.5), percentile(quiet, 0.99))
	t.Logf("inbound with flood:    %d messages, p50 %v, p99 %v", len(flooded), percentile(flooded, 0.5), percentile(flooded, 0.99))

	require.GreaterOrEqual(t, len(flooded), len(quiet)*9/10)
	require.Less(t, percentile(flooded, 0.99), 100*time.Millisecond)
}
