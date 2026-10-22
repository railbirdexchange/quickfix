# Heartbeat string allocation reduction

The PREPROD heartbeat profile identified repeated `FieldMap.GetString` wrapper
allocations and the enabled send-stage observer's MsgType byte clone. This change
removes those two costs without changing public signatures, string ownership,
observer delivery, validation, persistence or logging.

`GetString` now converts the field bytes directly to an owned string while holding
the same read lock. Missing-field rejects and nil, empty and arbitrary-byte values
are unchanged. Prepared outbound stages take one owned string snapshot before
header construction and callbacks, instead of cloning bytes and converting that
clone again at each stage. A separate boolean preserves the original distinction
between a nil MsgType and a non-nil empty MsgType. Stage start times and the current
observer at delivery still determine whether an event is emitted.

## Ownership

```text
message owner / existing FieldMap read lock
  -> field bytes -> owned GetString result -> caller may retain result

session reserves output slot
  -> reads original MsgType -> owned observer string
  -> header construction / application callback may move or replace field bytes
  -> prepared-stage events retain the original string
  -> encode / persist -> writer channel transfers output slot ownership
  -> completed write -> output slot returned

outbound-channel event reads the serialized wire's MsgType as before
```

No mutable byte view escapes through either string result. No new lock, pool,
reference count, dependency, queue, capacity setting or callback lifetime is
introduced. Longer strings still allocate their owned content; this change does
not make arbitrary string getters or the entire deployed service allocation-free.

## Controlled measurements

Baseline: `262acbacf4c011c413deb5109f99390b304c0192`, plus the same new benchmark
and parity tests, with production code unchanged. Both captures use Go 1.27.2,
darwin/arm64, Apple M4 Pro, `-cpu=1`, five one-second samples. Setup and 66 completed
heartbeat warmups are untimed. Builds, tests and benchmarks run serially.

The unchanged benchmark source SHA-256 is
`86aa5d33a3338069077331a7525112a488e942accafc19572870e3acbe5c0b50`.
Raw evidence: [before](strings-before.txt), [after](strings-after.txt).
Times below are medians; B/op and allocs/op agree across all five samples.

| Flow | Before ns/op | After ns/op | Before B/op / allocs/op | After B/op / allocs/op |
|---|---:|---:|---:|---:|
| Legacy GetString, MsgType `0` | 19.73 | 8.819 | 16 / 1 | 0 / 0 |
| Fixed GetString, MsgType `0` | 19.49 | 8.192 | 16 / 1 | 0 / 0 |
| Legacy GetString, `RAILBIRD` | 23.95 | 15.61 | 24 / 2 | 8 / 1 |
| Fixed GetString, `RAILBIRD` | 23.63 | 15.61 | 24 / 2 | 8 / 1 |
| Legacy GetString, `PREPRODFCM01` | 25.70 | 17.25 | 32 / 2 | 16 / 1 |
| Fixed GetString, `PREPRODFCM01` | 25.08 | 17.37 | 32 / 2 | 16 / 1 |
| Completed heartbeat, observer enabled | 1,907 | 1,887 | 8 / 1 | 0 / 0 |
| Same heartbeat plus three callback MsgType reads | 1,951 | 1,913 | 56 / 4 | 0 / 0 |

The heartbeat component uses the existing bounded external-store stand-in, null
logger and copying connection fixture. Each operation waits for writer completion,
output-slot return and independent peer parsing. Observation is enabled and every
event is consumed and checked. It does not contain the production StatsD
transport, Kafka network or FIX-server audit/tracing callbacks. Its roughly 1.9 us
result is component execution time, not PREPROD socket latency. The small aggregate
timing difference does not establish a live latency improvement.

The mandatory unchanged `BenchmarkAllocationBaseline` captures are retained in
[before](baseline-before.txt) and [after](baseline-after.txt). They exercise the
compatibility APIs as well as reused parsing/validation; those APIs are not all
covered by the bounded-session allocation contract. All allocation counts are
unchanged in that baseline: reused plain/dictionary/group parsing and validation
remain 0 B/op and 0 allocs/op; the allocating build wrapper remains 152 B/op and
6 allocs/op, and the legacy framed reader remains 160 B/op and 2 allocs/op.

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^Benchmark(HeartbeatStringRead|HeartbeatObservedStringReads)$' -benchmem -benchtime=1s -count=5 -cpu=1
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkAllocationBaseline$' -benchmem -benchtime=1s -count=5 -cpu=1
```

## Correctness and integration

The new parity tests passed on the original implementation before the change and
on the changed implementation. They cover fixed and legacy maps, public and
already-locked getters, missing-field reject details, arbitrary bytes, retained
strings across byte/map reuse, observer nil/empty gating, retained custom MsgTypes,
callback mutation, observer enable/disable/replacement and an initially untimed
persist stage. Existing scoped-message, outbound-observer and heartbeat-store
tests also pass in the focused candidate run.

| Verification | Result |
|---|---|
| [Go 1.27.2 core and local packages](core-go1.27.2.txt) | PASS, core 30.396 s |
| [Go 1.23.12 core and local packages](core-go1.23.12.txt) | PASS, core 30.569 s |
| [Separate Go 1.27.2 core/internal race](race-go1.27.2.txt) | PASS, core 32.646 s |
| [Go 1.23.12 vet](vet-go1.23.12.txt) | PASS, no diagnostics |
| [Exact Go 1.27.2 observed heartbeat totals](exact-go1.27.2.txt) | 100 completed writes; zero Mallocs and TotalAlloc delta |
| [Exact Go 1.23.12 observed heartbeat totals](exact-go1.23.12.txt) | 100 completed writes; zero Mallocs and TotalAlloc delta |

The exact check includes three callback MsgType reads and enabled observation;
it detects an individual allocation that per-operation benchmark rounding might
hide. Like the benchmark, it uses the bounded store and connection fixtures.
Allocation measurements are excluded from race builds; lifetime/correctness
fixtures run with race instrumentation separately.

```sh
GOTOOLCHAIN=auto rtk proxy go test -p=1 . ./internal ./datadictionary ./store/memory ./store/file ./log/composite ./log/file -count=1 -timeout=180s
GOTOOLCHAIN=go1.23.12 rtk proxy go test -p=1 . ./internal ./datadictionary ./store/memory ./store/file ./log/composite ./log/file -count=1 -timeout=180s
GOTOOLCHAIN=auto rtk proxy go test -race -p=1 . ./internal -count=1 -timeout=180s
GOTOOLCHAIN=go1.23.12 rtk proxy go vet . ./internal ./datadictionary
GOTOOLCHAIN=auto rtk proxy go test . -run '^TestHeartbeatObservedStringReadsExactAllocations$' -count=1 -v
GOTOOLCHAIN=go1.23.12 rtk proxy go test . -run '^TestHeartbeatObservedStringReadsExactAllocations$' -count=1 -v
```

This candidate has not yet been deployed or measured in PREPROD. The previously
reported Mongo logger teardown failure is an unrelated external integration
failure; this phase does not change or suppress that logger.
