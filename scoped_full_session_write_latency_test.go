package quickfix

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type ordinaryReadToWriteStart struct {
	received   time.Time
	identifier int
}

type ordinaryReadToWriteCompletion struct {
	ordinaryReadToWriteStart
	completed time.Time
	err       error
}

// Only the TCP writer owns the mutable frame state. The ordinary callback
// transfers a copied receive time and request identity; the driver receives a
// copied completion. Channels are allocated once and hold one outstanding
// request. An intervening admin frame cannot consume that request's sample.
type ordinaryReadToWriteConnection struct {
	net.Conn
	starts      chan ordinaryReadToWriteStart
	completions chan ordinaryReadToWriteCompletion
	request     ordinaryReadToWriteStart
	hasRequest  bool
	remaining   int
	measuring   bool
}

func newOrdinaryReadToWriteConnection(connection net.Conn) *ordinaryReadToWriteConnection {
	return &ordinaryReadToWriteConnection{
		Conn: connection, starts: make(chan ordinaryReadToWriteStart, 1),
		completions: make(chan ordinaryReadToWriteCompletion, 1),
	}
}

func ordinaryLatencyWireValue(wire []byte, tag int) []byte {
	for len(wire) != 0 {
		end := bytes.IndexByte(wire, 1)
		if end < 0 {
			return nil
		}
		field := wire[:end]
		wire = wire[end+1:]
		equals := bytes.IndexByte(field, '=')
		if equals < 1 {
			return nil
		}
		fieldTag := 0
		for _, digit := range field[:equals] {
			if digit < '0' || digit > '9' {
				return nil
			}
			fieldTag = fieldTag*10 + int(digit-'0')
		}
		if fieldTag == tag {
			return field[equals+1:]
		}
	}
	return nil
}

func ordinaryLatencyIdentifierEqual(wire []byte, identifier int) bool {
	if len(wire) == 0 || identifier < 0 {
		return false
	}
	for index := len(wire) - 1; index >= 0; index-- {
		if wire[index] != '0'+byte(identifier%10) {
			return false
		}
		identifier /= 10
	}
	return identifier == 0
}

func (connection *ordinaryReadToWriteConnection) Write(wire []byte) (int, error) {
	if connection.remaining == 0 {
		if !connection.hasRequest {
			select {
			case connection.request = <-connection.starts:
				connection.hasRequest = true
			default:
			}
		}
		connection.remaining = len(wire)
		kind := ordinaryLatencyWireValue(wire, 35)
		connection.measuring = connection.hasRequest && len(kind) == 1 && kind[0] == '8' &&
			ordinaryLatencyIdentifierEqual(ordinaryLatencyWireValue(wire, 11), connection.request.identifier)
	}

	written, err := connection.Conn.Write(wire)
	completed := time.Now() // Immediately after the underlying Write returns.
	measurementError := err
	if written < 0 || written > len(wire) {
		measurementError = io.ErrShortWrite
	} else if written == 0 && err == nil {
		measurementError = io.ErrNoProgress
	} else {
		connection.remaining -= written
	}
	if measurementError != nil || connection.remaining == 0 {
		if connection.measuring {
			connection.completions <- ordinaryReadToWriteCompletion{
				ordinaryReadToWriteStart: connection.request, completed: completed, err: measurementError,
			}
			connection.hasRequest = false
		}
		connection.remaining = 0
		connection.measuring = false
	}
	// Preserve the underlying result; the production writer retains its normal
	// partial-write retry and failure behavior.
	return written, err
}

func installOrdinaryReadToWriteMeasurement(fixture *boundedFullSession) (*ordinaryReadToWriteConnection, error) {
	connection := newOrdinaryReadToWriteConnection(fixture.server)
	err := fixture.configure(func(application *boundedFullApplication) {
		application.onOrder = func(message *Message) MessageRejectError {
			identifier, reject := message.Body.GetInt(11)
			if reject != nil {
				return reject
			}
			connection.starts <- ordinaryReadToWriteStart{received: message.ReceiveTime, identifier: identifier}
			return application.handleOrder(message)
		}
		application.session.writeConnection.connection = connection
	})
	return connection, err
}

// ReceiveTime is parser.lastRead: time.Now immediately after the final socket
// Read returns, NOT a timestamp taken after ReadMessageInto finishes. With one
// outstanding request and the previous ACK barrier complete, this includes
// remaining framing/check/copy work after that Read, inbound queueing, parsing,
// dictionary/group validation, the ordinary handler, encoding, persistence and
// writer queue waits through the final successful TCP Write. Network arrival,
// Read blocking and framing work before the final Read are excluded.
//
// The direct interval ends before peer validation and the extra ACK. A sample
// is nevertheless accepted only after the unchanged RTT helper has validated
// the response and completed ACK processing, all slot returns and journal
// retirement. Receive-time copying, identity inspection and channel transfer
// instrument the measured path; no overhead is subtracted.
func measureOrdinaryReadToWrite(fixture *boundedFullSession, connection *ordinaryReadToWriteConnection, responseRead chan<- struct{}) (ordinaryRoundTripMeasurement, ordinaryReadToWriteCompletion) {
	roundTrip := measureOrdinaryFullSessionRoundTrip(fixture, responseRead)
	var completion ordinaryReadToWriteCompletion
	if roundTrip.err != nil {
		return roundTrip, completion
	}
	select {
	case completion = <-connection.completions:
	default:
		roundTrip.err = errBoundedFullFixture
		return roundTrip, completion
	}
	if completion.err != nil {
		roundTrip.err = completion.err
	} else if completion.received.IsZero() || completion.received.Before(roundTrip.started) ||
		completion.completed.Before(completion.received) || completion.completed.After(roundTrip.completed) {
		roundTrip.err = errBoundedFullFixture
	}
	return roundTrip, completion
}

// Keep BenchmarkOrdinaryFullSessionRoundTrip unchanged for historical
// comparison. This additional fixture reports both directly observed intervals
// from the same requests, with all instrumentation enabled in both scenarios.
// Each fixed final-window percentile covers min(N, 65536) requests; maxima cover
// all N. ns/op and requests/s also include sample recording and the ACK barrier.
func BenchmarkOrdinaryFullSessionReadToWrite(benchmark *testing.B) {
	for _, scenario := range []struct {
		name   string
		groups bool
	}{{name: "ordinary"}, {name: "groups", groups: true}} {
		benchmark.Run(scenario.name, func(benchmark *testing.B) {
			benchmark.StopTimer()
			fixture := newBoundedFullSession(benchmark, scenario.groups)
			require.NoError(benchmark, fixture.warm())
			connection, err := installOrdinaryReadToWriteMeasurement(fixture)
			require.NoError(benchmark, err)
			warm, _ := measureOrdinaryReadToWrite(fixture, connection, nil)
			require.NoError(benchmark, warm.err)
			direct := ordinaryLatencySamples{values: make([]int64, ordinaryLatencySampleCapacity)}
			roundTrips := ordinaryLatencySamples{values: make([]int64, ordinaryLatencySampleCapacity)}
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			benchmark.StartTimer()
			for range benchmark.N {
				roundTrip, completion := measureOrdinaryReadToWrite(fixture, connection, nil)
				if roundTrip.err != nil {
					benchmark.Fatal(roundTrip.err)
				}
				direct.record(completion.completed.Sub(completion.received))
				roundTrips.record(roundTrip.completed.Sub(roundTrip.started))
			}
			benchmark.StopTimer()
			p50, p95, p99 := direct.percentiles()
			benchmark.ReportMetric(float64(p50), "read-write-p50-ns")
			benchmark.ReportMetric(float64(p95), "read-write-p95-ns")
			benchmark.ReportMetric(float64(p99), "read-write-p99-ns")
			benchmark.ReportMetric(float64(direct.maximum), "read-write-max-ns")
			p50, p95, p99 = roundTrips.percentiles()
			benchmark.ReportMetric(float64(p50), "rtt-p50-ns")
			benchmark.ReportMetric(float64(p95), "rtt-p95-ns")
			benchmark.ReportMetric(float64(p99), "rtt-p99-ns")
			benchmark.ReportMetric(float64(roundTrips.maximum), "rtt-max-ns")
			benchmark.ReportMetric(float64(direct.used), "latency-samples")
			benchmark.ReportMetric(float64(direct.total)/benchmark.Elapsed().Seconds(), "requests/s")
		})
	}
}

// This unit fixture deliberately returns partial writes; the complete TCP
// qualification below still exercises the production writer and real sockets.
type ordinaryLatencyPartialConnection struct {
	net.Conn
	limit int
	err   error
}

func (connection *ordinaryLatencyPartialConnection) Write(wire []byte) (int, error) {
	return min(connection.limit, len(wire)), connection.err
}

func TestOrdinaryReadToWritePartialFramesExcludeAdminAndOtherRequests(t *testing.T) {
	connection := newOrdinaryReadToWriteConnection(&ordinaryLatencyPartialConnection{limit: 7})
	start := ordinaryReadToWriteStart{received: time.Now(), identifier: 42}
	connection.starts <- start
	for _, wire := range [][]byte{
		[]byte("8=FIX.4.4\x0135=0\x0111=42\x0110=000\x01"),
		[]byte("8=FIX.4.4\x0135=8\x0111=43\x0110=000\x01"),
	} {
		for len(wire) != 0 {
			written, err := connection.Write(wire)
			require.NoError(t, err)
			wire = wire[written:]
			require.Empty(t, connection.completions, "admin and unrelated responses cannot consume the armed sample")
		}
	}
	wire := []byte("8=FIX.4.4\x0135=8\x0111=42\x0110=000\x01")
	for len(wire) != 0 {
		written, err := connection.Write(wire)
		require.NoError(t, err)
		wire = wire[written:]
		if len(wire) != 0 {
			require.Empty(t, connection.completions, "a partial response is not a completed write")
		}
	}
	completion := <-connection.completions
	require.NoError(t, completion.err)
	require.Equal(t, start, completion.ordinaryReadToWriteStart)
	require.False(t, completion.completed.Before(start.received))
	require.Empty(t, connection.completions, "exactly one completion belongs to the request")
}

func TestOrdinaryReadToWriteRejectsFailedWrites(t *testing.T) {
	for _, test := range []struct {
		name    string
		limit   int
		err     error
		wantErr error
	}{
		{name: "no_progress", wantErr: io.ErrNoProgress},
		{name: "partial_with_error", limit: 7, err: io.ErrClosedPipe, wantErr: io.ErrClosedPipe},
		{name: "all_bytes_with_error", limit: 1024, err: io.ErrClosedPipe, wantErr: io.ErrClosedPipe},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection := newOrdinaryReadToWriteConnection(&ordinaryLatencyPartialConnection{limit: test.limit, err: test.err})
			connection.starts <- ordinaryReadToWriteStart{received: time.Now(), identifier: 42}
			wire := []byte("8=FIX.4.4\x0135=8\x0111=42\x0110=000\x01")
			written, err := connection.Write(wire)
			require.Equal(t, min(test.limit, len(wire)), written)
			require.Equal(t, test.err, err, "the wrapper must preserve the result seen by the production writer")
			completion := <-connection.completions
			require.ErrorIs(t, completion.err, test.wantErr, "a failed write cannot qualify a successful latency sample")
		})
	}
}

type ordinaryLatencyBeforeWriteGate struct {
	net.Conn
	armed, entered, release chan struct{}
}

func (gate *ordinaryLatencyBeforeWriteGate) Write(wire []byte) (int, error) {
	select {
	case <-gate.armed:
		gate.entered <- struct{}{}
		<-gate.release
	default:
	}
	return gate.Conn.Write(wire)
}

func TestOrdinaryReadToWriteIncludesWriterWaitAndQualifiesSlotReturn(t *testing.T) {
	for _, beforeWrite := range []bool{true, false} {
		name := "after_write_before_slot_return"
		if beforeWrite {
			name = "before_write"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newBoundedFullSession(t, true)
			require.NoError(t, fixture.warm())
			connection, err := installOrdinaryReadToWriteMeasurement(fixture)
			require.NoError(t, err)
			armed, entered, release := make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{}, 1)
			t.Cleanup(func() {
				select {
				case release <- struct{}{}:
				default:
				}
			})
			require.NoError(t, fixture.configure(func(application *boundedFullApplication) {
				if beforeWrite {
					connection.Conn = &ordinaryLatencyBeforeWriteGate{Conn: fixture.server, armed: armed, entered: entered, release: release}
				} else {
					application.session.writeConnection.connection = &ordinaryLatencyCompletionGate{Conn: connection, armed: armed, written: entered, release: release}
				}
			}))
			armed <- struct{}{}
			responseRead := make(chan struct{}, 1)
			type result struct {
				roundTrip  ordinaryRoundTripMeasurement
				completion ordinaryReadToWriteCompletion
			}
			finished := make(chan result, 1)
			go func() {
				roundTrip, completion := measureOrdinaryReadToWrite(fixture, connection, responseRead)
				finished <- result{roundTrip: roundTrip, completion: completion}
			}()
			deadline := time.NewTimer(10 * time.Second)
			defer deadline.Stop()
			select {
			case <-entered:
			case measurement := <-finished:
				t.Fatalf("measurement finished before the write gate: %v", measurement.roundTrip.err)
			case <-deadline.C:
				t.Fatal("writer did not enter the gate")
			}
			if beforeWrite {
				require.Empty(t, connection.completions)
			} else {
				select {
				case <-responseRead:
				case <-deadline.C:
					t.Fatal("peer did not validate the completed TCP response")
				}
			}
			select {
			case measurement := <-finished:
				t.Fatalf("sample was accepted before output-slot return: %v", measurement.roundTrip.err)
			default:
			}
			releasedAt := time.Now()
			release <- struct{}{}
			var measurement result
			select {
			case measurement = <-finished:
			case <-deadline.C:
				t.Fatal("measurement did not finish after gate release")
			}
			require.NoError(t, measurement.roundTrip.err)
			require.False(t, measurement.completion.received.After(releasedAt))
			if beforeWrite {
				require.False(t, measurement.completion.completed.Before(releasedAt), "direct latency must include the blocked writer")
			} else {
				require.False(t, measurement.completion.completed.After(releasedAt), "direct latency ends at actual Write completion, before later slot-return gating")
			}
			require.False(t, measurement.roundTrip.completed.Before(releasedAt), "RTT and sample qualification still include slot return and ACK")
			require.Equal(t, 33, fixture.lastCheckpoint.freeOutputs)
			require.Zero(t, fixture.lastCheckpoint.retained)
		})
	}
}
