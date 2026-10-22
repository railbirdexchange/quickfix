package quickfix

import (
	"net"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const ordinaryLatencySampleCapacity = 65536

// The final fixed-size window supplies exact nearest-rank percentiles. Maximum
// and count cover every measured request, including samples that leave the
// window. Recording never allocates or grows the backing storage.
type ordinaryLatencySamples struct {
	values            []int64
	next, used, total int
	maximum           int64
}

func (samples *ordinaryLatencySamples) record(elapsed time.Duration) {
	value := int64(elapsed)
	samples.values[samples.next] = value
	samples.next++
	if samples.next == len(samples.values) {
		samples.next = 0
	}
	if samples.used < len(samples.values) {
		samples.used++
	}
	samples.total++
	if value > samples.maximum {
		samples.maximum = value
	}
}

// Call only after timing stops. Sorting destroys chronological window order;
// it does not change which samples are represented or allocate another buffer.
func (samples *ordinaryLatencySamples) percentiles() (p50, p95, p99 int64) {
	if samples.used == 0 {
		return 0, 0, 0
	}
	window := samples.values[:samples.used]
	slices.Sort(window)
	return window[(samples.used*50+99)/100-1], window[(samples.used*95+99)/100-1], window[(samples.used*99+99)/100-1]
}

type ordinaryRoundTripMeasurement struct {
	started, completed time.Time
	err                error
}

// One measured operation begins before client encoding and ends after the
// server's owner-side acknowledgement barrier. That barrier flushes pending
// writes, waits for every output slot to return, and retires only journal data
// explicitly acknowledged by the peer. Peer parsing, validation, the extra ACK
// frame and its processing are included. This is not engine-only latency.
//
// responseRead is nil in the benchmark. The correctness fixture uses it to
// demonstrate that receiving the response is earlier than write-slot return.
func measureOrdinaryFullSessionRoundTrip(fixture *boundedFullSession, responseRead chan<- struct{}) ordinaryRoundTripMeasurement {
	measurement := ordinaryRoundTripMeasurement{started: time.Now()}
	measurement.err = fixture.unacknowledgedBatch(1)
	if measurement.err == nil {
		if responseRead != nil {
			responseRead <- struct{}{}
		}
		_, measurement.err = fixture.acknowledge(false)
	}
	measurement.completed = time.Now()
	return measurement
}

// Each op is one application request/response plus the explicit completion ACK,
// not a batch. Percentiles describe the final min(N, 65536) requests; max covers
// all N. Default ns/op and requests/s include clock/sample-recording overhead.
// Setup, warm-up, percentile sorting and shutdown are outside timing.
func BenchmarkOrdinaryFullSessionRoundTrip(benchmark *testing.B) {
	for _, scenario := range []struct {
		name   string
		groups bool
	}{{name: "ordinary"}, {name: "groups", groups: true}} {
		benchmark.Run(scenario.name, func(benchmark *testing.B) {
			benchmark.StopTimer()
			fixture := newBoundedFullSession(benchmark, scenario.groups)
			require.NoError(benchmark, fixture.warm())
			samples := ordinaryLatencySamples{values: make([]int64, ordinaryLatencySampleCapacity)}
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			benchmark.StartTimer()
			for range benchmark.N {
				measurement := measureOrdinaryFullSessionRoundTrip(fixture, nil)
				if measurement.err != nil {
					benchmark.Fatal(measurement.err)
				}
				samples.record(measurement.completed.Sub(measurement.started))
			}
			benchmark.StopTimer()
			p50, p95, p99 := samples.percentiles()
			benchmark.ReportMetric(float64(p50), "p50-ns")
			benchmark.ReportMetric(float64(p95), "p95-ns")
			benchmark.ReportMetric(float64(p99), "p99-ns")
			benchmark.ReportMetric(float64(samples.maximum), "max-ns")
			benchmark.ReportMetric(float64(samples.used), "latency-samples")
			benchmark.ReportMetric(float64(samples.total)/benchmark.Elapsed().Seconds(), "requests/s")
		})
	}
}

func TestOrdinaryLatencySamplesKeepBoundedWindowAndGlobalMaximum(t *testing.T) {
	samples := ordinaryLatencySamples{values: make([]int64, 4)}
	storage := &samples.values[0]
	for _, value := range []time.Duration{100, 4, 3, 2, 1, 5} {
		samples.record(value)
	}
	p50, p95, p99 := samples.percentiles()
	require.Equal(t, int64(2), p50)
	require.Equal(t, int64(5), p95)
	require.Equal(t, int64(5), p99)
	require.Equal(t, int64(100), samples.maximum, "an old outlier must survive window rollover in the global maximum")
	require.Equal(t, 6, samples.total)
	require.Equal(t, 4, samples.used)
	require.Equal(t, 4, cap(samples.values))
	require.Same(t, storage, &samples.values[0])
}

// The underlying TCP Write completes first, allowing the peer to receive and
// validate all response bytes. The wrapper then retains the writer's output
// slot until release. This proves the timer's end is later than peer receipt.
type ordinaryLatencyCompletionGate struct {
	net.Conn
	armed, written, release chan struct{}
}

func (gate *ordinaryLatencyCompletionGate) Write(wire []byte) (int, error) {
	written, err := gate.Conn.Write(wire)
	if err == nil && written == len(wire) {
		select {
		case <-gate.armed:
			gate.written <- struct{}{}
			<-gate.release
		default:
		}
	}
	return written, err
}

func TestOrdinaryFullSessionLatencyIncludesWriteSlotReturn(t *testing.T) {
	fixture := newBoundedFullSession(t, true)
	require.NoError(t, fixture.warm())
	gate := &ordinaryLatencyCompletionGate{
		Conn: fixture.server, armed: make(chan struct{}, 1),
		written: make(chan struct{}, 1), release: make(chan struct{}, 1),
	}
	// Cleanup must unblock the writer even when a preceding assertion fails.
	t.Cleanup(func() {
		select {
		case gate.release <- struct{}{}:
		default:
		}
	})
	require.NoError(t, fixture.configure(func(application *boundedFullApplication) {
		application.session.writeConnection.connection = gate
	}))
	gate.armed <- struct{}{}
	responseRead := make(chan struct{}, 1)
	finished := make(chan ordinaryRoundTripMeasurement, 1)
	go func() { finished <- measureOrdinaryFullSessionRoundTrip(fixture, responseRead) }()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	select {
	case <-gate.written:
	case measurement := <-finished:
		t.Fatalf("measurement returned before gated write: %v", measurement.err)
	case <-deadline.C:
		t.Fatal("TCP writer did not reach completion gate")
	}
	select {
	case <-responseRead:
	case measurement := <-finished:
		t.Fatalf("measurement returned before peer validation: %v", measurement.err)
	case <-deadline.C:
		t.Fatal("peer did not validate the delivered response")
	}
	select {
	case measurement := <-finished:
		t.Fatalf("measurement completed while the writer still owned its slot: %v", measurement.err)
	default:
	}
	releasedAt := time.Now()
	gate.release <- struct{}{}
	var measurement ordinaryRoundTripMeasurement
	select {
	case measurement = <-finished:
	case <-deadline.C:
		t.Fatal("measurement did not finish after output-slot release")
	}
	require.NoError(t, measurement.err)
	require.False(t, measurement.started.After(releasedAt))
	require.False(t, measurement.completed.Before(releasedAt), "latency must include the period after peer receipt while the writer retains its slot")
	require.Equal(t, 33, fixture.lastCheckpoint.freeOutputs)
	require.Zero(t, fixture.lastCheckpoint.retained)
}
