# Return missing routing-header errors

Calling `MessageRouter.Route` with tag 8 (`BeginString`) absent previously
discarded the header getter's error and returned `nil`. It now returns that
existing error, just as it already did for a missing tag 35 (`MsgType`). A valid
caller-supplied session ID cannot hide a missing message header, and no handler
is invoked for either missing routing header.

This is one changed return statement. It adds no lookup, validation pass, queue,
copy or ownership transition. Normal wire processing validates the header before
the callback; this correction protects callers using the router directly too.
Unmatched admin messages and BusinessMessageReject (`35=j`) retain their existing
allowance after routing headers and any required FIXT version are resolved.

The prepared dictionary error is immutable and can outlive the message slot.
The new test verifies the exact existing error is returned, repeated calls
allocate nothing, and resetting the message does not invalidate the error.
The ordinary allocating getter retains its existing allocation behavior.

## Paired router measurements

Measured October 21, 2026, Apple M4 Pro, darwin/arm64, Go 1.27.2, CPU 1.
Before runtime: `b830f01c67b42e0f8ec2aff7b9c118558b97e594`. After runtime:
the one-line `Route` error-propagation correction accompanying this evidence.
Both runs use the identical benchmark fixture, with setup and warm-up outside
timing. No task-owned builds, tests or profiles ran concurrently with measurement.
Rows are medians of five one-second samples.

| Fixture | Before ns/op | After ns/op | Before B/op, allocs/op | After B/op, allocs/op |
|---|---:|---:|---:|---:|
| FIX 4.4 route | 43.31 | 42.89 | 0, 0 | 0, 0 |
| FIXT explicit version | 56.53 | 56.94 | 0, 0 | 0, 0 |
| FIXT default version | 100.4 | 101.6 | 0, 0 | 0, 0 |
| FIX 4.4 multibyte message type | 37.20 | 37.53 | 0, 0 | 0, 0 |
| FIXT custom long keys | 50.60 | 51.48 | 0, 0 | 0, 0 |
| FIXT custom default version | 99.42 | 100.0 | 0, 0 | 0, 0 |
| Missing BeginString, ordinary getter | 33.14 | 34.28 | 72, 2 | 72, 2 |
| Missing BeginString, prepared getter | 12.82 | 12.95 | 0, 0 | 0, 0 |

Valid-route medians changed between -1.0% and +1.7%, with a largest absolute
change of 1.2 ns. These samples do not establish exact timing equivalence or a
speedup. They establish unchanged allocation counts, including zero allocations
when returning the prepared error. The ordinary error-path median increased
1.14 ns (3.4%) while returning the previously discarded error to the caller.
This is a router component measurement, not deployed FIX-server latency.

Raw samples: [before](before.txt), [after](after.txt).

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkMessageRouter(Allocation|MissingBeginString)$' -benchmem -benchtime=1s -count=5 -cpu=1
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkAllocationBaseline$' -benchmem -benchtime=1s -count=5 -cpu=1
```

For the standard allocation baseline, the before result is the already completed
run on the same parent runtime (`b830f01c`), retained in
[session-registry/allocation-after.txt](../session-registry/allocation-after.txt).
It was not rerun for this correction. The candidate result is
[allocation-after.txt](allocation-after.txt); every B/op and allocs/op row is
unchanged across all five samples.

## Correctness evidence

The new regression tests [fail on the parent](regression-before.txt) because
missing tag 8 is reported as success. The complete router tests
[pass after the correction](tests-focused.txt), including existing admin/business
reject, FIXT resolution, callback argument/return, retained-message and concurrent
immutable-router coverage. Two existing expectations that encoded the silent
success were updated to require the getter's missing-tag-8 error.

| Check | Result | Evidence |
|---|---|---|
| `go test . -run '^TestMessageRouter' -count=1 -v` | PASS; 0.834 s | [Focused tests](tests-focused.txt) |
| `go test ./... -skip '^TestMongoLogTestSuite$' -count=1` | PASS; core 37.863 s | [Package results](tests.txt) |
| `go test -race . -run '^TestMessageRouter' -count=1` | PASS; 1.787 s | [Router race result](race.txt) |
| `go vet ./...` | PASS; exit 0, no diagnostics | [Output](vet.txt) |

The package run includes all seven existing bounded full-session exact-allocation
tests, each requiring zero new heap objects and bytes across 100 completed cycles.
Race instrumentation is measured separately. The full core race suite passed on
the preceding registry implementation; this follow-up reruns the router race tests.

Mongo integration remains NOT RUN: its external fixture is unavailable, and the
known logger teardown failure is documented in
[separate historical evidence](../../dynamic-retirement/mongo-environment-failure.txt).
Only that logger suite is explicitly excluded; the Mongo store suite skips itself
without its connection setting. No PREPROD deployment or service load test is part
of this correction.

This correctness follow-up is part of the
[session registry PR](../session-registry/README.md), which contains the measured
registration/removal allocation reduction. It introduces no dependency or public
API signature change.
