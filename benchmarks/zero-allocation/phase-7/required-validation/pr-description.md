Missing required fields currently allocate a new immutable rejection for every
message. Prepare those rejections once from each session's finite dictionary
tag union. The original dictionary traversal still chooses which missing tag
to reject, preserving reject precedence and all wire behavior.

Ownership:

```text
session setup -> immutable required-tag metadata and owned error values
message owner -> dictionary validation -> selected immutable error
  -> reject control -> output slot -> writer channel
  -> completed write -> returned slot
retained error remains valid across message reuse and validator detachment
```

No new hot-path lock, pool, growth or public API. The private session validator
uses the prepared values automatically. Public validators, early missing-MsgType
checks, recursive group checks and unknown tags keep their constructor fallback.

Go 1.27.2, Apple M4 Pro, CPU 1, five one-second samples; median times. Setup and
66 warm-ups excluded. Both revisions use the identical frozen benchmark harness.

| Operation | Before ns/op | After ns/op | Before B/op / allocs/op | After B/op / allocs/op |
|---|---:|---:|---:|---:|
| Valid session validation | 647.1 | 633.4 | 0 / 0 | 0 / 0 |
| Missing Side validation, two error text reads | 209.8 | 184.4 | 64 / 1 | 0 / 0 |
| Validation through completed reject write | 3,711 | 3,525 | 64 / 1 | 0 / 0 |

Completed reject median improves 5.0%, including validation, callback, bounded
persistence, encoding, completed writer copy, slot return and peer parse. This
is component evidence, not TCP or deployed service latency. Every mandatory
`BenchmarkAllocationBaseline` byte/object row is unchanged. Raw before/after
captures, harness hash and correctness receipts are in
`benchmarks/zero-allocation/phase-7/required-validation/README.md`.

PASS: exact counters for 1,000 validations plus 2,000 error text reads and
100 completed FIX44 reject writes, all at zero allocated bytes and objects;
focused correctness; all-package tests; vet; separate core/internal race tests.
Fixtures cover section precedence, FIXT dictionary selection, finite cache and
fallback behavior, retained immutable errors, concurrent error readers, and
FIX40/FIX44 completed reject bytes and sequence behavior. FIXT exact allocation
counts are not separately measured. The existing unrelated Mongo logging
teardown failure is excluded by name; store tests remain enabled.

Stacked on PR29 (`perf/session-identity-formatting`), before commit
`2568ec7288d573ba92ea0c353267612e2034075e`. No deployment is claimed here.
