# Phase 7: recovery allocation reduction and full-session qualification

Before source: `be6ea9527fe03a4359d97c956c25bef2a5d067af` (phase 6).
This phase removes allocations in actual resend and inbound-gap processing and
qualifies the complete supported TCP flow. The public configuration, lifetime,
exhaustion and exclusion contract is in [ZERO_ALLOCATION.md](../../../ZERO_ALLOCATION.md).

## Actual production changes

Resend requests use the fixed outbound builder instead of constructing a legacy
message. The bounded resend call dispatches before the legacy by-value message
escapes into its iterator. Sequence rejects, recovery state and timeout wrappers
live in session-owned cells; legacy sessions preserve their value behavior.
Gap-fill booleans are read directly from bytes with unchanged format rejects.

Numeric recovery events use a fixed 128-byte session buffer and the additive
`BorrowedEventLog` callback. Its capability is resolved once at configuration.
Legacy loggers retain their exact format strings and argument lifetimes.
Golden tests cover every event and signed-integer bounds. No validation,
persistence, resend callbacks or existing raw log callbacks were removed.

## Full-session measurement

The harness creates a real loopback TCP peer and the production session loop,
reader and writer, with the FIX44 dictionary and validation enabled. An order
passes through framing, the input channel, fixed message/group parsing,
validation, borrowed application callbacks, fixed encoding, journal commit and
socket write. The peer independently checks dictionary validity, groups,
BodyLength/checksum and expected replay contents before acknowledging receipt.

The acknowledgment callback runs on the session owner, drains completed output
slots and explicitly retires only acknowledged journal history before publishing
a copied checkpoint. A benchmark operation cannot finish on enqueue or on a
socket write whose output slot has not returned. Observers never read live
session-owned state. The original input's slot has already returned before the
owner processes this acknowledgment.

Warm-up fills and reads all 1,024 journal entries before acknowledging them,
visits every input slot and rotates all 33 output slots. Counters continue beyond
1,024 without resets. Setup, connection/Logon, warm-up and cleanup are outside
timing. The reference capacities and logger/store/handler restrictions are those
in the public contract.

The operation units differ deliberately:

- `ordinary` and `groups`: one order/response, with completion acknowledgments
  amortized over batches of up to 32. Groups contain nested tags 453 and 802.
- `outbound_resend`: an original order/response followed by a ResendRequest and
  its exact-body replay, including PossDup and original sending time.
- `inbound_gap`: two ordered application deliveries after one arrives early,
  a server ResendRequest, retained-frame replay and completed responses.
- `slow_consumer`: one request and 64 persisted responses with 8 KiB payloads.
  The peer waits for the owner to observe the output queue full before reading;
  this is observed backpressure, not an assumed sleep duration.
- `warmed_post_reconnect`: the ordinary grouped flow after disconnect, reconnect
  and another warm-up. Connection/control operations themselves are untimed.

Run before and after, separately from correctness/race/profile commands:

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkAllocationBaseline$' -benchmem -benchtime=1s -count=5 -cpu=1
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkBoundedFullSession$' -benchmem -benchtime=1s -count=5 -cpu=1 -timeout=5m
```

Measured October 15, 2026, Go 1.25.3, darwin/arm64, Apple M4 Pro. All five
allocation samples agree in the [before](full-session-before-go1.25.3.txt) and
[after](full-session-after-go1.25.3.txt) files:

| Full TCP operation | Before B/op | Before allocs/op | After B/op | After allocs/op |
|---|---:|---:|---:|---:|
| Ordinary order/response | 0 | 0 | 0 | 0 |
| Nested groups | 0 | 0 | 0 | 0 |
| Outbound resend cycle | 360 | 6 | 0 | 0 |
| Inbound gap cycle | 1,552 | 29 | 0 | 0 |
| Saturated slow-consumer burst | 0 | 0 | 0 | 0 |
| Warmed traffic after reconnect | 0 | 0 | 0 | 0 |

These results establish an allocation reduction, not a claim of statistically
significant CPU or network-latency improvement. Raw timing/throughput samples are
included without treating different operation units as interchangeable.

The original controlled [before](before-go1.25.3.txt) and
[after](after-go1.25.3.txt) benchmark retain the parent allocation results:
dictionary/group parse 0/0, legacy validation 936 B/5, legacy build 152 B/6,
legacy framed read 160 B/2. Decimal overflow remains 168 B/5 and outside the
guarantee. The unchanged harness blobs are:

| Harness | Git blob |
|---|---|
| `allocation_baseline_test.go` | `80bc8bf1fd51732a053235c53bdd76ab173cdabf` |
| `bounded_full_session_test.go` | `7bbec341d21d107e7f56b6965e3d17741d3b8edc` |
| `bounded_full_session_peer_test.go` | `f7a474e0a5b6bf9a325ad32ddda2a23f2d690938` |

## Exact totals and diagnostic profiles

Rounded per-operation reports can hide sparse allocations, including
`testing.AllocsPerRun`, which uses integer division. The separate
`TestBoundedFullSessionExactAllocations` asserts that both total `Mallocs` and
total `TotalAlloc` deltas equal zero across 100 complete cycles of each scenario,
with warm-up and assertions outside the interval and `GOMAXPROCS=1`.

[Five Go 1.25 runs](exact-allocations-go1.25.3.txt) passed all 30 intervals with
exactly zero allocations and bytes; the [Go 1.23 run](focused-go1.23.12.txt) also
passed all six intervals. The test is excluded from race builds; the same TCP
flow correctness fixtures run with the race detector separately.

The [initial profile](profile-before-source.txt) identifies allocating resend
construction, sequence/state boxing, numeric event arguments and the legacy
resend message copy. Its [top report](profile-before-top.txt) includes dictionary
loading and other untimed setup, because a process allocation profile does not
obey benchmark timer boundaries. Profile timing is diagnostic, not performance
evidence.

The [first exact run](diagnostic-first-exact.txt) (whitespace normalized) also caught one 48-byte
allocation in a 100-cycle gap interval. A three-run diagnostic retry did not
reproduce it. The event capability assertion was subsequently moved out of the
hot path to avoid a lazily allocated runtime assertion cache; this is a potential
source, not a profiler-confirmed attribution of that single allocation. All final
exact intervals passed. The earlier four-scenario measurement is retained as
diagnostic evidence; the controlled comparison uses the final unchanged
six-scenario harness and full journal warm-up.

## Correctness and lifecycle evidence

```sh
GOTOOLCHAIN=go1.23.12+auto rtk proxy go test ./... -skip '^TestMongoLogTestSuite$' -count=1 -timeout=3m
GOTOOLCHAIN=go1.23.12+auto rtk proxy go vet ./...
GOTOOLCHAIN=go1.23.12+auto rtk proxy go test . -run '^TestBounded(FullSessionExactAllocations|Recovery|SequenceEvent)' -count=1 -v -timeout=90s
GOTOOLCHAIN=auto rtk proxy go test -race . -count=1 -timeout=3m
```

[Package tests](tests-go1.23.12.txt), [vet](vet-go1.23.12.txt),
[focused compatibility/ownership tests](focused-go1.23.12.txt) and the
[full core race suite](race.txt) passed. Tests cover:

- Exact wire bodies, dictionary/groups, sequence counters and returned slots.
- Real TCP reset followed by replay from the retained journal, with no reset.
- A gated writer holding all 33 slots, then failing the 34th send without an
  additional journal entry, sender increment or target advancement; reconnect
  replays the entire committed prefix even with `ResetOnDisconnect` enabled.
- A completely full journal rejecting a reconnect Logon and preserving all
  bytes/counters instead of evicting history.
- Repeated timeout/recovery cell reuse, chunk boundaries, gap-fill discards,
  ordered replay and complete slot reclamation on the second peer timeout.
- Legacy event formats/argument lifetime and borrowed event scratch lifetime.

The only repository-wide exclusion is the known optional Mongo logger suite.
With `MONGODB_TEST_CXN` absent its setup skips, but teardown dereferences an
uninitialized logger. It failed before the phase-1 codec change and is documented
[separately](../phase-1/README.md#existing-optional-mongo-logger-test-failure).
Mongo code is unchanged; passing/skipping other package commands does not
establish live Mongo integration coverage.
