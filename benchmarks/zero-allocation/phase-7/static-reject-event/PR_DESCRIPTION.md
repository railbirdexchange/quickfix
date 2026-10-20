Enabled retaining loggers copied every bounded reject event from mutable bytes
into an owned string, including fixed diagnostics such as `Required tag missing`.
For the existing `BorrowedEventLog` opt-in, supply complete immutable event
literals through its existing `OnEvent(string)` callback for 14 fixed reasons.
Keep the capacity guard, dynamic borrowed path and non-opt-in `OnEventf` path.
Public errors, APIs and event text are unchanged.

```text
fixed reason -> immutable complete event literal -> selected OnEvent
  -> logger/hook may retain the string indefinitely
dynamic reason -> session-owned event scratch -> selected OnEventBorrowed
  -> logger copies before retaining -> session may reuse scratch
no bounded capability / insufficient capacity -> existing OnEventf
```

Matched Go 1.27.2, CPU1, five one-second samples; setup and 66 warm-ups excluded.
One operation is a synchronous reject event through a small logger retaining
strings in a fixed ring. These are logger-component measurements, not production
Zerolog, completed TCP or full-session latency results.

| Event | Before median ns/op; B/op; allocs/op | After median ns/op; B/op; allocs/op |
|---|---:|---:|
| Required tag missing | 14.39; 48; 1 | 3.089; 0; 0 |
| Invalid enum | 15.81; 64; 1 | 4.644; 0; 0 |
| Invalid format | 15.33; 64; 1 | 3.284; 0; 0 |
| Dynamic control | 15.78; 64; 1 | 16.55; 64; 1 |
| Filtered control | 3.401; 0; 0 | 4.498; 0; 0 |
| Legacy control | 65.36; 96; 3 | 65.39; 96; 3 |

The lookup adds 0.77 ns to the dynamic control and 1.097 ns to the filtered
control in these samples; their allocations are unchanged. No mutable cache or
per-session storage is added. Arbitrary diagnostic text and logger internals
remain outside the zero-allocation result.

The same parity fixtures pass before and after, including retained text after
scratch reuse, empty/dynamic/Unicode/invalid-UTF-8 text, exact/undersized capacity,
missing capability and legacy retained format arguments. After-only checks
require the selected owned callback and exactly zero added `Mallocs`/`TotalAlloc`
over 1,000 retained events for each of the 14 fixed reasons. The frozen benchmark
and parity fixture hashes are identical before and after.

The mandatory allocation baseline preserves every allocation count. All-package
tests pass (core 38.055 s), separate core/internal race tests pass
(47.129 s/2.617 s), and vet passes. Only the pre-existing
`TestMongoLogTestSuite` teardown failure is excluded; its
[original evidence](benchmarks/zero-allocation/phase-1/tests-before.txt) and
[diagnosis](benchmarks/zero-allocation/phase-1/README.md#existing-optional-mongo-logger-test-failure)
remain separate. No deployment is included.

Full measurements, commands, source hashes and raw output:
[phase evidence](benchmarks/zero-allocation/phase-7/static-reject-event/README.md).
