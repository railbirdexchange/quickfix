Repeated `SessionID.String()` calls rebuild the same session name during logging,
audit and metrics callbacks. Prepare one immutable string for each registered
full SessionID, publish an immutable lookup snapshot during registration/removal,
and return that retained text without allocating. Unregistered IDs now use one
exactly sized string allocation. The public struct, equality, formatting and
string lifetime remain unchanged.

Ownership:

```text
registry lifecycle owner (existing lock) -> new immutable full-ID/text snapshot
  -> atomic publication -> concurrent String readers -> retainable string
unregister -> replacement snapshot drops ID; retained strings stay valid
mutable frame/message/output ownership and completed-write return are unchanged
```

No new hot-path lock, pool, growth or public API. Snapshot rebuilds cost O(active
sessions) at setup/teardown; removed sessions do not accumulate in the current
snapshot. Calls before registration, after unregister, or with different
canonicalized IDs retain the one-allocation fallback.

Controlled Go 1.27.2, Apple M4 Pro, CPU 1; five 1-second samples, median times.
Setup/warm-up excluded; heartbeat waits for completed write and slot return.

| Operation | Before ns/op | After ns/op | Before B/op / allocs/op | After B/op / allocs/op |
|---|---:|---:|---:|---:|
| Registered short ID | 40.94 | 37.71 | 96 / 2 | 0 / 0 |
| Registered all-fields ID | 108.4 | 46.78 | 288 / 3 | 0 / 0 |
| Registered long ID | 150.2 | 42.97 | 784 / 4 | 0 / 0 |
| Unregistered short ID | 52.18 | 27.94 | 96 / 2 | 32 / 1 |
| Completed heartbeat, zero formatting calls | 1,963 | 1,985 | 0 / 0 | 0 / 0 |
| Completed heartbeat, one formatting call | 2,050 | 1,984 | 112 / 2 | 0 / 0 |
| Completed heartbeat, four formatting calls | 2,273 | 2,124 | 448 / 8 | 0 / 0 |

Four-call heartbeat median improves 6.6%; no-formatting control is 1.1% slower.
This is component evidence, not a deployed service latency claim. All mandatory
`BenchmarkAllocationBaseline` byte/object rows are unchanged. Raw captures,
frozen harness digest, full fallback rows and ownership limits are in
`benchmarks/zero-allocation/phase-7/session-identity/README.md`.

PASS: focused fixtures, all-package Go 1.27.2 tests, vet, separate core/internal
race tests. Fixtures cover every optional separator combination, copied/modified
full IDs, retained strings/snapshots, duplicate/unknown lifecycle operations,
unregister/re-register and concurrent readers. The existing unrelated Mongo
logging teardown failure is excluded by name; store tests remain enabled.

Stacked on PR28 (`perf/missing-field-rejects`, before commit
`39cb1237f05686b36f5631e2ac9f2054cdfc688a`). No deployment is claimed here.
