# Static reject events

Known fixed reject reasons already have immutable text. The existing bounded
logging path copies the prefix and reason into reusable event bytes; an enabled
logger with retaining hooks must then copy those borrowed bytes into an owned
string. This phase supplies the complete immutable event string directly through
the existing `OnEvent(string)` callback for a finite set of built-in reasons.

```text
fixed built-in reason -> immutable complete event literal
  -> selected BorrowedEventLog.OnEvent -> logger/hook may retain string

dynamic reason -> existing bounded event scratch -> OnEventBorrowed
  -> logger copies before retaining -> scratch may be reused

no bounded capability or insufficient capacity -> existing OnEventf
  -> existing owned format/argument lifetime
```

The capability and capacity checks stay in place. No error type, equality,
public API, logging level, formatter cache or mutable sharing is introduced.
The fixed set contains the seven configured static validation reasons,
required-tag missing, invalid/unsupported message type, invalid tag number,
CompID problem, sending-time accuracy problem, and group-fields-out-of-order
without its optional dynamic detail. Other reasons retain the existing path.

## Measurement boundary

Before runtime: `e0a5d799fc030c5cdc7f31e2e4149237e4357814` (PR36).
The logging benchmark and parity fixtures are frozen before the runtime edit.
One operation is a synchronous `session.logControlReject` call through a small
logger that retains strings in a fixed four-entry ring. Its borrowed callback
copies the bytes, while its owned callback retains the string. This models the
ownership boundary of a retaining hook; it is not the production Zerolog logger,
completed TCP traffic or a full-session latency measurement.

Setup and 66 warm-ups are outside timing. Fixed reasons, a dynamic reason,
filtered events and a non-opt-in legacy logger are measured separately, with
five one-second samples and CPU1. The benchmark checks delivered text and that
filtered events never reach the retaining hook.

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkStaticRejectEvent$' -benchmem -benchtime=1s -count=5 -cpu=1
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkAllocationBaseline$' -benchmem -benchtime=1s -count=5 -cpu=1
```

The frozen parity fixtures cover exact text, Unicode/invalid UTF-8, retained
strings after scratch reuse and multiple ring rotations, near-match dynamic
text, exact/undersized/empty capacity, missing capability and legacy retained
format arguments. Post-change fixtures additionally require owned dispatch for
all fixed reasons and exactly zero added `Mallocs`/`TotalAlloc` over 1,000
retained events per reason. No zero-allocation claim applies to arbitrary
diagnostic text or allocating logger implementations.

## Results

Go 1.27.2, darwin/arm64, Apple M4 Pro. The following are medians of the five
samples in [before](event-before.txt) and [after](event-after.txt):

| Retaining logger component | Before ns/op | After ns/op | Before B/op; allocs/op | After B/op; allocs/op |
|---|---:|---:|---:|---:|
| Required tag missing | 14.39 | 3.089 | 48; 1 | 0; 0 |
| Invalid enum | 15.81 | 4.644 | 64; 1 | 0; 0 |
| Invalid format | 15.33 | 3.284 | 64; 1 | 0; 0 |
| Dynamic reason | 15.78 | 16.55 | 64; 1 | 64; 1 |
| Filtered fixed reason | 3.401 | 4.498 | 0; 0 | 0; 0 |
| Non-opt-in legacy logger | 65.36 | 65.39 | 96; 3 | 96; 3 |

The finite lookup adds 0.77 ns to the dynamic control and 1.097 ns to the
filtered control in these samples. Their allocations are unchanged. The fixed
enabled rows remove the actual owned-string copy without dropping the event.
There is no setup-owned lookup table or additional per-session storage.

[Source hashes before](source-before.sha256) and
[after](source-after.sha256) show that the benchmark and parity fixtures are
byte-identical across measurement, including formatting. The after-only dispatch
and exact-allocation tests are also hashed. [Toolchain](toolchain.txt) and
[base commit](before-source.txt) are recorded separately.

The mandatory allocation baseline also preserves every allocation count.
These unaffected controls are included to detect regressions, not attributed
speedups. Raw [before](baseline-before.txt) and [after](baseline-after.txt)
outputs contain all five samples:

| Baseline flow | Before median ns/op | After median ns/op | Before and after B/op; allocs/op |
|---|---:|---:|---:|
| UDecimal short | 11.07 | 10.82 | 0; 0 |
| UDecimal signed fraction | 27.28 | 27.10 | 0; 0 |
| UDecimal max uint128 | 57.27 | 56.31 | 0; 0 |
| UDecimal overflow | 311.9 | 314.5 | 168; 5 |
| Reused parse without dictionary | 366.6 | 371.9 | 0; 0 |
| Reused parse with dictionary | 431.1 | 431.1 | 0; 0 |
| Reused parse with group | 794.0 | 797.3 | 0; 0 |
| Valid NewOrderSingle validation | 672.7 | 678.4 | 0; 0 |
| Legacy NewOrderSingle build | 760.2 | 756.0 | 144; 5 |
| Legacy framed read | 112.4 | 112.8 | 160; 2 |

## Correctness and checks

The frozen parity suite passes [before](focused-before.txt) and
[after](focused-after.txt). The after output also records the new dispatch and
exact-allocation checks: all 14 reasons deliver 1,000 retained events with zero
added allocations and bytes over each complete interval.

Commands run serially with `GOTOOLCHAIN=auto`, separately from performance:

```sh
rtk proxy go test . -run '^TestStaticRejectEvent' -count=1 -v
rtk proxy go test -p=1 ./... -skip '^TestMongoLogTestSuite$' -count=1 -timeout=5m
rtk proxy go test -race -p=1 . ./internal -count=1 -timeout=5m
rtk proxy go vet ./...
```

The all-package run [passes](packages-after.txt), including the core suite
(38.055 s). The separate [race run](race-after.txt) passes for core (47.129 s)
and internal (2.617 s). Vet exits 0; its [raw output](vet-after.txt) is empty.
Only the unrelated `TestMongoLogTestSuite` teardown failure is excluded. Its
[original before-change evidence](../../phase-1/tests-before.txt)
and [diagnosis](../../phase-1/README.md#existing-optional-mongo-logger-test-failure)
are retained separately; no live Mongo verification is claimed here.

No deployment, service change or infrastructure change is part of this revision.

## Recovery qualification regression

`TestScopedLogoutTimeoutBeforeCompletedRequestWrite` gates the actual outbound
writer, verifies the Logout response timer is armed, and explicitly delivers the
normal timeout event while the request write remains blocked. It observes the
timeout log and `OnLogout` without any incoming admin response. Teardown waits
for the accepted write, then returns all inbound/outbound slots and closes the
socket. The hour-long test timer avoids a scheduling race; no runtime timeout
setting or state-machine behavior changes.

Together with the existing graceful Logout drain fixture, this test passed
20 repetitions (0.772 seconds) and a separate 10-repetition race run
(1.909 seconds) on 2026-10-20:

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^TestScoped(LogoutTimeoutBeforeCompletedRequestWrite|OutboundGracefulLogoutCompletesBeforeSocketClose)$' -count=20
GOTOOLCHAIN=auto rtk proxy go test . -race -run '^TestScoped(LogoutTimeoutBeforeCompletedRequestWrite|OutboundGracefulLogoutCompletesBeforeSocketClose)$' -count=10
```

This establishes the local timeout/write lifecycle, not the cause of the
PREPROD client's missing Logout reply. The allocation measurements above remain
the unchanged runtime comparison.
