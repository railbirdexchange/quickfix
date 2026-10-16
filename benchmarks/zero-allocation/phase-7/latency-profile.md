# Remaining latency: diagnostic profiles

Source: `0572f01bace8c2edce06c8fb6fa225f96de4f00f`, Go 1.25.3,
darwin/arm64, Apple M4 Pro, 2026-10-16. These profiles ran after the unprofiled
five-sample captures, without concurrent test/build workloads. Their benchmark
timing is diagnostic; it does not replace the controlled before/after evidence.

The [individual TCP completion profile](scoped-full-session-cpu-top.txt)
attributes 70.70% of samples to `syscall.syscall` and 25.49% to `runtime.kevent`.
This fixture includes both peers and an extra acknowledgement/slot-return
barrier. Sampling on macOS does not provide an exact decomposition of request
wall time, so these percentages must not be subtracted from the measured RTT to
invent an engine-only latency. They identify transport/runtime interaction as a
necessary part of the next latency investigation.

The separate [completed-write outbound component profile](scoped-outbound-cpu-top.txt)
uses an in-process copying connection and includes peer parsing and explicit
journal retirement. It reports 1,999 ns/op in its profiled run, not a TCP or
complete inbound-to-outbound latency. Across 12.72 seconds of sampled time,
`prepareOutbound` accounts for 6.51 seconds cumulatively. Within that call path:

- Replay-capacity parsing (`doParsing`) accounts for 1.93 seconds cumulatively.
- `appendTo` accounts for 1.29 seconds cumulatively.
- `messageArena.setScalar` accounts for 0.79 seconds cumulatively.
- Fixed-field lookup accounts for 0.51 seconds flat; `lengthWithin` accounts for
  0.25 seconds cumulatively.

These costs overlap through their call trees and must not be added together.
The [focused report](scoped-outbound-prepare-cpu-top.txt) retains percentages
relative to the entire profile. The [arena source report](scoped-outbound-cpu-source.txt)
locates sampled work. Source review identifies repeated scalar lookup, repeated
length scans despite cached lengths, and per-field compaction when clearing an
entire control. These are candidates for measured changes, not established
speedups. Replay admission must continue to enforce field/group/depth limits
before persistence; parsing cannot simply be removed to improve timing.

Both profiles include setup/warm-up because CPU profiling follows process scope,
not benchmark timer scope. Their raw [component run](scoped-outbound-cpu-run.txt)
and [TCP run](scoped-full-session-cpu-run.txt) are preserved. The profile binaries
were removed after producing text reports; the small `.pprof` artifacts remain.

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkScopedOutboundSession$/^new_order_single$' -benchmem -benchtime=10s -count=1 -cpu=1 -cpuprofile=benchmarks/zero-allocation/phase-7/scoped-outbound-cpu-after.pprof -o benchmarks/zero-allocation/phase-7/scoped-profile.test
rtk proxy go tool pprof -top -nodecount=45 benchmarks/zero-allocation/phase-7/scoped-profile.test benchmarks/zero-allocation/phase-7/scoped-outbound-cpu-after.pprof
rtk proxy go tool pprof -top -nodecount=35 -focus prepareOutbound benchmarks/zero-allocation/phase-7/scoped-profile.test benchmarks/zero-allocation/phase-7/scoped-outbound-cpu-after.pprof
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkOrdinaryFullSessionRoundTrip$/^ordinary$' -benchmem -benchtime=10s -count=1 -cpu=1 -cpuprofile=benchmarks/zero-allocation/phase-7/scoped-full-session-cpu-after.pprof -o benchmarks/zero-allocation/phase-7/scoped-profile.test
rtk proxy go tool pprof -top -nodecount=40 benchmarks/zero-allocation/phase-7/scoped-profile.test benchmarks/zero-allocation/phase-7/scoped-full-session-cpu-after.pprof
```

The one-microsecond goal remains unmet. No transport change, validation removal,
pool or dependency was introduced in response to these profiles. The user-supplied
[design references](design-references.md) informed the lifetime and profiling
review; their benchmark numbers are not QuickFIX evidence.

## Corrected runtime and next measured changes

At `a2fb11b9`, the new [direct read-to-write fixture](read-to-write/README.md)
measures ordinary p50 8.875–8.959 microseconds and p99 13.166–13.875 microseconds
on Go 1.23. It starts at the final socket Read return and ends at the final
successful Write return; acceptance still requires peer validation and slot
return. This removes the extra ACK from the direct latency interval without
removing the completion proof. All ten candidate rows report zero allocations,
but the matched comparison does not demonstrate a latency improvement.

Source review of the corrected runtime identifies two bounded follow-up
experiments; neither is implemented or claimed as a speedup here. First,
`messageArena.scalarFits` finds the record and calculates lengths that
`setScalar` immediately calculates again. Byte setters also preflight before
alias staging and then repeat preflight. Carrying one locally checked record
index and lengths into mutation could remove repeated lookup without persistent
caching. All capacity checks must remain before staging/mutation; overlap,
group replacement, exact-fit/one-over, sticky errors and section replacement
fixtures must remain intact.

Second, outbound replay admission currently resets its private parse slot
before and after parsing; previous admission and resend completion already
reset it, and Parse restores its representation. Removing only the preceding
reset is a candidate after auditing every exit and alternating malformed,
oversized, decreasing-field and nested-group cases. Full replay-capacity parsing
and the following reset must remain before commit. Both changes require new
controlled before/after component and complete-path measurements. Skipping
validation or assuming a globally valid encoded-length cache is not justified.
