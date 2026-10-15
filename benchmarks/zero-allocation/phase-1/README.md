# Phase 1: decimal byte parsing

`FIXUDecimal.Read` previously converted its borrowed field bytes to a string
before parsing. It now calls the existing byte parser through
`udecimal.Decimal.UnmarshalText`, removing that copy. The dependency remains
pinned to v1.8.0. Its single error wrapper is unwrapped to preserve the previous
error text, chain and direct sentinel comparisons. Both entry points use the
same underlying parser and clear the receiver on failure. `Scale` is unchanged.

Decimal writing and FIX wire formatting are unchanged.

## Ownership

```mermaid
sequenceDiagram
    participant Caller as Caller / frame owner
    participant Field as Caller-owned FIXUDecimal
    Caller->>Field: Read(borrowed field bytes)
    Field->>Field: Parse bytes into decimal value
    Field-->>Caller: Return value/error; retain no input bytes
    Note over Caller: Input slot may be reused after Read returns
```

The synchronous call leaves ownership with the caller. No channel, queue,
sequence number, store, or retention behavior changes in this phase.

## Controlled measurements

Measured October 15, 2026, with Go 1.25.3, darwin/arm64, Apple M4 Pro. Each
allocation result below was identical in all five samples. The same benchmark
harness ran before and after the production change, with setup and one warm-up
operation outside the timed loop. Performance runs completed before correctness,
race and fuzz checks started.

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkAllocationBaseline$' -benchmem -benchtime=1s -count=5 -cpu=1
```

| Warmed flow | Before B/op | Before allocs/op | After B/op | After allocs/op |
|---|---:|---:|---:|---:|
| Decimal: `-124.3456` | 16 | 1 | 0 | 0 |
| Decimal: `-1234567890123456789.1234567890123456789` | 48 | 1 | 0 | 0 |
| Decimal: maximum uint128 coefficient | 48 | 1 | 0 | 0 |
| Decimal: uint128 overflow (excluded from guarantee) | 216 | 6 | 168 | 5 |
| Parse, reused message, no dictionary | 0 | 0 | 0 | 0 |
| Parse with dictionary | 112 | 7 | 112 | 7 |
| Parse with repeating group | 1,184 | 34 | 1,184 | 34 |
| Validate NewOrderSingle | 1,200 | 23 | 1,200 | 23 |
| Build NewOrderSingle | 376 | 10 | 376 | 10 |
| Framed read | 160 | 2 | 160 | 2 |

Raw output, including timings, is in [before-go1.25.3.txt](before-go1.25.3.txt)
and [after-go1.25.3.txt](after-go1.25.3.txt). Reproduce the before run at commit
`0cff9549c66da2cc8a94f15930134c9828e9501c`, which contains the same harness and
fixtures but the original production codec. Reproduce the after run at this
PR's head. The PR base is `d0993b42089daed35711c252244147d1183fd398`.

These are codec measurements, not a full-session zero-allocation claim. The
three non-overflow decimal fixtures measure zero allocations. Arbitrary-precision
overflow, error construction, unbounded stores, allocating loggers, legacy
convenience APIs and the remaining session phases are outside this result.
Invalid inputs still allocate errors; `UnmarshalText` also constructs a wrapper
before `Read` unwraps it. No allocation reduction is claimed for reject paths.

## Correctness and compatibility

The existing numeric and write fixtures remain in place. Added table tests and
fuzzing compare against the former `udecimal.Parse(string(input))` behavior,
including exact decimal representation, precision, error chains/text, empty and
oversized sentinel errors, signs, trailing zeros, precision limits, uint128
boundaries, arbitrary-precision values, malformed bytes, and reused receivers.
They also verify that input is unmodified and that overwriting it after return
cannot change the value or error. The baseline message/group fixtures remain
unchanged.

| Check | Result | Evidence |
|---|---|---|
| Focused decimal, fuzz seeds and baseline fixtures | PASS before and after | `rtk proxy go test . -run '^(TestFIXUDecimal.*\|TestAllocationBaselineFixtures\|FuzzFIXUDecimalReadCompatibility)$' -count=1` |
| Core QuickFIX suite, Go 1.23.12 | PASS | [tests-go1.23.12.txt](tests-go1.23.12.txt) |
| Core QuickFIX suite, race detector, Go 1.25.3 | PASS | [race.txt](race.txt) |
| Decimal compatibility fuzzing, 15 seconds, 2 workers | PASS; 428,472 executions | [fuzz.txt](fuzz.txt) |
| `rtk proxy go vet ./...`, Go 1.23.12 | PASS | Exit status 0, no diagnostics |
| All packages, Go 1.25.3 | FAIL before and after in optional Mongo logger tests | [tests-before.txt](tests-before.txt), [tests-after.txt](tests-after.txt) |

Verification commands (run separately from performance measurement):

```sh
GOTOOLCHAIN=go1.23.12+auto rtk proxy go test . -count=1
GOTOOLCHAIN=go1.23.12+auto rtk proxy go vet ./...
GOTOOLCHAIN=auto rtk proxy go test -race . -count=1
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -fuzz '^FuzzFIXUDecimalReadCompatibility$' -fuzztime=15s -parallel=2
GOTOOLCHAIN=auto rtk proxy go test ./... -count=1
```

The source and tests remain compatible with Go 1.23; performance numbers above
were measured on Go 1.25.3. An independent source review found no actionable
issues in the codec, compatibility fixtures or benchmark harness.

## Existing optional Mongo logger test failure

The repository-wide command discovers `log/mongo` even though this codec work
requires no MongoDB service. With `MONGODB_TEST_CXN` absent, `SetupTest` calls
`SkipNow`, but `TearDownTest` dereferences the uninitialized `suite.log` and
panics. The same two tests fail before and after the codec change. Other package
commands exit successfully; skipped Mongo integration tests do not establish
Mongo integration coverage. No Mongo code or test behavior is changed here.

## Remaining qualification

Outbound encoding, dictionary/group parsing, validation, frame-slot ownership,
completed outbound writes and bounded resend retention still require their own
allocation-reducing PRs and before/after measurements. Full-session qualification
and configured resource limits are not established by this phase.
