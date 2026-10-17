//go:build !race

package quickfix

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// Allocation measurement is separate from the race-instrumented correctness
// fixtures, which exercise these same TCP flows. ReadMemStats totals detect even
// one allocation across the interval; per-operation rounded reports cannot.
func TestBoundedFullSessionExactAllocations(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	for _, scenario := range []string{"ordinary", "groups", "outbound_resend", "inbound_gap", "slow_consumer", "warmed_post_reconnect", "heartbeat_test_request"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newBoundedFullSession(t, scenario != "ordinary")
			require.NoError(t, fixture.warm())
			switch scenario {
			case "slow_consumer":
				require.NoError(t, fixture.prepareSlowConsumer())
			case "warmed_post_reconnect":
				require.NoError(t, fixture.reconnect())
				require.NoError(t, fixture.warm())
			}
			cycle := fixture.scenario(scenario)
			require.NoError(t, cycle())

			var before, after runtime.MemStats
			var cycleError error
			completed := 0
			runtime.ReadMemStats(&before)
			for range 100 {
				if cycleError = cycle(); cycleError != nil {
					break
				}
				completed++
			}
			runtime.ReadMemStats(&after)

			allocations, allocatedBytes := after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc
			t.Logf("completed_cycles=%d total_mallocs_delta=%d total_alloc_bytes_delta=%d", completed, allocations, allocatedBytes)
			require.NoError(t, cycleError)
			require.Equal(t, 100, completed)
			require.Zero(t, allocations, "the full warmed interval must have no heap allocation")
			require.Zero(t, allocatedBytes, "the full warmed interval must allocate no heap bytes")
		})
	}
}
