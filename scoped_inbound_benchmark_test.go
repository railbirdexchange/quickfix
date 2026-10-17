package quickfix

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix/config"
	"github.com/quickfixgo/quickfix/internal"
)

// The same synchronous application exercises owned and explicitly borrowed
// callback delivery. It retains no message or input byte views.
type scopedInboundBenchmarkApplication struct {
	calls int
	value byte
}

func (*scopedInboundBenchmarkApplication) OnCreate(SessionID)              {}
func (*scopedInboundBenchmarkApplication) OnLogon(SessionID)               {}
func (*scopedInboundBenchmarkApplication) OnLogout(SessionID)              {}
func (*scopedInboundBenchmarkApplication) ToAdmin(*Message, SessionID)     {}
func (*scopedInboundBenchmarkApplication) ToApp(*Message, SessionID) error { return nil }
func (*scopedInboundBenchmarkApplication) FromAdmin(*Message, SessionID) MessageRejectError {
	return nil
}
func (application *scopedInboundBenchmarkApplication) FromApp(message *Message, _ SessionID) MessageRejectError {
	value, reject := message.Body.GetBytes(11)
	if reject != nil {
		return reject
	}
	if len(value) == 0 {
		return RequiredTagMissing(11)
	}
	application.calls++
	application.value = value[0]
	return nil
}

// Each frame advances the sequence and checksum without allocating. This is a
// deterministic byte source for the real framing reader, not a socket benchmark.
type scopedInboundBenchmarkConnection struct {
	wire           []byte
	sequenceOffset int
	checksumOffset int
	offset         int
	sequence       int
	remaining      int
}

func (connection *scopedInboundBenchmarkConnection) Read(destination []byte) (int, error) {
	if connection.remaining == 0 {
		return 0, io.EOF
	}
	if connection.offset == 0 {
		sequence := connection.sequence
		for index := connection.sequenceOffset + 9; index >= connection.sequenceOffset; index-- {
			connection.wire[index] = byte(sequence%10) + '0'
			sequence /= 10
		}
		checksum := 0
		for _, value := range connection.wire[:connection.checksumOffset-3] {
			checksum += int(value)
		}
		checksum %= 256
		connection.wire[connection.checksumOffset] = byte(checksum/100) + '0'
		connection.wire[connection.checksumOffset+1] = byte(checksum/10%10) + '0'
		connection.wire[connection.checksumOffset+2] = byte(checksum%10) + '0'
	}
	count := copy(destination, connection.wire[connection.offset:])
	connection.offset += count
	if connection.offset == len(connection.wire) {
		connection.offset = 0
		connection.sequence++
		connection.remaining--
	}
	return count, nil
}

func (*scopedInboundBenchmarkConnection) Write(value []byte) (int, error)  { return len(value), nil }
func (*scopedInboundBenchmarkConnection) Close() error                     { return nil }
func (*scopedInboundBenchmarkConnection) LocalAddr() net.Addr              { return nil }
func (*scopedInboundBenchmarkConnection) RemoteAddr() net.Addr             { return nil }
func (*scopedInboundBenchmarkConnection) SetDeadline(time.Time) error      { return nil }
func (*scopedInboundBenchmarkConnection) SetReadDeadline(time.Time) error  { return nil }
func (*scopedInboundBenchmarkConnection) SetWriteDeadline(time.Time) error { return nil }

func scopedInboundBenchmarkSetup(test testing.TB, groups bool, frames int) (*session, *scopedInboundBenchmarkApplication, <-chan fixIn, func()) {
	test.Helper()
	return scopedInboundBenchmarkSetupDelivery(test, groups, frames, true)
}

func scopedInboundBenchmarkSetupDelivery(test testing.TB, groups bool, frames int, borrowed bool) (*session, *scopedInboundBenchmarkApplication, <-chan fixIn, func()) {
	test.Helper()
	wire, dictionary := boundedOwnershipFixture(test, groups)
	message := NewMessage()
	if err := ParseMessageWithDataDictionary(message, bytes.NewBuffer(wire), dictionary, dictionary); err != nil {
		test.Fatal(err)
	}
	message.Header.SetString(tagMsgSeqNum, "0000000001")
	message.Header.SetString(tagSendingTime, time.Now().UTC().Format("20060102-15:04:05.000"))
	beginString, _ := message.Header.GetString(tagBeginString)
	sender, _ := message.Header.GetString(tagSenderCompID)
	target, _ := message.Header.GetString(tagTargetCompID)
	wire = message.build()
	connection := &scopedInboundBenchmarkConnection{wire: wire, sequence: 1, remaining: frames,
		sequenceOffset: bytes.Index(wire, []byte("\x0134=")) + 4, checksumOffset: len(wire) - 4}
	settings := NewSessionSettings()
	settings.Set(config.DataDictionary, "spec/FIX40.xml")
	if groups {
		settings.Set(config.DataDictionary, "spec/FIX44.xml")
	}
	settings.Set(config.InChanCapacity, "32")
	settings.Set(config.MaxLatency, "3600")
	application := &scopedInboundBenchmarkApplication{}
	var callback Application = application
	if borrowed {
		callback = BorrowApplication(application)
	}
	current, err := (sessionFactory{}).newSession(SessionID{BeginString: beginString, SenderCompID: target, TargetCompID: sender}, NewMemoryStoreFactory(), settings, NewNullLogFactory(), callback)
	if err != nil {
		test.Fatal(err)
	}
	current.State = inSession{}
	current.HeartBtInt = time.Hour
	current.peerTimer = internal.NewEventTimer(func() {})
	current.stateTimer = internal.NewEventTimer(func() {})
	current.messageOut = make(chan []byte, 1)
	messageIn := make(chan fixIn, current.InChanCapacity)
	current.messageIn = messageIn
	// Both callback delivery contracts use the production fixed-slot reader.
	control, err := startBoundedRead(connection, newParser(connection), current.boundedIn, messageIn, nil, time.Time{})
	if err != nil {
		test.Fatal(err)
	}
	current.readConnection = control
	cleanup := func() {
		for incoming := range messageIn {
			if incoming.slot != nil {
				incoming.slot.release()
			}
		}
		<-control.done
		current.readConnection = nil
		current.peerTimer.Stop()
		current.stateTimer.Stop()
		_ = current.store.Close()
	}
	return current, application, messageIn, cleanup
}

// BenchmarkScopedInboundSession measures the same ordinary fixed-storage engine
// with owned callbacks and BorrowApplication: framing -> inbound channel ->
// parse/dictionary -> validation -> callback -> sequence advance -> slot return.
// Owned delivery includes the compatibility copy at the application boundary.
// Every measured iteration waits for Incoming to complete. Socket writes and
// application-created output are outside this inbound phase's measurement.
func BenchmarkScopedInboundSession(benchmark *testing.B) {
	for _, groups := range []bool{false, true} {
		name := "new_order_single"
		if groups {
			name = "repeating_group"
		}
		for _, delivery := range []struct {
			name     string
			borrowed bool
		}{{name: "owned"}, {name: "borrowed", borrowed: true}} {
			benchmark.Run(name+"/"+delivery.name, func(benchmark *testing.B) {
				const warmFrames = 66
				current, application, incoming, cleanup := scopedInboundBenchmarkSetupDelivery(benchmark, groups, benchmark.N+warmFrames, delivery.borrowed)
				defer cleanup()
				process := func() {
					message, open := <-incoming
					if !open || message.readError != nil {
						benchmark.Fatalf("reader stopped: open=%v, error=%v", open, message.readError)
					}
					current.Incoming(current, message)
					if !current.IsLoggedOn() {
						benchmark.Fatal("ordinary session disconnected during processing")
					}
				}
				for range warmFrames {
					process()
				}
				benchmark.ReportAllocs()
				benchmark.ResetTimer()
				for iteration := 0; iteration < benchmark.N; iteration++ {
					process()
				}
				benchmark.StopTimer()
				if application.calls != benchmark.N+warmFrames || current.store.NextTargetMsgSeqNum() != benchmark.N+warmFrames+1 {
					benchmark.Fatalf("callbacks=%d, next target=%d", application.calls, current.store.NextTargetMsgSeqNum())
				}
			})
		}
	}
}

func TestScopedInboundSessionAllocations(t *testing.T) {
	for _, groups := range []bool{false, true} {
		name := "new_order_single"
		if groups {
			name = "repeating_group"
		}
		t.Run(name, func(t *testing.T) {
			const warmFrames, measuredFrames = 66, 1000
			// AllocsPerRun performs one additional warm-up call.
			const totalFrames = warmFrames + measuredFrames + 1
			// Only explicit borrowed delivery excludes the compatibility copy.
			current, application, incoming, cleanup := scopedInboundBenchmarkSetupDelivery(t, groups, totalFrames, true)
			cleaned := false
			defer func() {
				if !cleaned {
					cleanup()
				}
			}()
			process := func() {
				message, open := <-incoming
				if !open || message.readError != nil {
					t.Fatalf("reader stopped: open=%v, error=%v", open, message.readError)
				}
				current.Incoming(current, message)
				if !current.IsLoggedOn() {
					t.Fatal("ordinary session disconnected during processing")
				}
			}
			for range warmFrames {
				process()
			}
			allocations := testing.AllocsPerRun(measuredFrames, process)
			cleanup()
			cleaned = true
			if allocations != 0 {
				t.Fatalf("borrowed inbound processing allocated %v times per frame", allocations)
			}
			if application.calls != totalFrames || current.store.NextTargetMsgSeqNum() != totalFrames+1 {
				t.Fatalf("callbacks=%d, next target=%d", application.calls, current.store.NextTargetMsgSeqNum())
			}
			if returned, count := len(current.boundedIn.ring.vacant), len(current.boundedIn.ring.slots); returned != count {
				t.Fatalf("returned %d of %d raw slots after completed processing", returned, count)
			}
		})
	}
}
